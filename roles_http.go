package gocommerce

import "net/http"

// Role routes. Reading the matrix and writing it sit behind the same right,
// settings.write, because they are the same act at different speeds: an
// operator who can see exactly which rights each role is missing is being
// handed the map of the store's access, and the only people with a use for it
// are the people who may redraw it. An operator's own rights reach them on
// their record, so nobody needs this to find out what they may do.
func (a *App) mountRoleRoutes() {
	a.HandleAdminFunc("GET /api/admin/roles", a.handleListRoles, RightSettingsWrite)
	a.HandleAdminFunc("PUT /api/admin/roles/{role}", a.handleSetRoleRights, RightSettingsWrite)
	a.HandleAdminFunc("DELETE /api/admin/roles/{role}", a.handleResetRoleRights, RightSettingsWrite)
}

// handleListRoles returns the whole matrix: every role, the closed list of
// rights, and the floor. One response rather than three, because a client
// rendering a grid needs all of it and a client rendering half of it draws
// something wrong.
func (a *App) handleListRoles(w http.ResponseWriter, r *http.Request) {
	matrix, err := a.roles.Matrix(r.Context())
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, matrix)
}

// handleSetRoleRights replaces one role's set.
//
// PUT and not PATCH: the body is the whole set the operator means the role to
// have, which is what the screen has in front of them. A partial update would
// have to invent a way to say "and take this one away", and two clients each
// adding one right would silently both win.
func (a *App) handleSetRoleRights(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Rights []Right `json:"rights"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	// SuperuserFrom is nil for a static admin token, which is the case Set
	// reads as "nobody's own role", and correctly: a script has no role to lock
	// itself out of, and it is the credential an operator who *has* locked
	// themselves out would use to undo it.
	set, err := a.roles.Set(r.Context(), r.PathValue("role"), in.Rights, SuperuserFrom(r.Context()))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	a.log.Info("role rights changed", "role", set.Role,
		"rights", set.Rights, "customized", set.Customized)
	Respond(w, http.StatusOK, set)
}

// handleResetRoleRights drops the store's override so the role tracks the
// engine's defaults again. A DELETE, because what it removes is the override
// and not the role.
func (a *App) handleResetRoleRights(w http.ResponseWriter, r *http.Request) {
	set, err := a.roles.Reset(r.Context(), r.PathValue("role"))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	a.log.Info("role rights reset to defaults", "role", set.Role)
	Respond(w, http.StatusOK, set)
}
