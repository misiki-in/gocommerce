package gocommerce

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The superuser suite. Passwords, sessions and the login throttle are the
// three places where a mistake is silent: a broken hash still returns a
// token, a session that outlives a password change still opens the panel, and
// a throttle that never fires looks exactly like one that does until someone
// is guessing. Each test below pins one of those down.

// TestMain winds the KDF cost down for the whole package. 600,000 iterations
// is the right number in production and the wrong one in a test binary: this
// file hashes and verifies dozens of passwords, and paying the real cost for
// each would spend minutes proving nothing that the iteration count encoded
// in every hash does not already prove.
func TestMain(m *testing.M) {
	pbkdf2Iterations = 1000
	os.Exit(m.Run())
}

// Nothing in this file calls t.Parallel: several tests move the package-level
// pbkdf2Iterations, and a parallel test resuming mid-move would race on it.

// ---------------------------------------------------------------- fixtures

// jsonBody attaches a JSON payload to a request built by do, which the shared
// helper leaves bodyless. The returned option is reusable: it encodes once and
// hands out a fresh reader per request.
func jsonBody(t *testing.T, v any) func(*http.Request) {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode request body: %v", err)
	}
	return func(r *http.Request) {
		r.Body = io.NopCloser(bytes.NewReader(encoded))
		r.ContentLength = int64(len(encoded))
		r.Header.Set("Content-Type", "application/json")
	}
}

// fromIP overrides the peer address, so a test can drive the login throttle
// from more than one client.
func fromIP(ip string) func(*http.Request) {
	return func(r *http.Request) { r.RemoteAddr = ip + ":54321" }
}

func newSuperuser(t *testing.T, app *App, email, password string) *Superuser {
	t.Helper()
	su, err := app.Superusers().Create(context.Background(), email, password, RoleOwner)
	if err != nil {
		t.Fatalf("create superuser %s: %v", email, err)
	}
	return su
}

func signIn(t *testing.T, app *App, email, password, ip string) *Session {
	t.Helper()
	_, sess, err := app.Superusers().Authenticate(context.Background(), email, password, ip)
	if err != nil {
		t.Fatalf("authenticate %s: %v", email, err)
	}
	return sess
}

// decodeData pulls the data half of the response envelope into v.
func decodeData(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (body %s)", err, rec.Body)
	}
	if err := json.Unmarshal(env.Data, v); err != nil {
		t.Fatalf("decode data: %v (body %s)", err, rec.Body)
	}
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) *APIError {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (body %s)", err, rec.Body)
	}
	if env.Error == nil {
		t.Fatalf("response carries no error: %s", rec.Body)
	}
	return env.Error
}

// ------------------------------------------------------------------ refresh

// Refreshing must extend the session in place and hand back the *same* token.
//
// The version that rotated it looked reasonable and broke two things at once:
// a request already in flight when the swap landed came back 401, and a second
// browser tab refreshing on load silently invalidated the first tab's
// credential. Both were intermittent, which is exactly why this is pinned.
func TestAuthRefreshKeepsTheSameToken(t *testing.T) {
	app := newTestApp(t)
	newSuperuser(t, app, "keeper@example.com", "a good password")
	sess := signIn(t, app, "keeper@example.com", "a good password", "10.0.0.1")

	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+sess.Token) }

	rec := do(t, app, http.MethodPost, "/api/admin/auth-refresh", bearer)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth-refresh = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var got struct {
		Token     string     `json:"token"`
		ExpiresAt time.Time  `json:"expires_at"`
		Record    *Superuser `json:"record"`
	}
	decodeData(t, rec, &got)

	if got.Token != sess.Token {
		t.Errorf("refresh rotated the token; a rotation invalidates other tabs and in-flight requests")
	}
	if !got.ExpiresAt.After(sess.ExpiresAt) {
		t.Errorf("expires_at = %v, want later than the original %v", got.ExpiresAt, sess.ExpiresAt)
	}
	if got.Record == nil || got.Record.Email != "keeper@example.com" {
		t.Errorf("record = %+v, want the signed-in operator", got.Record)
	}

	// The original token must still open a real admin route afterwards.
	if rec := do(t, app, http.MethodGet, "/api/admin/products?limit=1", bearer); rec.Code != http.StatusOK {
		t.Errorf("the token stopped working after a refresh: %d (body %s)", rec.Code, rec.Body)
	}

	// A static admin token is a valid credential but not a person, so there is
	// nothing to refresh — and the panel relies on that being a 400 rather than
	// a 401, so it keeps the token instead of signing itself out.
	rec = do(t, app, http.MethodPost, "/api/admin/auth-refresh", withAdmin)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("auth-refresh with a static token = %d, want 400", rec.Code)
	}
	if code := decodeError(t, rec).Code; code != "not_a_session" {
		t.Errorf("error code = %q, want not_a_session", code)
	}
}

// ---------------------------------------------------------------- passwords

func TestPasswordHashingRoundTrip(t *testing.T) {
	const password = "correct horse battery staple"

	hash, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}

	t.Run("verifies against its own password", func(t *testing.T) {
		if !verifyPassword(hash, password) {
			t.Error("a freshly made hash does not verify")
		}
	})

	t.Run("rejects anything else", func(t *testing.T) {
		for _, wrong := range []string{
			"",
			"correct horse battery stapl",
			"correct horse battery staple ",
			"Correct Horse Battery Staple",
		} {
			if verifyPassword(hash, wrong) {
				t.Errorf("verifyPassword accepted %q", wrong)
			}
		}
	})

	t.Run("hashes are salted", func(t *testing.T) {
		// Identical hashes for one password would tell whoever steals the table
		// which operators share a password, and would let a single cracked hash
		// open more than one account.
		second, err := hashPassword(password)
		if err != nil {
			t.Fatalf("hashPassword: %v", err)
		}
		if second == hash {
			t.Error("two hashes of the same password are identical — the salt is not random")
		}
		if !verifyPassword(second, password) {
			t.Error("the second hash does not verify")
		}
	})

	t.Run("a malformed hash is a mismatch, not a panic", func(t *testing.T) {
		// A corrupted or hand-edited row has to fail closed. A panic here would
		// be a 500 on the login endpoint; a true would be an authentication
		// bypass that no other test would notice.
		garbage := []string{
			"",
			"$",
			"$$$",
			"not-a-hash",
			"pbkdf2-sha256",
			"pbkdf2-sha256$1000$c2FsdA",              // too few fields
			"pbkdf2-sha256$1000$c2FsdA$a2V5$trailer", // too many
			"bcrypt$1000$c2FsdA$a2V5",                // another scheme
			"PBKDF2-SHA256$1000$c2FsdA$a2V5",         // the scheme is not case-folded
			"pbkdf2-sha256$abc$c2FsdA$a2V5",          // iterations are not a number
			"pbkdf2-sha256$0$c2FsdA$a2V5",
			"pbkdf2-sha256$-1$c2FsdA$a2V5",
			"pbkdf2-sha256$1000$!!!!$a2V5",   // the salt is not base64
			"pbkdf2-sha256$1000$c2FsdA$!!!!", // the key is not base64
			// An empty key is the interesting one: a constant-time compare of two
			// empty slices is a match, so this must be rejected before it gets
			// there or every password would verify against it.
			"pbkdf2-sha256$1000$c2FsdA$",
			"pbkdf2-sha256$1000$$",
		}
		for _, encoded := range garbage {
			for _, attempt := range []string{password, "", "anything"} {
				if verifyPassword(encoded, attempt) {
					t.Errorf("verifyPassword(%q, %q) = true", encoded, attempt)
				}
			}
		}
	})
}

// TestPasswordVerifiesAcrossIterationCounts is the reason the cost is written
// into the hash. Raising pbkdf2Iterations has to be a one-line change, not a
// migration and a flag day: an operator who has not signed in since the change
// still verifies against the count their own hash was made with.
func TestPasswordVerifiesAcrossIterationCounts(t *testing.T) {
	const password = "a-passphrase-worth-keeping"

	current := pbkdf2Iterations
	t.Cleanup(func() { pbkdf2Iterations = current })

	const legacyCost = 137
	pbkdf2Iterations = legacyCost
	legacy, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}

	pbkdf2Iterations = current * 4
	if !strings.Contains(legacy, "$"+strconv.Itoa(legacyCost)+"$") {
		t.Errorf("hash %q does not record the cost it was made with", legacy)
	}
	if !verifyPassword(legacy, password) {
		t.Errorf("a hash made at %d iterations stopped verifying once the cost rose to %d",
			legacyCost, pbkdf2Iterations)
	}
	if verifyPassword(legacy, "not-"+password) {
		t.Error("a hash made at a different cost accepted the wrong password")
	}

	// And the other direction: a hash made at the higher cost still verifies
	// after the knob comes back down, so lowering it in a test cannot
	// invalidate anything.
	expensive, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	pbkdf2Iterations = legacyCost
	if !verifyPassword(expensive, password) {
		t.Error("a hash made at a higher cost stopped verifying once the cost fell")
	}
}

// --------------------------------------------------------------- management

func TestSuperuserCRUD(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	svc := app.Superusers()

	var ops, boundary *Superuser

	t.Run("create normalises the email", func(t *testing.T) {
		var err error
		ops, err = svc.Create(ctx, "  Ops@Example.COM\t", "ops-operator-password", RoleOwner)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if ops.Email != "ops@example.com" {
			t.Errorf("email = %q, want it lowercased and trimmed", ops.Email)
		}
		if ops.ID == 0 || ops.CreatedAt.IsZero() || ops.UpdatedAt.IsZero() {
			t.Errorf("Create returned an incomplete record: %+v", ops)
		}
	})

	t.Run("a duplicate email is a conflict", func(t *testing.T) {
		// Normalisation and the unique index have to agree. If they did not, a
		// second "OPS@example.com" would become a shadow account with its own
		// password that nobody looking at the panel would spot.
		for _, dup := range []string{"ops@example.com", "OPS@Example.com", " ops@example.com "} {
			_, err := svc.Create(ctx, dup, "another-fine-password", RoleOwner)
			if !errors.Is(err, ErrConflict) {
				t.Errorf("Create(%q) error = %v, want a conflict", dup, err)
			}
		}
	})

	t.Run("credentials are validated", func(t *testing.T) {
		tests := []struct {
			name     string
			email    string
			password string
		}{
			{name: "empty email", email: "", password: "long-enough-password"},
			{name: "whitespace email", email: "   ", password: "long-enough-password"},
			{name: "not an address", email: "not-an-email", password: "long-enough-password"},
			{name: "no domain", email: "someone@", password: "long-enough-password"},
			{name: "empty password", email: "short@example.com", password: ""},
			{
				name:  "one rune short",
				email: "short@example.com", password: strings.Repeat("x", MinPasswordLength-1),
			},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				got, err := svc.Create(ctx, tc.email, tc.password, RoleOwner)
				if !errors.Is(err, ErrValidation) {
					t.Fatalf("Create(%q, %q) = (%v, %v), want a validation error", tc.email, tc.password, got, err)
				}
			})
		}
	})

	t.Run("the minimum length itself is allowed", func(t *testing.T) {
		var err error
		boundary, err = svc.Create(ctx, "boundary@example.com", strings.Repeat("x", MinPasswordLength), RoleOwner)
		if err != nil {
			t.Fatalf("a password of exactly MinPasswordLength (%d) was rejected: %v", MinPasswordLength, err)
		}
	})

	t.Run("list is oldest first", func(t *testing.T) {
		list, err := svc.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("List returned %d superusers, want 2 — the rejected creates left rows behind", len(list))
		}
		if list[0].ID != ops.ID || list[1].ID != boundary.ID {
			t.Errorf("List order = %d, %d, want %d, %d", list[0].ID, list[1].ID, ops.ID, boundary.ID)
		}

		n, err := svc.Count(ctx)
		if err != nil {
			t.Fatalf("Count: %v", err)
		}
		if n != len(list) {
			t.Errorf("Count = %d but List returned %d", n, len(list))
		}
	})

	t.Run("update", func(t *testing.T) {
		t.Run("email is normalised", func(t *testing.T) {
			got, err := svc.Update(ctx, boundary.ID, "  Edge@Example.COM ", "")
			if err != nil {
				t.Fatalf("Update: %v", err)
			}
			if got.Email != "edge@example.com" {
				t.Errorf("email = %q, want it lowercased and trimmed", got.Email)
			}
		})

		t.Run("password alone is accepted", func(t *testing.T) {
			if _, err := svc.Update(ctx, boundary.ID, "", "a-brand-new-password"); err != nil {
				t.Fatalf("Update: %v", err)
			}
		})

		t.Run("rejections", func(t *testing.T) {
			tests := []struct {
				name     string
				id       int64
				email    string
				password string
				want     error
			}{
				{name: "nothing to change", id: boundary.ID, want: ErrValidation},
				{name: "invalid email", id: boundary.ID, email: "nope", want: ErrValidation},
				{
					name: "short password", id: boundary.ID,
					password: strings.Repeat("x", MinPasswordLength-1), want: ErrValidation,
				},
				{name: "taken email", id: boundary.ID, email: "ops@example.com", want: ErrConflict},
				{name: "no such superuser", id: 987654, email: "ghost@example.com", want: ErrNotFound},
			}
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					_, err := svc.Update(ctx, tc.id, tc.email, tc.password)
					if !errors.Is(err, tc.want) {
						t.Errorf("Update error = %v, want %v", err, tc.want)
					}
				})
			}
		})
	})

	t.Run("delete", func(t *testing.T) {
		// While two exist, a missing id is a plain not-found. Checked before the
		// row goes, because Delete's last-superuser guard runs first and would
		// otherwise answer the count question instead of the identity one.
		if err := svc.Delete(ctx, 987654); !errors.Is(err, ErrNotFound) {
			t.Errorf("deleting a missing superuser: error = %v, want not-found", err)
		}
		if err := svc.Delete(ctx, boundary.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		list, err := svc.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 1 || list[0].ID != ops.ID {
			t.Errorf("after the delete List returned %d rows, want just %d", len(list), ops.ID)
		}
	})
}

// TestSuperuserDeleteKeepsTheLastOne pins the one refusal an operator cannot
// argue with: an admin panel nobody can sign in to is not a state anyone can
// click their way out of.
func TestSuperuserDeleteKeepsTheLastOne(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	svc := app.Superusers()

	only := newSuperuser(t, app, "only@example.com", "only-operator-password")

	if err := svc.Delete(ctx, only.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleting the last superuser: error = %v, want a conflict", err)
	}
	if n, err := svc.Count(ctx); err != nil || n != 1 {
		t.Fatalf("Count = %d (err %v) after the refused delete, want 1", n, err)
	}

	second := newSuperuser(t, app, "second@example.com", "second-operator-password")
	if err := svc.Delete(ctx, only.ID); err != nil {
		t.Fatalf("deleting one of two superusers: %v", err)
	}

	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != second.ID {
		t.Errorf("survivors = %+v, want only %d", list, second.ID)
	}
}

// ---------------------------------------------------------- authentication

func TestSuperuserAuthenticate(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	svc := app.Superusers()

	const email, password = "pilot@example.com", "pilot-operator-password"
	su := newSuperuser(t, app, email, password)

	t.Run("correct credentials issue a session", func(t *testing.T) {
		// The identity goes through the same normalisation as the stored email,
		// or an operator whose browser capitalised the first letter could not
		// sign in to their own account.
		got, sess, err := svc.Authenticate(ctx, "  PILOT@Example.com  ", password, "203.0.113.1")
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if got.ID != su.ID || got.Email != email {
			t.Errorf("authenticated as %+v, want %d/%s", got, su.ID, email)
		}
		if sess.Token == "" {
			t.Error("no session token was issued")
		}
		if !sess.ExpiresAt.After(time.Now()) {
			t.Errorf("session expires at %s, which is not in the future", sess.ExpiresAt)
		}
	})

	var wrongPassword, unknownIdentity *APIError

	t.Run("a wrong password fails", func(t *testing.T) {
		_, _, err := svc.Authenticate(ctx, email, "not-the-password", "203.0.113.2")
		if !errors.As(err, &wrongPassword) {
			t.Fatalf("error = %v, want an APIError", err)
		}
	})

	t.Run("an unknown identity fails", func(t *testing.T) {
		_, _, err := svc.Authenticate(ctx, "nobody@example.com", password, "203.0.113.3")
		if !errors.As(err, &unknownIdentity) {
			t.Fatalf("error = %v, want an APIError", err)
		}
	})

	t.Run("the two failures are indistinguishable", func(t *testing.T) {
		// This is the account-enumeration guarantee. If "no such operator" read
		// differently from "wrong password", the login form would answer whether
		// an address has an account here — which is the first thing an attacker
		// wants to know, and the cheapest thing to ask for.
		if wrongPassword.Message != unknownIdentity.Message {
			t.Errorf("wrong password says %q but an unknown identity says %q",
				wrongPassword.Message, unknownIdentity.Message)
		}
		if wrongPassword.Code != unknownIdentity.Code {
			t.Errorf("codes differ: %q vs %q", wrongPassword.Code, unknownIdentity.Code)
		}
		if wrongPassword.Status != unknownIdentity.Status {
			t.Errorf("statuses differ: %d vs %d", wrongPassword.Status, unknownIdentity.Status)
		}
		if wrongPassword.Code != "invalid_credentials" {
			t.Errorf("code = %q, want invalid_credentials", wrongPassword.Code)
		}
		// And neither may name the address it was given, which would leak the
		// same fact by another route.
		for _, msg := range []string{wrongPassword.Message, unknownIdentity.Message} {
			if strings.Contains(msg, email) || strings.Contains(msg, "nobody@example.com") {
				t.Errorf("the failure message names the identity: %q", msg)
			}
		}
	})

	t.Run("an empty password never authenticates", func(t *testing.T) {
		if _, _, err := svc.Authenticate(ctx, email, "", "203.0.113.4"); err == nil {
			t.Error("an empty password was accepted")
		}
	})
}

func TestSuperuserSessionLifecycle(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	svc := app.Superusers()

	const email, password = "session@example.com", "session-operator-password"
	su := newSuperuser(t, app, email, password)
	sess := signIn(t, app, email, password, "203.0.113.10")

	t.Run("the token resolves to its operator", func(t *testing.T) {
		got, ok := svc.Resolve(ctx, sess.Token)
		if !ok {
			t.Fatal("a session token issued a moment ago does not resolve")
		}
		if got.ID != su.ID {
			t.Errorf("resolved to superuser %d, want %d", got.ID, su.ID)
		}
	})

	t.Run("only the hash is stored", func(t *testing.T) {
		// The token exists in the operator's browser and nowhere else, so a
		// database leak yields nothing anyone can sign in with.
		var raw int
		if err := app.DB().QueryRowContext(ctx,
			`SELECT count(*) FROM superuser_sessions WHERE token_hash = $1`, sess.Token).Scan(&raw); err != nil {
			t.Fatalf("query: %v", err)
		}
		if raw != 0 {
			t.Error("the raw session token is stored in superuser_sessions")
		}
	})

	t.Run("unknown tokens resolve to nobody", func(t *testing.T) {
		for _, token := range []string{"", "not-a-real-token", sess.Token + "x"} {
			if _, ok := svc.Resolve(ctx, token); ok {
				t.Errorf("Resolve(%q) succeeded", token)
			}
		}
	})

	t.Run("revoke ends it", func(t *testing.T) {
		if err := svc.Revoke(ctx, sess.Token); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		if _, ok := svc.Resolve(ctx, sess.Token); ok {
			t.Error("a revoked session still resolves")
		}
		// Revoking twice is not an error: a client that retries a logout it
		// never saw the answer to must not get a 500 for its trouble.
		if err := svc.Revoke(ctx, sess.Token); err != nil {
			t.Errorf("revoking an already-revoked session: %v", err)
		}
	})
}

// TestSuperuserPasswordChangeRevokesSessions is the pair of assertions that
// make a password change worth making.
func TestSuperuserPasswordChangeRevokesSessions(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	svc := app.Superusers()

	const original, replacement = "original-operator-password", "replacement-operator-password"
	su := newSuperuser(t, app, "rotate@example.com", original)
	sess := signIn(t, app, su.Email, original, "203.0.113.20")

	t.Run("an email change leaves sessions alone", func(t *testing.T) {
		// A rename is housekeeping, not a compromise. Signing the operator out
		// of the panel they are standing in front of would be noise, and would
		// train them to expect a logout for every edit.
		if _, err := svc.Update(ctx, su.ID, "renamed@example.com", ""); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if _, ok := svc.Resolve(ctx, sess.Token); !ok {
			t.Error("changing the email revoked a live session")
		}
	})

	t.Run("a password change revokes them", func(t *testing.T) {
		// A password change is how someone answers a compromise. A session that
		// survived it would leave the attacker signed in, which is the exact
		// outcome the operator was trying to prevent.
		if _, err := svc.Update(ctx, su.ID, "", replacement); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if _, ok := svc.Resolve(ctx, sess.Token); ok {
			t.Error("a session issued before the password change still resolves")
		}
	})

	t.Run("only the new password works", func(t *testing.T) {
		if _, _, err := svc.Authenticate(ctx, "renamed@example.com", original, "203.0.113.21"); err == nil {
			t.Error("the replaced password still authenticates")
		}
		if _, _, err := svc.Authenticate(ctx, "renamed@example.com", replacement, "203.0.113.22"); err != nil {
			t.Errorf("the new password does not authenticate: %v", err)
		}
	})
}

// ------------------------------------------------------------------- HTTP

func TestAuthWithPasswordOverHTTP(t *testing.T) {
	app := newTestApp(t)
	const email, password = "panel@example.com", "panel-operator-password"
	newSuperuser(t, app, email, password)

	t.Run("good credentials return a token and a record", func(t *testing.T) {
		rec := do(t, app, http.MethodPost, "/api/admin/auth-with-password",
			jsonBody(t, map[string]string{"identity": email, "password": password}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}

		var got authResponse
		decodeData(t, rec, &got)
		if got.Token == "" {
			t.Error("the response carries no token")
		}
		if got.Record == nil || got.Record.Email != email {
			t.Errorf("record = %+v, want the operator %s", got.Record, email)
		}
		if !got.ExpiresAt.After(time.Now()) {
			t.Errorf("expires_at = %s, which is not in the future", got.ExpiresAt)
		}

		// The handler serialises the Superuser struct itself, so the missing
		// json tag on passwordHash is the only thing keeping the hash out of the
		// wire format. Assert on the raw body: a tag added by accident would
		// still decode into authResponse without complaint.
		body := rec.Body.String()
		for _, leak := range []string{"pbkdf2", "password_hash", "passwordHash", "password", password} {
			if strings.Contains(body, leak) {
				t.Errorf("the login response leaks %q: %s", leak, body)
			}
		}
	})

	t.Run("the email field also works as the identity", func(t *testing.T) {
		// PocketBase clients send "identity"; a hand-written one is likely to
		// send "email". Both are accepted so neither has to be corrected.
		rec := do(t, app, http.MethodPost, "/api/admin/auth-with-password",
			jsonBody(t, map[string]string{"email": email, "password": password}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
	})

	t.Run("bad credentials are refused identically", func(t *testing.T) {
		wrong := do(t, app, http.MethodPost, "/api/admin/auth-with-password",
			jsonBody(t, map[string]string{"identity": email, "password": "not-the-password"}),
			fromIP("198.51.100.1"))
		unknown := do(t, app, http.MethodPost, "/api/admin/auth-with-password",
			jsonBody(t, map[string]string{"identity": "ghost@example.com", "password": password}),
			fromIP("198.51.100.2"))

		if wrong.Code != unknown.Code {
			t.Errorf("statuses differ: %d for a wrong password, %d for an unknown identity",
				wrong.Code, unknown.Code)
		}
		// Byte-identical bodies: the enumeration guarantee has to survive the
		// trip through the error envelope, not just hold inside the service.
		if wrong.Body.String() != unknown.Body.String() {
			t.Errorf("the two failures answer differently:\n  wrong password:   %s\n  unknown identity: %s",
				wrong.Body, unknown.Body)
		}
		if code := decodeError(t, wrong).Code; code != "invalid_credentials" {
			t.Errorf("code = %q, want invalid_credentials", code)
		}
	})
}

// TestSessionTokenSatisfiesAdminAuth proves the claim the whole design rests
// on: a session and a static token are both just "Bearer <x>", and no handler
// has to know which one it got.
func TestSessionTokenSatisfiesAdminAuth(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	const email, password = "operator@example.com", "operator-panel-password"
	su := newSuperuser(t, app, email, password)
	sess := signIn(t, app, email, password, "203.0.113.30")

	// A probe mounted through the engine's own (unrestricted) wiring, so the
	// test can see which credential the middleware attributed the request to.
	var seen *Superuser
	app.HandleAdminFunc("GET /api/admin/probe/whoami", func(w http.ResponseWriter, r *http.Request) {
		seen = SuperuserFrom(r.Context())
		Respond(w, http.StatusOK, map[string]bool{"ok": true})
	})

	const route = "/api/admin/products"

	t.Run("a session token opens an admin route", func(t *testing.T) {
		rec := do(t, app, http.MethodGet, route, header("Authorization", "Bearer "+sess.Token))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
	})

	t.Run("a static admin token still does too", func(t *testing.T) {
		// The regression that matters: adding sessions must not have cost
		// scripts and CI their credential.
		rec := do(t, app, http.MethodGet, route, withAdmin)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
	})

	t.Run("the request knows which one it was", func(t *testing.T) {
		seen = nil
		if rec := do(t, app, http.MethodGet, "/api/admin/probe/whoami",
			header("Authorization", "Bearer "+sess.Token)); rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		if seen == nil || seen.ID != su.ID {
			t.Errorf("SuperuserFrom = %+v, want the signed-in operator %d", seen, su.ID)
		}

		// A static token is a valid credential but not a person, so there is
		// nobody to attribute the request to.
		seen = nil
		if rec := do(t, app, http.MethodGet, "/api/admin/probe/whoami", withAdmin); rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		if seen != nil {
			t.Errorf("SuperuserFrom = %+v for a static token, want nil", seen)
		}
	})

	t.Run("bad credentials are refused", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			opts []func(*http.Request)
		}{
			{name: "none"},
			{name: "not a token", opts: []func(*http.Request){header("Authorization", "Bearer nonsense")}},
			{name: "wrong scheme", opts: []func(*http.Request){header("Authorization", "Basic "+sess.Token)}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := do(t, app, http.MethodGet, route, tc.opts...)
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("status = %d, want 401", rec.Code)
				}
			})
		}
	})

	t.Run("a revoked session stops opening it", func(t *testing.T) {
		if err := app.Superusers().Revoke(ctx, sess.Token); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		rec := do(t, app, http.MethodGet, route, header("Authorization", "Bearer "+sess.Token))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 — the revoked session is still accepted", rec.Code)
		}
	})
}

// TestAuthStateAndInstall walks a fresh install: the panel asks whether anyone
// can sign in, creates the first operator when nobody can, and finds the door
// closed the second time.
func TestAuthStateAndInstall(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	state := func(t *testing.T) map[string]any {
		t.Helper()
		rec := do(t, app, http.MethodGet, "/api/admin/auth-state")
		if rec.Code != http.StatusOK {
			t.Fatalf("auth-state status = %d, want 200: %s", rec.Code, rec.Body)
		}
		var got map[string]any
		decodeData(t, rec, &got)
		return got
	}

	// The probe has to be public and has to answer before anyone has
	// credentials, or a fresh install would render a login form nobody could
	// satisfy.
	empty := state(t)
	if empty["installed"] != false {
		t.Errorf("installed = %v on an empty database, want false", empty["installed"])
	}
	if empty["token_auth"] != true {
		t.Errorf("token_auth = %v, want true — this app is configured with a static token", empty["token_auth"])
	}

	install := func(email, password string) *httptest.ResponseRecorder {
		return do(t, app, http.MethodPost, "/api/admin/install",
			jsonBody(t, map[string]string{"email": email, "password": password}))
	}

	var created authResponse
	t.Run("the first install creates the operator", func(t *testing.T) {
		rec := install("First@Example.com", "first-operator-password")
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
		}
		decodeData(t, rec, &created)
		if created.Record == nil || created.Record.Email != "first@example.com" {
			t.Errorf("record = %+v, want the normalised address", created.Record)
		}
		if created.Token == "" {
			t.Fatal("the installer returned no token")
		}
	})

	t.Run("the installer hands back a working session", func(t *testing.T) {
		// Otherwise the panel would have to bounce straight to a login form
		// using credentials the operator typed ten seconds ago.
		rec := do(t, app, http.MethodGet, "/api/admin/products",
			header("Authorization", "Bearer "+created.Token))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
	})

	t.Run("auth-state now reports installed", func(t *testing.T) {
		if got := state(t); got["installed"] != true {
			t.Errorf("installed = %v after the install, want true", got["installed"])
		}
	})

	t.Run("a second install is a conflict", func(t *testing.T) {
		// The endpoint is public only while the window is open. Once an operator
		// exists it would be an unauthenticated account-creation form.
		rec := install("second@example.com", "second-operator-password")
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
		}
		list, err := app.Superusers().List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 1 {
			t.Errorf("there are now %d superusers, want 1 — the refused install created one anyway", len(list))
		}
	})
}

// ---------------------------------------------------------------- throttling

func TestLoginThrottle(t *testing.T) {
	const (
		identity = "target@example.com"
		attacker = "203.0.113.66"
	)

	t.Run("the first attempts are free", func(t *testing.T) {
		th := newLoginThrottle()
		for i := range throttleFreeAttempts {
			th.fail(identity, attacker)
			if _, blocked := th.blocked(identity, attacker); blocked {
				t.Fatalf("blocked after %d failures; the first %d are meant to be free",
					i+1, throttleFreeAttempts)
			}
		}

		th.fail(identity, attacker)
		retryAfter, blocked := th.blocked(identity, attacker)
		if !blocked {
			t.Fatalf("still unblocked after %d failures", throttleFreeAttempts+1)
		}
		if retryAfter <= 0 || retryAfter > throttleMaxDelay {
			t.Errorf("retry-after = %v, want something between 0 and %v", retryAfter, throttleMaxDelay)
		}
	})

	t.Run("a success clears the counter", func(t *testing.T) {
		// Someone who mistypes their password four times and then gets it right
		// must not be one slip away from a lockout for the rest of the hour.
		th := newLoginThrottle()
		for range throttleFreeAttempts {
			th.fail(identity, attacker)
		}
		th.succeed(identity, attacker)

		for i := range throttleFreeAttempts {
			th.fail(identity, attacker)
			if _, blocked := th.blocked(identity, attacker); blocked {
				t.Fatalf("blocked %d failures after a successful login, want a full budget again", i+1)
			}
		}
	})

	t.Run("buckets are keyed by identity and client", func(t *testing.T) {
		th := newLoginThrottle()
		for range throttleFreeAttempts + 1 {
			th.fail(identity, attacker)
		}
		if _, blocked := th.blocked(identity, attacker); !blocked {
			t.Fatal("the failing client is not blocked")
		}

		// Keyed by the client too, or an attacker could lock a real operator out
		// of their own account by failing on their behalf — a denial of service
		// that costs nothing to run.
		if _, blocked := th.blocked(identity, "198.51.100.7"); blocked {
			t.Error("failures from one client blocked the same identity from another")
		}
		// And keyed by identity, or one bad guess would freeze every account
		// behind a shared NAT.
		if _, blocked := th.blocked("someone-else@example.com", attacker); blocked {
			t.Error("failures against one identity blocked another from the same client")
		}
	})
}

func TestAuthenticateThrottlesRepeatedFailures(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	svc := app.Superusers()

	const email, password = "throttled@example.com", "throttled-operator-password"
	newSuperuser(t, app, email, password)

	const attacker = "203.0.113.99"
	for i := range throttleFreeAttempts + 1 {
		_, _, err := svc.Authenticate(ctx, email, "not-the-password", attacker)
		if err == nil {
			t.Fatalf("attempt %d succeeded with the wrong password", i+1)
		}
	}

	// The correct password is refused too: once the budget is spent the answer
	// stops depending on the credential, which is what makes the throttle worth
	// having against someone who is about to guess right.
	_, _, err := svc.Authenticate(ctx, email, password, attacker)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want an APIError", err)
	}
	if apiErr.Status != http.StatusTooManyRequests || apiErr.Code != "too_many_attempts" {
		t.Errorf("error = %d %s, want 429 too_many_attempts", apiErr.Status, apiErr.Code)
	}

	if _, _, err := svc.Authenticate(ctx, email, password, "198.51.100.42"); err != nil {
		t.Errorf("the operator's own client was blocked by someone else's failures: %v", err)
	}
}

func TestAuthWithPasswordThrottlesOverHTTP(t *testing.T) {
	app := newTestApp(t)
	const email, password = "http-throttle@example.com", "http-throttle-password"
	newSuperuser(t, app, email, password)

	login := func(attempt, ip string) *httptest.ResponseRecorder {
		return do(t, app, http.MethodPost, "/api/admin/auth-with-password",
			jsonBody(t, map[string]string{"identity": email, "password": attempt}), fromIP(ip))
	}

	const attacker = "203.0.113.77"
	for i := range throttleFreeAttempts + 1 {
		if rec := login("not-the-password", attacker); rec.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d status = %d, want the ordinary rejection: %s", i+1, rec.Code, rec.Body)
		}
	}

	rec := login(password, attacker)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", rec.Code, rec.Body)
	}
	if code := decodeError(t, rec).Code; code != "too_many_attempts" {
		t.Errorf("code = %q, want too_many_attempts", code)
	}

	// clientIP reads the peer address only — never X-Forwarded-For, which
	// behind no proxy an attacker sets themselves. A forged header must not
	// buy a fresh budget.
	forged := do(t, app, http.MethodPost, "/api/admin/auth-with-password",
		jsonBody(t, map[string]string{"identity": email, "password": password}),
		fromIP(attacker), header("X-Forwarded-For", "198.51.100.200"))
	if forged.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d with a forged X-Forwarded-For, want the block to hold at 429", forged.Code)
	}

	if rec := login(password, "198.51.100.201"); rec.Code != http.StatusOK {
		t.Errorf("a different client got %d, want 200: %s", rec.Code, rec.Body)
	}
}
