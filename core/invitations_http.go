package gocommerce

import (
	"net/http"
	"strings"
)

func (a *App) mountTeamRoutes() {
	// Seeing the team is team.read; changing it is team.write, which can hand
	// somebody the owner role and so can hand somebody everything. Inviting is
	// a write for that reason: it is access being granted, not a screen the
	// invitee will later use.
	a.HandleAdminFunc("GET /api/admin/invitations", a.handleListInvitations, RightTeamRead)
	a.HandleAdminFunc("POST /api/admin/invitations", a.handleCreateInvitation, RightTeamWrite)
	a.HandleAdminFunc("DELETE /api/admin/invitations/{id}", a.handleRevokeInvitation, RightTeamWrite)

	// Unauthenticated, and necessarily so: the invitee has no account yet.
	// Holding the token is the credential, and neither route reveals anything
	// somebody without it could ask for.
	a.HandleFunc("GET /api/admin/invitations/accept/{token}", a.handleLookupInvitation)
	a.HandleFunc("POST /api/admin/invitations/accept/{token}", a.handleAcceptInvitation)

	// Self-service. No right is declared, which means any authenticated
	// operator: these act on the caller and on nobody else, and gating them
	// behind team.write is what forces a staff member to ask an owner to
	// choose a password for them.
	a.HandleAdminFunc("GET /api/admin/me", a.handleGetMe)
	a.HandleAdminFunc("PATCH /api/admin/me", a.handleUpdateMe)
	a.HandleAdminFunc("POST /api/admin/me/revoke-sessions", a.handleRevokeMySessions)

	// Ending somebody else's sessions is an act of team management, and the one
	// thing an owner wants before removing an account or reducing a role.
	a.HandleAdminFunc("POST /api/admin/superusers/{id}/revoke-sessions",
		a.handleRevokeSessions, RightTeamWrite)
}

func (a *App) handleListInvitations(w http.ResponseWriter, r *http.Request) {
	list, err := a.invitations.List(r.Context())
	if err != nil {
		RespondError(w, r, err)
		return
	}
	RespondList(w, list, ListMeta{Total: len(list), Limit: len(list), Offset: 0})
}

func (a *App) handleCreateInvitation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}

	// Who invited whom, when the caller is a person. The static admin token has
	// nobody behind it, so the invitation simply records no inviter rather than
	// attributing it to somebody who was not there.
	var invitedBy *int64
	if su := SuperuserFrom(r.Context()); su != nil {
		invitedBy = &su.ID
	}

	inv, err := a.invitations.Invite(r.Context(), in.Email, in.Role, invitedBy)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	// The link is assembled here rather than in the service, because only the
	// HTTP layer knows what this store is reached at. It is returned once and
	// never again.
	inv.AcceptURL = a.acceptURL(r, inv.Token)
	Respond(w, http.StatusCreated, inv)
}

// acceptURL is where the invitee goes. It is built from the request rather than
// from configuration, so a store reached through a proxy, a tunnel or a bare IP
// produces a link that actually resolves from where the operator is standing.
func (a *App) acceptURL(r *http.Request, token string) string {
	scheme := "https"
	if r.TLS == nil && !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "http"
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = fwd
	}
	return scheme + "://" + host + "/accept-invite/" + token
}

func (a *App) handleRevokeInvitation(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	if err := a.invitations.Revoke(r.Context(), id); err != nil {
		RespondError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleLookupInvitation(w http.ResponseWriter, r *http.Request) {
	inv, err := a.invitations.Lookup(r.Context(), r.PathValue("token"))
	respondOr(w, r, inv, err)
}

func (a *App) handleAcceptInvitation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	su, sess, err := a.invitations.Accept(r.Context(), r.PathValue("token"), in.Password)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	// The same shape the login route returns, so the panel can reuse the code
	// that stores a session rather than growing a second path for arriving.
	Respond(w, http.StatusCreated, authResponse{
		Token:     sess.Token,
		ExpiresAt: sess.ExpiresAt.UTC(),
		Record:    su,
	})
}

func (a *App) handleGetMe(w http.ResponseWriter, r *http.Request) {
	su := SuperuserFrom(r.Context())
	if su == nil {
		// The static admin token. It is a credential, not a person, and it has
		// no profile to show or change.
		RespondError(w, r, Forbiddenf(
			"this route is for a signed-in operator; the admin token has no account"))
		return
	}
	sessions, newest, err := a.superusers.Sessions(r.Context(), su.ID)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, map[string]any{
		"superuser":      su,
		"sessions":       sessions,
		"newest_session": newest,
	})
}

func (a *App) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	su := SuperuserFrom(r.Context())
	if su == nil {
		RespondError(w, r, Forbiddenf(
			"this route is for a signed-in operator; the admin token has no account"))
		return
	}
	var in struct {
		CurrentPassword string `json:"current_password"`
		Email           string `json:"email"`
		Password        string `json:"password"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	token, _ := bearerToken(r)
	updated, err := a.superusers.UpdateSelf(r.Context(), su.ID,
		in.CurrentPassword, in.Email, in.Password, token)
	respondOr(w, r, updated, err)
}

func (a *App) handleRevokeMySessions(w http.ResponseWriter, r *http.Request) {
	su := SuperuserFrom(r.Context())
	if su == nil {
		RespondError(w, r, Forbiddenf(
			"this route is for a signed-in operator; the admin token has no account"))
		return
	}
	n, err := a.superusers.RevokeAll(r.Context(), su.ID)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	// Including this browser: "sign out everywhere" that quietly excepted the
	// device you are on would not be what it says, and the person clicking it
	// usually means to start again from a known state.
	Respond(w, http.StatusOK, map[string]any{"revoked": n})
}

func (a *App) handleRevokeSessions(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	n, err := a.superusers.RevokeAll(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, map[string]any{"revoked": n})
}
