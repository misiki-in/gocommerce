package gocommerce

import (
	"net/http"
	"time"
)

// Superuser routes. The shape mirrors PocketBase's superuser auth — an
// identity plus a password in, a token plus the record out — so the panel's
// login flow is the one PocketBase operators already know.
func (a *App) mountSuperuserRoutes() {
	// Public. A login endpoint cannot require a login, and the setup probe has
	// to answer before anyone can have credentials.
	a.HandleFunc("GET /api/admin/auth-state", a.handleAuthState)
	a.HandleFunc("POST /api/admin/auth-with-password", a.handleAuthWithPassword)
	a.HandleFunc("POST /api/admin/install", a.handleInstall)

	a.HandleAdminFunc("POST /api/admin/auth-refresh", a.handleAuthRefresh)
	a.HandleAdminFunc("POST /api/admin/auth-logout", a.handleAuthLogout)

	a.HandleAdminFunc("GET /api/admin/superusers", a.handleListSuperusers, RightSettingsWrite)
	a.HandleAdminFunc("POST /api/admin/superusers", a.handleCreateSuperuser, RightSettingsWrite)
	a.HandleAdminFunc("PATCH /api/admin/superusers/{id}", a.handleUpdateSuperuser, RightSettingsWrite)
	a.HandleAdminFunc("PUT /api/admin/superusers/{id}/role", a.handleSetRole, RightSettingsWrite)
	a.HandleAdminFunc("DELETE /api/admin/superusers/{id}", a.handleDeleteSuperuser, RightSettingsWrite)
}

// authResponse is what a successful sign-in returns: the credential and the
// operator it belongs to, in PocketBase's token/record shape.
type authResponse struct {
	Token     string     `json:"token"`
	ExpiresAt time.Time  `json:"expires_at"`
	Record    *Superuser `json:"record"`
}

// handleAuthState tells the panel whether anyone can log in yet. A fresh
// database has no superuser, and the panel needs to know that *before*
// rendering a login form nobody could satisfy.
func (a *App) handleAuthState(w http.ResponseWriter, r *http.Request) {
	n, err := a.superusers.Count(r.Context())
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, map[string]any{
		"installed": n > 0,
		// Whether a static token would also be accepted. The panel never uses
		// one, but an operator debugging a 401 benefits from seeing it.
		"token_auth": len(a.cfg.AdminTokens) > 0,
	})
}

// handleInstall creates the very first superuser. It is public *only* while no
// superuser exists: the moment one does, Bootstrap declines and this becomes a
// 409 for everyone. That is the same window PocketBase's installer runs in.
func (a *App) handleInstall(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	su, created, err := a.superusers.Bootstrap(r.Context(), in.Email, in.Password)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	if !created {
		RespondError(w, r, Conflictf("this installation already has a superuser; sign in instead"))
		return
	}
	sess, err := a.superusers.issue(r.Context(), su.ID)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	a.log.Info("superuser created", "email", su.Email, "via", "install")
	Respond(w, http.StatusCreated, authResponse{
		Token:     sess.Token,
		ExpiresAt: sess.ExpiresAt.UTC(),
		Record:    su,
	})
}

func (a *App) handleAuthWithPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		// PocketBase calls the username field "identity" because a collection
		// may authenticate on more than one field. Superusers authenticate on
		// email alone, but keeping the name means a PocketBase client works
		// here unchanged.
		Identity string `json:"identity"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	identity := in.Identity
	if identity == "" {
		identity = in.Email
	}
	su, sess, err := a.superusers.Authenticate(r.Context(), identity, in.Password, clientIP(r))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, authResponse{
		Token:     sess.Token,
		ExpiresAt: sess.ExpiresAt.UTC(),
		Record:    su,
	})
}

// handleAuthRefresh extends the caller's session and says who they are. The
// panel calls it on boot to turn a stored token back into an identity — and
// to discover that the token has expired without waiting for a real request
// to fail.
//
// The same token comes back, deliberately. See Superusers.Touch.
func (a *App) handleAuthRefresh(w http.ResponseWriter, r *http.Request) {
	token, _ := bearerToken(r)
	su, sess, ok := a.superusers.Touch(r.Context(), token)
	if !ok {
		// A static admin token got here. It is a valid credential but not a
		// person, so there is no record to return and nothing to refresh.
		RespondError(w, r, &APIError{
			Status:  http.StatusBadRequest,
			Code:    "not_a_session",
			Message: "this credential is a static admin token, not a superuser session",
		})
		return
	}
	Respond(w, http.StatusOK, authResponse{
		Token:     sess.Token,
		ExpiresAt: sess.ExpiresAt.UTC(),
		Record:    su,
	})
}

func (a *App) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	token, _ := bearerToken(r)
	if err := a.superusers.Revoke(r.Context(), token); err != nil {
		RespondError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleListSuperusers(w http.ResponseWriter, r *http.Request) {
	list, err := a.superusers.List(r.Context())
	if err != nil {
		RespondError(w, r, err)
		return
	}
	RespondList(w, list, ListMeta{Total: len(list), Limit: len(list)})
}

func (a *App) handleCreateSuperuser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	su, err := a.superusers.Create(r.Context(), in.Email, in.Password, in.Role)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	a.log.Info("superuser created", "email", su.Email, "via", "api")
	Respond(w, http.StatusCreated, su)
}

func (a *App) handleUpdateSuperuser(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	su, err := a.superusers.Update(r.Context(), id, in.Email, in.Password)
	if err != nil {
		RespondError(w, r, err)
		return
	}

	// A password change revokes every session that operator holds — including
	// the one being used right now. When they are changing their own password
	// through the panel, hand back a replacement in the same response: the
	// security property is that no session predating the change survives, not
	// that the person has to sign in again to finish the thing they started.
	if caller := SuperuserFrom(r.Context()); in.Password != "" && caller != nil && caller.ID == id {
		sess, err := a.superusers.issue(r.Context(), su.ID)
		if err != nil {
			RespondError(w, r, err)
			return
		}
		Respond(w, http.StatusOK, authResponse{
			Token:     sess.Token,
			ExpiresAt: sess.ExpiresAt.UTC(),
			Record:    su,
		})
		return
	}
	Respond(w, http.StatusOK, su)
}

func (a *App) handleDeleteSuperuser(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	if err := a.superusers.Delete(r.Context(), id); err != nil {
		RespondError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetRole changes what an operator may do. Separate from the general
// update because it is a different act: changing somebody's email is
// administration, and changing their role is granting or taking away access.
func (a *App) handleSetRole(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var in struct {
		Role string `json:"role"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	su, err := a.superusers.SetRole(r.Context(), id, in.Role)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	a.log.Info("superuser role changed", "email", su.Email, "role", su.Role)
	Respond(w, http.StatusOK, su)
}
