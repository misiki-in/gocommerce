package gocommerce

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Invitations are how somebody joins the team.
//
// The alternative — which is what this engine did until M18 — is for an owner
// to invent a password on the new person's behalf and then tell it to them.
// That password is known to two people from the moment it exists, it travels
// through whatever chat window was open, and it is almost never changed
// afterwards. An invitation moves the password to the only person who should
// ever know it, and gives the store a record of who was asked and who actually
// arrived.
//
// The token is generated once, returned once, and never stored: only its
// SHA-256 goes in the table, exactly as with a session. Losing the link means
// issuing a new invitation, which is the correct and cheap recovery.
type Invitations struct {
	app *App
}

// Team returns the invitation service.
func (a *App) Team() *Invitations { return a.invitations }

// How long an invitation link works for. Long enough to survive a weekend and
// a holiday; short enough that a link forgotten in an inbox is not a permanent
// way into the store.
const invitationTTL = 7 * 24 * time.Hour

// Invitation is an outstanding or historical invitation to join the team.
type Invitation struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"`
	// Rights is what the role carries *here*, resolved on the way out through
	// the store's matrix. The person deciding whether to send this needs to see
	// what they are handing over, and a role name alone does not say — least of
	// all in a store that has re-cut the role.
	Rights []Right `json:"rights"`
	// Token is the secret, and it is populated on exactly one response: the one
	// that created the invitation. Every later read leaves it empty, because by
	// then the store no longer knows it.
	Token string `json:"token,omitempty"`
	// AcceptURL is the link to send, filled in by the HTTP layer, which is the
	// only part that knows what this store is reached at. Populated alongside
	// Token and just as briefly.
	AcceptURL     string     `json:"accept_url,omitempty"`
	InvitedByID   *int64     `json:"invited_by_id,omitempty"`
	InvitedByName string     `json:"invited_by,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	AcceptedAt    *time.Time `json:"accepted_at,omitempty"`
	// Status is the one field the panel actually renders, because "expired" is
	// a fact about now rather than a column.
	Status string `json:"status"`
}

// The three states an invitation can be in.
const (
	InvitationPending  = "pending"
	InvitationAccepted = "accepted"
	InvitationExpired  = "expired"
)

const invitationColumns = `i.id, i.email, i.role, i.invited_by,
	coalesce(s.email, ''), i.created_at, i.expires_at, i.accepted_at`

// scanInvitation reads one invitation. rights is the store's resolved matrix,
// passed in rather than looked up here so that listing a page of invitations
// asks the question once — and so that what an invitee is told they are getting
// is the same set the store will actually give them.
func scanInvitation(row scanner, rights map[string][]Right) (*Invitation, error) {
	inv := &Invitation{}
	var invitedBy sql.NullInt64
	var accepted sql.NullTime
	if err := row.Scan(&inv.ID, &inv.Email, &inv.Role, &invitedBy,
		&inv.InvitedByName, &inv.CreatedAt, &inv.ExpiresAt, &accepted); err != nil {
		return nil, err
	}
	if invitedBy.Valid {
		inv.InvitedByID = &invitedBy.Int64
	}
	if accepted.Valid {
		inv.AcceptedAt = &accepted.Time
	}
	inv.Rights = rights[inv.Role]
	inv.Status = invitationStatus(inv.ExpiresAt, accepted)
	return inv, nil
}

func invitationStatus(expires time.Time, accepted sql.NullTime) string {
	switch {
	case accepted.Valid:
		return InvitationAccepted
	case time.Now().After(expires):
		return InvitationExpired
	default:
		return InvitationPending
	}
}

// Invite asks somebody to join the team in a role.
//
// It refuses an address that already belongs to an operator. Inviting somebody
// who is already here is either a mistake or an attempt to change their role,
// and the second one has its own endpoint that cannot be used to hand out a
// fresh password.
func (s *Invitations) Invite(ctx context.Context, email, role string, invitedBy *int64) (*Invitation, error) {
	email = normalizeEmail(email)
	if email == "" || !strings.Contains(email, "@") {
		return nil, Validationf("a valid email address is required")
	}
	if role == "" {
		role = RoleStaff
	}
	if !ValidRole(role) {
		return nil, Validationf("%q is not a role; the roles are %s", role, strings.Join(Roles, ", "))
	}

	token, err := newSessionToken()
	if err != nil {
		return nil, Internalf(err, "generate invitation token")
	}

	var id int64
	err = InTx(ctx, s.app.db, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT true FROM superusers WHERE email = $1`, email).Scan(&exists); err == nil {
			return Conflictf("%s is already on the team; change their role instead", email)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return Internalf(err, "look up superuser")
		}

		// Re-inviting somebody replaces the outstanding invitation rather than
		// failing on the unique index: an owner who resends a link means the
		// old one to stop working, and two live links for one person is exactly
		// what the index exists to prevent.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM superuser_invitations WHERE email = $1 AND accepted_at IS NULL`,
			email); err != nil {
			return Internalf(err, "clear the previous invitation")
		}

		return tx.QueryRowContext(ctx, `
			INSERT INTO superuser_invitations (email, role, token_hash, invited_by, expires_at)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			email, role, hashToken(token), invitedBy, time.Now().Add(invitationTTL),
		).Scan(&id)
	})
	if err != nil {
		return nil, err
	}

	inv, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	// The only time this is ever populated. After this response the store
	// cannot produce it again, which is the point of storing the hash.
	inv.Token = token
	return inv, nil
}

// Get returns one invitation, without its token.
func (s *Invitations) Get(ctx context.Context, id int64) (*Invitation, error) {
	rights, err := s.app.roles.All(ctx)
	if err != nil {
		return nil, err
	}
	row := s.app.db.QueryRowContext(ctx, `
		SELECT `+invitationColumns+`
		FROM superuser_invitations i
		LEFT JOIN superusers s ON s.id = i.invited_by
		WHERE i.id = $1`, id)
	inv, err := scanInvitation(row, rights)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NotFoundf("invitation %d does not exist", id)
	}
	if err != nil {
		return nil, Internalf(err, "read invitation")
	}
	return inv, nil
}

// List returns every invitation, outstanding ones first, then most recent.
//
// Accepted ones are kept and shown because "who let this person in" is a
// question a team screen should be able to answer without a database client.
func (s *Invitations) List(ctx context.Context) ([]*Invitation, error) {
	rights, err := s.app.roles.All(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.app.db.QueryContext(ctx, `
		SELECT `+invitationColumns+`
		FROM superuser_invitations i
		LEFT JOIN superusers s ON s.id = i.invited_by
		ORDER BY i.accepted_at IS NOT NULL, i.created_at DESC`)
	if err != nil {
		return nil, Internalf(err, "list invitations")
	}
	defer rows.Close()
	out := []*Invitation{}
	for rows.Next() {
		inv, err := scanInvitation(rows, rights)
		if err != nil {
			return nil, Internalf(err, "scan invitation")
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// Revoke closes an outstanding invitation. An accepted one is history and
// cannot be revoked: the way to undo that is to remove the operator.
func (s *Invitations) Revoke(ctx context.Context, id int64) error {
	res, err := s.app.db.ExecContext(ctx,
		`DELETE FROM superuser_invitations WHERE id = $1 AND accepted_at IS NULL`, id)
	if err != nil {
		return Internalf(err, "revoke invitation")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Distinguish the two, because they need different actions: one is a
		// stale screen, the other is "remove the person instead".
		var accepted bool
		err := s.app.db.QueryRowContext(ctx,
			`SELECT accepted_at IS NOT NULL FROM superuser_invitations WHERE id = $1`,
			id).Scan(&accepted)
		if errors.Is(err, sql.ErrNoRows) {
			return NotFoundf("invitation %d does not exist", id)
		}
		if err != nil {
			return Internalf(err, "read invitation")
		}
		return Conflictf("this invitation was already accepted; remove the operator instead")
	}
	return nil
}

// Lookup resolves a token so the accept page can say who the invitation is for
// and what it grants, before asking for a password.
//
// It is deliberately reachable without authentication — the invitee has no
// account yet, which is the whole point — and so it returns only what somebody
// holding the link already knows or is about to be told. The token itself is
// the secret; nothing here is enumerable without it.
func (s *Invitations) Lookup(ctx context.Context, token string) (*Invitation, error) {
	rights, err := s.app.roles.All(ctx)
	if err != nil {
		return nil, err
	}
	row := s.app.db.QueryRowContext(ctx, `
		SELECT `+invitationColumns+`
		FROM superuser_invitations i
		LEFT JOIN superusers s ON s.id = i.invited_by
		WHERE i.token_hash = $1`, hashToken(token))
	inv, err := scanInvitation(row, rights)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NotFoundf("this invitation link is not valid")
	}
	if err != nil {
		return nil, Internalf(err, "read invitation")
	}
	if err := usableInvitation(inv); err != nil {
		return nil, err
	}
	return inv, nil
}

// usableInvitation is the one place that decides whether a link still works, so
// that Lookup and Accept can never disagree about it.
func usableInvitation(inv *Invitation) error {
	switch inv.Status {
	case InvitationAccepted:
		return Conflictf("this invitation has already been used")
	case InvitationExpired:
		return Conflictf("this invitation expired on %s; ask for a new one",
			inv.ExpiresAt.Format("2 January 2006"))
	}
	return nil
}

// Accept turns an invitation into an operator and signs them in.
//
// Signing them in is not a convenience: an invitee who has just chosen a
// password and is then shown a login form will type it again, and the one thing
// worse than that is being told the credentials are wrong because they fumbled
// it the first time and there is nothing to compare against.
//
// The whole thing is one transaction, and the invitation is claimed with a
// conditional UPDATE, so two people opening the same link at once cannot both
// become operators.
func (s *Invitations) Accept(ctx context.Context, token, password string) (*Superuser, *Session, error) {
	rights, err := s.app.roles.All(ctx)
	if err != nil {
		return nil, nil, err
	}
	var su *Superuser
	err = InTx(ctx, s.app.db, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			SELECT `+invitationColumns+`
			FROM superuser_invitations i
			LEFT JOIN superusers s ON s.id = i.invited_by
			WHERE i.token_hash = $1 FOR UPDATE OF i`, hashToken(token))
		inv, err := scanInvitation(row, rights)
		if errors.Is(err, sql.ErrNoRows) {
			return NotFoundf("this invitation link is not valid")
		}
		if err != nil {
			return Internalf(err, "read invitation")
		}
		if err := usableInvitation(inv); err != nil {
			return err
		}
		// The address is validated here rather than at Invite: the password is
		// the invitee's, and it is the first time anybody has typed one.
		if err := validateCredentials(inv.Email, password); err != nil {
			return err
		}
		hash, err := hashPassword(password)
		if err != nil {
			return Internalf(err, "hash password")
		}

		created := tx.QueryRowContext(ctx, `
			INSERT INTO superusers (email, password_hash, role) VALUES ($1, $2, $3)
			RETURNING `+superuserColumns, inv.Email, hash, inv.Role)
		// scanSuperuser rather than the resolving scan: this is inside a
		// transaction, and that one would take a second pool connection.
		su, err = scanSuperuser(created)
		if err != nil {
			if isUniqueViolation(err) {
				return Conflictf("%s is already on the team; sign in instead", inv.Email)
			}
			return Internalf(err, "create superuser")
		}

		res, err := tx.ExecContext(ctx, `
			UPDATE superuser_invitations SET accepted_at = now()
			WHERE id = $1 AND accepted_at IS NULL`, inv.ID)
		if err != nil {
			return Internalf(err, "claim invitation")
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Somebody else got here first. Rolling back is what undoes the
			// operator this transaction just created.
			return Conflictf("this invitation has already been used")
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	su.Rights = rights[su.Role]

	sess, err := s.app.superusers.issue(ctx, su.ID)
	if err != nil {
		return nil, nil, err
	}
	return su, sess, nil
}
