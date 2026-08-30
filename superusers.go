package gocommerce

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A superuser is an operator who signs in to the admin panel with an email
// and a password, the way PocketBase's superusers do.
//
// This sits *beside* Config.AdminTokens rather than replacing them. A token is
// the right credential for a script — it has no session, no expiry and no
// human behind it — and a password is the right credential for a person. Both
// arrive as "Authorization: Bearer <x>" and both satisfy the same middleware,
// so no handler has to care which one it got.
//
// The engine still has no notion of a *customer*; this is an operator table,
// and D22 (guest checkout is permanent) is untouched by it.

const (
	// PBKDF2-HMAC-SHA256 at OWASP's 2023 floor. Chosen over bcrypt because it
	// is in the standard library as of Go 1.24, and the whole engine's claim to
	// slimness is that it has one production dependency.
	pbkdf2SaltBytes = 16
	pbkdf2KeyBytes  = 32
	pbkdf2Scheme    = "pbkdf2-sha256"

	sessionTokenBytes = 32
	// How long a panel session lasts. Matches PocketBase's default superuser
	// token duration.
	sessionTTL = 14 * 24 * time.Hour

	// MinPasswordLength is the shortest password a superuser may set. It
	// matches PocketBase's minimum.
	MinPasswordLength = 8
)

// pbkdf2Iterations is a var, not a const, so tests can wind it down. Nothing
// outside this package can reach it, and the serialized hash records the
// iteration count it was made with, so lowering it never invalidates a hash
// that already exists.
var pbkdf2Iterations = 600_000

// Superuser is an admin-panel operator. The password hash is never
// serialized: the struct is returned directly from HTTP handlers, so the
// absence of a json tag here is load-bearing.
type Superuser struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	// Role decides what they may do — see rights.go. Every operator that
	// existed before roles did is an owner, which is what they were.
	Role string `json:"role"`
	// Rights is the role spelled out, so the panel can hide what it cannot do
	// without keeping its own copy of the table.
	Rights    []Right   `json:"rights"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	passwordHash string
}

// Superusers is the operator-identity service.
type Superusers struct {
	db       *sql.DB
	throttle *loginThrottle
}

func newSuperusers(db *sql.DB) *Superusers {
	return &Superusers{db: db, throttle: newLoginThrottle()}
}

// ---------------------------------------------------------------- passwords

// hashPassword returns a self-describing PBKDF2 hash:
//
//	pbkdf2-sha256$<iterations>$<salt>$<key>
//
// Encoding the parameters means the cost can be raised later without a
// migration: an old hash still verifies against its own recorded iteration
// count.
func hashPassword(password string) (string, error) {
	salt := make([]byte, pbkdf2SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyBytes)
	if err != nil {
		return "", fmt.Errorf("derive password key: %w", err)
	}
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("%s$%d$%s$%s", pbkdf2Scheme, pbkdf2Iterations,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// verifyPassword reports whether password matches the encoded hash. The
// comparison is constant time; a malformed hash is a mismatch rather than an
// error, so a corrupted row cannot become an authentication bypass.
func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != pbkdf2Scheme {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := b64.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyVerify burns the same time a real verification would. Authenticate
// calls it when the identity does not exist, so that "no such account" and
// "wrong password" take the same time and the endpoint cannot be used to
// enumerate operators.
//
// The cost is taken from a hash that actually exists rather than from
// pbkdf2Iterations, because those two numbers diverge the moment the cost is
// raised: stored hashes keep verifying at the count they were made with, so a
// dummy pinned to the new, dearer count would make "no such account"
// measurably slower than "wrong password" and reopen the very oracle this
// closes. Falling back to the global is safe — it only happens when there are
// no superusers at all, and then there is nothing to enumerate.
func (s *Superusers) dummyVerify(ctx context.Context, password string) {
	var encoded string
	if err := s.db.QueryRowContext(ctx,
		`SELECT password_hash FROM superusers LIMIT 1`).Scan(&encoded); err == nil {
		verifyPassword(encoded, password)
		return
	}
	salt := make([]byte, pbkdf2SaltBytes)
	_, _ = pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyBytes)
}

// --------------------------------------------------------------- management

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func validateCredentials(email, password string) error {
	email = normalizeEmail(email)
	if email == "" {
		return Validationf("email is required")
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return Validationf("%q is not a valid email address", email)
	}
	if len([]rune(password)) < MinPasswordLength {
		return Validationf("password must be at least %d characters", MinPasswordLength)
	}
	return nil
}

const superuserColumns = `id, email, password_hash, role, created_at, updated_at`

func scanSuperuser(row interface{ Scan(...any) error }) (*Superuser, error) {
	var s Superuser
	if err := row.Scan(&s.ID, &s.Email, &s.passwordHash, &s.Role, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	// Spelled out on the way out rather than stored: the role is the fact, the
	// rights are what it currently means, and only one of those belongs in a
	// row that outlives this version of the table.
	s.Rights = RightsOf(s.Role)
	return &s, nil
}

// Create adds a superuser in a role. An empty role means owner, which is what
// every operator was before roles existed and what the first one has to be.
func (s *Superusers) Create(ctx context.Context, email, password, role string) (*Superuser, error) {
	if err := validateCredentials(email, password); err != nil {
		return nil, err
	}
	if role == "" {
		role = RoleOwner
	}
	if !ValidRole(role) {
		return nil, Validationf("%q is not a role; the roles are %s", role, strings.Join(Roles, ", "))
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, Internalf(err, "hash password")
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO superusers (email, password_hash, role) VALUES ($1, $2, $3)
		RETURNING `+superuserColumns, normalizeEmail(email), hash, role)
	su, err := scanSuperuser(row)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, Conflictf("a superuser with email %q already exists", normalizeEmail(email))
		}
		return nil, Internalf(err, "create superuser")
	}
	return su, nil
}

// List returns every superuser, oldest first.
func (s *Superusers) List(ctx context.Context) ([]*Superuser, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+superuserColumns+` FROM superusers ORDER BY id`)
	if err != nil {
		return nil, Internalf(err, "list superusers")
	}
	defer rows.Close()

	out := []*Superuser{}
	for rows.Next() {
		su, err := scanSuperuser(rows)
		if err != nil {
			return nil, Internalf(err, "scan superuser")
		}
		out = append(out, su)
	}
	if err := rows.Err(); err != nil {
		return nil, Internalf(err, "list superusers")
	}
	return out, nil
}

// Update changes a superuser's email, password, or both. Empty values are
// left alone.
//
// Changing the password invalidates *every* session that operator holds,
// including the one making the request — a password change is how someone
// responds to a compromise, and a session that survived it would defeat the
// point. The caller is expected to hand the operator a fresh session
// afterwards if they were changing their own password; see
// handleUpdateSuperuser.
func (s *Superusers) Update(ctx context.Context, id int64, email, password string) (*Superuser, error) {
	var (
		sets []string
		args []any
	)
	if email = normalizeEmail(email); email != "" {
		if _, err := mail.ParseAddress(email); err != nil {
			return nil, Validationf("%q is not a valid email address", email)
		}
		args = append(args, email)
		sets = append(sets, fmt.Sprintf("email = $%d", len(args)))
	}
	if password != "" {
		if len([]rune(password)) < MinPasswordLength {
			return nil, Validationf("password must be at least %d characters", MinPasswordLength)
		}
		hash, err := hashPassword(password)
		if err != nil {
			return nil, Internalf(err, "hash password")
		}
		args = append(args, hash)
		sets = append(sets, fmt.Sprintf("password_hash = $%d", len(args)))
	}
	if len(sets) == 0 {
		return nil, Validationf("nothing to update: supply an email, a password, or both")
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, id)

	var su *Superuser
	err := InTx(ctx, s.db, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `UPDATE superusers SET `+strings.Join(sets, ", ")+
			fmt.Sprintf(" WHERE id = $%d RETURNING ", len(args))+superuserColumns, args...)
		var err error
		if su, err = scanSuperuser(row); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return NotFoundf("superuser %d not found", id)
			}
			if isUniqueViolation(err) {
				return Conflictf("a superuser with email %q already exists", email)
			}
			return Internalf(err, "update superuser")
		}
		if password != "" {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM superuser_sessions WHERE superuser_id = $1`, id); err != nil {
				return Internalf(err, "revoke sessions")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return su, nil
}

// Delete removes a superuser. The last one cannot be deleted: an admin panel
// nobody can sign in to is not a state an operator can click their way out of.
func (s *Superusers) Delete(ctx context.Context, id int64) error {
	return InTx(ctx, s.db, func(tx *sql.Tx) error {
		// Every row is locked before it is counted. Read Committed alone would
		// let two concurrent deletes of the last two operators each see a
		// count of two, each pass the guard, and each commit — leaving a panel
		// nobody can sign in to, which is precisely what the guard exists to
		// prevent. Counting under FOR UPDATE serialises the pair.
		rows, err := tx.QueryContext(ctx, `SELECT id FROM superusers ORDER BY id FOR UPDATE`)
		if err != nil {
			return Internalf(err, "lock superusers")
		}
		var ids []int64
		for rows.Next() {
			var got int64
			if err := rows.Scan(&got); err != nil {
				rows.Close()
				return Internalf(err, "scan superuser id")
			}
			ids = append(ids, got)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return Internalf(err, "lock superusers")
		}

		// Existence is checked before the last-one guard, so deleting an id
		// that was never there says so, instead of blaming the count.
		if !slices.Contains(ids, id) {
			return NotFoundf("superuser %d not found", id)
		}
		if len(ids) <= 1 {
			return Conflictf("cannot delete the last superuser")
		}

		// The same lockout SetRole refuses, reached through a different door.
		// Removing the only owner leaves a team where nobody carries
		// settings.write, and settings.write is the only right that can hand
		// out roles — so the survivors cannot promote anybody, including
		// themselves, and the way back in is a database client. Deleting is not
		// gentler than demoting; it is the same door.
		//
		// The rows are already locked by the SELECT above, so counting here is
		// serialised against a concurrent delete of the other owner.
		var role string
		if err := tx.QueryRowContext(ctx,
			`SELECT role FROM superusers WHERE id = $1`, id).Scan(&role); err != nil {
			return Internalf(err, "read superuser")
		}
		if role == RoleOwner {
			var owners int
			if err := tx.QueryRowContext(ctx,
				`SELECT count(*) FROM superusers WHERE role = $1`, RoleOwner).Scan(&owners); err != nil {
				return Internalf(err, "count owners")
			}
			if owners <= 1 {
				return Conflictf(
					"this is the only owner; make somebody else an owner before removing this one")
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM superusers WHERE id = $1`, id); err != nil {
			return Internalf(err, "delete superuser")
		}
		return nil
	})
}

// UpdateSelf changes an operator's own email or password.
//
// It exists because the alternative is that changing your own password needs
// settings.write — the right that can also change everybody else's role. A
// staff member who suspects their password is known would otherwise have to ask
// an owner to choose a new one for them, which is the practice invitations
// exist to end.
//
// The current password is required for either change. A session left open on an
// unlocked laptop is the threat: it should not be enough to take the account
// over, and re-typing the password is what separates "this browser is signed
// in" from "this is the person".
//
// keepToken is the caller's own session, which survives a password change while
// every other one ends. Signing somebody out of the browser they are currently
// typing in — as a reward for improving their password — teaches them not to.
func (s *Superusers) UpdateSelf(ctx context.Context, id int64, currentPassword, email, password, keepToken string) (*Superuser, error) {
	var (
		sets []string
		args []any
	)
	if email = normalizeEmail(email); email != "" {
		if _, err := mail.ParseAddress(email); err != nil {
			return nil, Validationf("%q is not a valid email address", email)
		}
		args = append(args, email)
		sets = append(sets, fmt.Sprintf("email = $%d", len(args)))
	}
	if password != "" {
		if len([]rune(password)) < MinPasswordLength {
			return nil, Validationf("password must be at least %d characters", MinPasswordLength)
		}
		hash, err := hashPassword(password)
		if err != nil {
			return nil, Internalf(err, "hash password")
		}
		args = append(args, hash)
		sets = append(sets, fmt.Sprintf("password_hash = $%d", len(args)))
	}
	if len(sets) == 0 {
		return nil, Validationf("nothing to update: supply an email, a password, or both")
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, id)

	var su *Superuser
	err := InTx(ctx, s.db, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT `+superuserColumns+` FROM superusers WHERE id = $1 FOR UPDATE`, id)
		current, err := scanSuperuser(row)
		if errors.Is(err, sql.ErrNoRows) {
			return NotFoundf("superuser %d not found", id)
		}
		if err != nil {
			return Internalf(err, "read superuser")
		}
		if !verifyPassword(current.passwordHash, currentPassword) {
			// Deliberately not the login form's "invalid credentials": the
			// operator is already identified, so there is nothing to enumerate
			// and the specific message is the useful one.
			return Forbiddenf("your current password is not correct")
		}

		row = tx.QueryRowContext(ctx, `UPDATE superusers SET `+strings.Join(sets, ", ")+
			fmt.Sprintf(" WHERE id = $%d RETURNING ", len(args))+superuserColumns, args...)
		if su, err = scanSuperuser(row); err != nil {
			if isUniqueViolation(err) {
				return Conflictf("a superuser with email %q already exists", email)
			}
			return Internalf(err, "update superuser")
		}
		if password != "" {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM superuser_sessions WHERE superuser_id = $1 AND token_hash <> $2`,
				id, hashToken(keepToken)); err != nil {
				return Internalf(err, "revoke sessions")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return su, nil
}

// RevokeAll ends every session an operator has, everywhere.
//
// Two uses, one mechanism: an operator signing themselves out of a device they
// no longer have, and an owner cutting somebody off before their account is
// removed or their role reduced. It returns how many sessions ended, because
// "0" and "4" mean quite different things to somebody who has lost a laptop.
func (s *Superusers) RevokeAll(ctx context.Context, id int64) (int, error) {
	// Checked first, because "0 sessions ended" and "there is no such operator"
	// look identical from a delete that matched nothing, and only one of them
	// means the caller should go and look at what they clicked.
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT true FROM superusers WHERE id = $1`, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, NotFoundf("superuser %d not found", id)
	}
	if err != nil {
		return 0, Internalf(err, "read superuser")
	}

	res, err := s.db.ExecContext(ctx,
		`DELETE FROM superuser_sessions WHERE superuser_id = $1`, id)
	if err != nil {
		return 0, Internalf(err, "revoke sessions")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, Internalf(err, "revoke sessions")
	}
	return int(n), nil
}

// Sessions reports how many live sessions an operator has and when the most
// recent one started — enough to say "signed in on 3 devices" without keeping
// user agents and IP addresses that nobody agreed to store.
func (s *Superusers) Sessions(ctx context.Context, id int64) (int, *time.Time, error) {
	var n int
	var newest sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*), max(created_at) FROM superuser_sessions
		WHERE superuser_id = $1 AND expires_at > now()`, id).Scan(&n, &newest)
	if err != nil {
		return 0, nil, Internalf(err, "count sessions")
	}
	if newest.Valid {
		return n, &newest.Time, nil
	}
	return n, nil, nil
}

// Count reports how many superusers exist. The panel asks before showing a
// login form, so that a fresh install can offer to create the first operator
// instead of demanding credentials that do not exist yet.
func (s *Superusers) Count(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM superusers`).Scan(&n); err != nil {
		return 0, Internalf(err, "count superusers")
	}
	return n, nil
}

// Superusers exposes the operator-identity service, so a host program can
// create the first operator at boot without going through HTTP.
func (a *App) Superusers() *Superusers { return a.superusers }

// Bootstrap creates the first superuser, and only the first. It is what the
// `superuser create` command and GOCOMMERCE_ADMIN_EMAIL both call, so an
// unattended deploy converges on exactly one operator however it is run.
func (s *Superusers) Bootstrap(ctx context.Context, email, password string) (*Superuser, bool, error) {
	n, err := s.Count(ctx)
	if err != nil {
		return nil, false, err
	}
	if n > 0 {
		return nil, false, nil
	}
	// The first operator is an owner by necessity: there is nobody else to
	// grant them anything.
	su, err := s.Create(ctx, email, password, RoleOwner)
	if err != nil {
		return nil, false, err
	}
	return su, true, nil
}

// ---------------------------------------------------------------- sessions

// Session is a signed-in superuser's credential.
type Session struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}

func newSessionToken() (string, error) {
	buf := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Authenticate verifies an identity and password and issues a session.
//
// Every failure returns the same error. Distinguishing "no such operator"
// from "wrong password" would turn the login form into an account-enumeration
// oracle, and the constant-time-ish path through dummyVerify keeps the timing
// from saying what the message refuses to.
func (s *Superusers) Authenticate(ctx context.Context, identity, password, clientIP string) (*Superuser, *Session, error) {
	identity = normalizeEmail(identity)

	if retryAfter, ok := s.throttle.blocked(identity, clientIP); ok {
		return nil, nil, (&APIError{
			Status:  http.StatusTooManyRequests,
			Code:    "too_many_attempts",
			Message: fmt.Sprintf("too many failed attempts; try again in %s", retryAfter.Round(time.Second)),
		})
	}

	invalid := &APIError{
		Status:  http.StatusBadRequest,
		Code:    "invalid_credentials",
		Message: "invalid login credentials",
	}

	row := s.db.QueryRowContext(ctx,
		`SELECT `+superuserColumns+` FROM superusers WHERE email = $1`, identity)
	su, err := scanSuperuser(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.dummyVerify(ctx, password)
			s.throttle.fail(identity, clientIP)
			return nil, nil, invalid
		}
		return nil, nil, Internalf(err, "look up superuser")
	}
	if !verifyPassword(su.passwordHash, password) {
		s.throttle.fail(identity, clientIP)
		return nil, nil, invalid
	}
	s.throttle.succeed(identity, clientIP)

	sess, err := s.issue(ctx, su.ID)
	if err != nil {
		return nil, nil, err
	}
	return su, sess, nil
}

func (s *Superusers) issue(ctx context.Context, superuserID int64) (*Session, error) {
	token, err := newSessionToken()
	if err != nil {
		return nil, Internalf(err, "issue session")
	}
	expires := time.Now().Add(sessionTTL)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO superuser_sessions (token_hash, superuser_id, expires_at)
		VALUES ($1, $2, $3)`, hashToken(token), superuserID, expires); err != nil {
		return nil, Internalf(err, "store session")
	}
	// Opportunistic sweep. A dedicated janitor for a table this small would be
	// more machinery than the problem deserves.
	_, _ = s.db.ExecContext(ctx, `DELETE FROM superuser_sessions WHERE expires_at < now()`)
	return &Session{Token: token, ExpiresAt: expires}, nil
}

// Resolve returns the superuser a session token belongs to, or nil if the
// token is unknown or expired.
func (s *Superusers) Resolve(ctx context.Context, token string) (*Superuser, bool) {
	if token == "" {
		return nil, false
	}
	// The list is spelled out because of the join, and has to stay in step with
	// superuserColumns: scanSuperuser reads a fixed shape, and a list that
	// drifts from it surfaces as "unknown token" rather than as an error.
	row := s.db.QueryRowContext(ctx, `
		SELECT s.id, s.email, s.password_hash, s.role, s.created_at, s.updated_at
		FROM superuser_sessions ss
		JOIN superusers s ON s.id = ss.superuser_id
		WHERE ss.token_hash = $1 AND ss.expires_at > now()`, hashToken(token))
	su, err := scanSuperuser(row)
	if err != nil {
		return nil, false
	}
	return su, true
}

// Touch extends a session's life and returns whose it is.
//
// This is what "refresh" means here, and it deliberately does *not* rotate the
// token. Issuing a new token and revoking the old one would mean every page
// load invalidates the credential any other open tab is holding — and, within
// a single tab, a request already in flight when the swap lands comes back 401.
// A server-side session with an expiry gets its value from the expiry, not from
// churning the secret.
func (s *Superusers) Touch(ctx context.Context, token string) (*Superuser, *Session, bool) {
	su, ok := s.Resolve(ctx, token)
	if !ok {
		return nil, nil, false
	}
	expires := time.Now().Add(sessionTTL)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE superuser_sessions SET expires_at = $1 WHERE token_hash = $2`,
		expires, hashToken(token)); err != nil {
		return nil, nil, false
	}
	return su, &Session{Token: token, ExpiresAt: expires}, true
}

// Revoke ends one session.
func (s *Superusers) Revoke(ctx context.Context, token string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM superuser_sessions WHERE token_hash = $1`, hashToken(token)); err != nil {
		return Internalf(err, "revoke session")
	}
	return nil
}

// ------------------------------------------------------------- request context

func withSuperuser(r *http.Request, su *Superuser) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKeySuperuser, su))
}

// SuperuserFrom returns the signed-in operator, if the request was
// authenticated by a session rather than a static admin token.
//
// A nil result is normal, not an error: it means a script authenticated with
// a token, and a handler that needs to attribute an action to a person should
// treat that as "the system" rather than refuse.
func SuperuserFrom(ctx context.Context) *Superuser {
	su, _ := ctx.Value(ctxKeySuperuser).(*Superuser)
	return su
}

// --------------------------------------------------------------- throttling

// loginThrottle slows down password guessing. It is in-process and therefore
// per-instance: behind several replicas an attacker gets one budget per
// replica. That is a deliberate trade — a shared counter would mean a store
// in Redis, and the outbox-not-a-queue reasoning applies here too. It raises
// the cost of online guessing, which is what it is for; it is not a
// substitute for a strong password.
type loginThrottle struct {
	mu       sync.Mutex
	attempts map[string]*attemptRecord
}

type attemptRecord struct {
	failures int
	until    time.Time
	seen     time.Time
}

const (
	throttleFreeAttempts  = 5
	throttleSprayAttempts = 30
	throttleBaseDelay     = 5 * time.Second
	throttleMaxDelay      = 15 * time.Minute
	throttleForget        = time.Hour
)

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{attempts: map[string]*attemptRecord{}}
}

// buckets returns the two counters a failure charges against.
//
// The first is keyed by identity *and* address, so one attacker cannot lock a
// real operator out of their own account by failing on their behalf. But that
// key alone gives every identity its own budget, which means password
// spraying — one likely password tried against a hundred addresses from one
// host — never trips anything. The second bucket is the address alone, with a
// larger allowance, so a single host guessing broadly is still slowed down.
func (t *loginThrottle) buckets(identity, clientIP string) [2]bucket {
	return [2]bucket{
		{key: identity + "\x00" + clientIP, free: throttleFreeAttempts},
		{key: "\x00ip\x00" + clientIP, free: throttleSprayAttempts},
	}
}

type bucket struct {
	key  string
	free int
}

func (t *loginThrottle) blocked(identity, clientIP string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gc()
	var longest time.Duration
	for _, b := range t.buckets(identity, clientIP) {
		rec := t.attempts[b.key]
		if rec == nil {
			continue
		}
		if d := time.Until(rec.until); d > longest {
			longest = d
		}
	}
	return longest, longest > 0
}

func (t *loginThrottle) fail(identity, clientIP string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for _, b := range t.buckets(identity, clientIP) {
		rec := t.attempts[b.key]
		if rec == nil {
			rec = &attemptRecord{}
			t.attempts[b.key] = rec
		}
		rec.failures++
		rec.seen = now
		if rec.failures > b.free {
			delay := throttleBaseDelay << min(rec.failures-b.free-1, 10)
			if delay > throttleMaxDelay || delay <= 0 {
				delay = throttleMaxDelay
			}
			rec.until = now.Add(delay)
		}
	}
}

// succeed clears the identity's own bucket but deliberately leaves the
// address bucket alone: one correct password does not vouch for the hundred
// wrong ones that came from the same host beside it.
func (t *loginThrottle) succeed(identity, clientIP string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.attempts, t.buckets(identity, clientIP)[0].key)
}

// gc drops records nobody has touched in an hour. Called under the lock from
// blocked(), which every login goes through, so the map cannot grow without
// bound from scattered one-off failures.
func (t *loginThrottle) gc() {
	cutoff := time.Now().Add(-throttleForget)
	for k, rec := range t.attempts {
		if rec.seen.Before(cutoff) && rec.until.Before(time.Now()) {
			delete(t.attempts, k)
		}
	}
}

// clientIP is the peer address with the port stripped. It deliberately does
// not read X-Forwarded-For: behind no proxy that header is attacker-supplied,
// and trusting it would let anyone reset their own throttle bucket at will.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ------------------------------------------------------------------- roles

// SetRole changes what an operator may do.
//
// The one rule: a store keeps at least one owner. Owner is the only role that
// carries settings.write, and settings.write is the only right that can hand
// out roles — so demoting the last one locks every remaining person out of the
// team screen with no way back in short of the database. Refusing is the whole
// of the protection; there is nothing else to check.
func (s *Superusers) SetRole(ctx context.Context, id int64, role string) (*Superuser, error) {
	if !ValidRole(role) {
		return nil, Validationf("%q is not a role; the roles are %s",
			role, strings.Join(Roles, ", "))
	}

	var su *Superuser
	err := InTx(ctx, s.db, func(tx *sql.Tx) error {
		// Locked and counted in the same transaction as the write, or two
		// requests demoting the two remaining owners both pass the check.
		// The owner rows are locked and then counted, rather than counted with
		// FOR UPDATE — PostgreSQL refuses that combination, and locking the rows
		// is what actually serialises two requests each demoting one of the last
		// two owners.
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM superusers WHERE role = $1 FOR UPDATE`, RoleOwner)
		if err != nil {
			return Internalf(err, "lock owners")
		}
		owners := 0
		for rows.Next() {
			owners++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return Internalf(err, "count owners")
		}
		rows.Close()
		var current string
		if err := tx.QueryRowContext(ctx,
			`SELECT role FROM superusers WHERE id = $1`, id).Scan(&current); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return NotFoundf("superuser %d does not exist", id)
			}
			return Internalf(err, "read superuser")
		}
		if current == RoleOwner && role != RoleOwner && owners == 1 {
			return Conflictf(
				"this is the only owner; make somebody else an owner before changing this one")
		}

		row := tx.QueryRowContext(ctx, `
			UPDATE superusers SET role = $2, updated_at = now()
			WHERE id = $1 RETURNING `+superuserColumns, id, role)
		su, err = scanSuperuser(row)
		if err != nil {
			return Internalf(err, "set role")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return su, nil
}
