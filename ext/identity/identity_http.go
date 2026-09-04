package identity

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"

	gocommerce "github.com/misiki/gocommerce/core"
)

// mountRoutes wires the module's namespace. Public routes are the ones a
// shopper reaches; "auth" routes are the same namespace behind a shopper
// session; admin routes are under the engine's admin authentication as every
// module's are.
func (m *Module) mountRoutes(app *gocommerce.App) {
	app.HandleFunc("POST /x/identity/register", m.handleRegister)
	app.HandleFunc("POST /x/identity/login", m.handleLogin)
	app.HandleFunc("POST /x/identity/password-reset", m.handleRequestReset)
	app.HandleFunc("POST /x/identity/password-reset/confirm", m.handleConfirmReset)

	auth := m.requireSession
	app.HandleFunc("POST /x/identity/logout", auth(m.handleLogout))
	app.HandleFunc("POST /x/identity/refresh", auth(m.handleRefresh))
	app.HandleFunc("GET /x/identity/me", auth(m.handleMe))
	app.HandleFunc("PATCH /x/identity/me", auth(m.handleUpdateMe))
	app.HandleFunc("PUT /x/identity/me/password", auth(m.handleChangePassword))

	app.HandleFunc("GET /x/identity/me/addresses", auth(m.handleListAddresses))
	app.HandleFunc("POST /x/identity/me/addresses", auth(m.handleAddAddress))
	app.HandleFunc("GET /x/identity/me/addresses/{id}", auth(m.handleGetAddress))
	app.HandleFunc("PATCH /x/identity/me/addresses/{id}", auth(m.handleUpdateAddress))
	app.HandleFunc("DELETE /x/identity/me/addresses/{id}", auth(m.handleDeleteAddress))

	app.HandleFunc("GET /x/identity/me/orders", auth(m.handleListOrders))
	app.HandleFunc("POST /x/identity/me/orders", auth(m.handleClaimOrder))
	app.HandleFunc("GET /x/identity/me/orders/{number}", auth(m.handleGetOrder))

	app.HandleAdminFunc("GET /api/admin/x/identity/customers", m.handleAdminList)
	app.HandleAdminFunc("GET /api/admin/x/identity/customers/{id}", m.handleAdminGet)
	app.HandleAdminFunc("DELETE /api/admin/x/identity/customers/{id}", m.handleAdminDelete)
}

// ------------------------------------------------------------ the session

type ctxKey int

const ctxKeyCustomer ctxKey = iota

// CustomerFrom returns the signed-in shopper on a request that passed
// requireSession, or nil.
func CustomerFrom(ctx context.Context) *Customer {
	c, _ := ctx.Value(ctxKeyCustomer).(*Customer)
	return c
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// requireSession turns a bearer token into a customer or answers 401. It is
// the module's own middleware, not the engine's admin one: a shopper session
// must never satisfy an admin route, and an admin token must never read a
// shopper's address book.
func (m *Module) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			gocommerce.RespondError(w, r, gocommerce.ErrUnauthorized)
			return
		}
		c, ok := m.Resolve(r.Context(), token)
		if !ok {
			gocommerce.RespondError(w, r, gocommerce.ErrUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKeyCustomer, c)))
	}
}

func respondAuth(w http.ResponseWriter, status int, c *Customer, s *Session) {
	gocommerce.Respond(w, status, AuthResponse{Token: s.Token, ExpiresAt: s.ExpiresAt.UTC(), Record: c})
}

func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, gocommerce.Validationf("%s must be a positive integer", name)
	}
	return id, nil
}

// ------------------------------------------------------------------ public

func (m *Module) handleRegister(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Name     string `json:"name"`
		Phone    string `json:"phone"`
	}
	if err := gocommerce.DecodeJSON(w, r, &in); err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	c, sess, err := m.Signup(r.Context(), in.Email, in.Password, in.Name, in.Phone)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	respondAuth(w, http.StatusCreated, c, sess)
}

func (m *Module) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := gocommerce.DecodeJSON(w, r, &in); err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	c, sess, err := m.Authenticate(r.Context(), in.Email, in.Password, clientIP(r))
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	respondAuth(w, http.StatusOK, c, sess)
}

func (m *Module) handleRequestReset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	if err := gocommerce.DecodeJSON(w, r, &in); err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	if err := m.RequestReset(r.Context(), in.Email); err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	// Accepted, not OK: the answer is the same whether or not an email is on
	// its way, and 202 says exactly that.
	gocommerce.Respond(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (m *Module) handleConfirmReset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := gocommerce.DecodeJSON(w, r, &in); err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	c, sess, err := m.ConfirmReset(r.Context(), in.Token, in.Password)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	respondAuth(w, http.StatusOK, c, sess)
}

// ----------------------------------------------------------------- session

func (m *Module) handleLogout(w http.ResponseWriter, r *http.Request) {
	token, _ := bearerToken(r)
	if err := m.Revoke(r.Context(), token); err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Module) handleRefresh(w http.ResponseWriter, r *http.Request) {
	token, _ := bearerToken(r)
	c, sess, ok := m.Touch(r.Context(), token)
	if !ok {
		gocommerce.RespondError(w, r, gocommerce.ErrUnauthorized)
		return
	}
	respondAuth(w, http.StatusOK, c, sess)
}

func (m *Module) handleMe(w http.ResponseWriter, r *http.Request) {
	gocommerce.Respond(w, http.StatusOK, CustomerFrom(r.Context()))
}

func (m *Module) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email *string `json:"email"`
		Name  *string `json:"name"`
		Phone *string `json:"phone"`
	}
	if err := gocommerce.DecodeJSON(w, r, &in); err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	c, err := m.Update(r.Context(), CustomerFrom(r.Context()).ID, in.Email, in.Name, in.Phone)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	gocommerce.Respond(w, http.StatusOK, c)
}

func (m *Module) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CurrentPassword string `json:"current_password"`
		Password        string `json:"password"`
	}
	if err := gocommerce.DecodeJSON(w, r, &in); err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	c, sess, err := m.ChangePassword(r.Context(), CustomerFrom(r.Context()).ID, in.CurrentPassword, in.Password)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	respondAuth(w, http.StatusOK, c, sess)
}

// --------------------------------------------------------------- addresses

func (m *Module) handleListAddresses(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := gocommerce.Page(r)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	list, total, err := m.Addresses(r.Context(), CustomerFrom(r.Context()).ID, limit, offset)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	gocommerce.RespondList(w, list, gocommerce.ListMeta{Total: total, Limit: limit, Offset: offset})
}

func (m *Module) handleAddAddress(w http.ResponseWriter, r *http.Request) {
	var in AddressInput
	if err := gocommerce.DecodeJSON(w, r, &in); err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	a, err := m.AddAddress(r.Context(), CustomerFrom(r.Context()).ID, in)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	gocommerce.Respond(w, http.StatusCreated, a)
}

func (m *Module) handleGetAddress(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	a, err := m.Address(r.Context(), CustomerFrom(r.Context()).ID, id)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	gocommerce.Respond(w, http.StatusOK, a)
}

func (m *Module) handleUpdateAddress(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	var in AddressInput
	if err := gocommerce.DecodeJSON(w, r, &in); err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	a, err := m.UpdateAddress(r.Context(), CustomerFrom(r.Context()).ID, id, in)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	gocommerce.Respond(w, http.StatusOK, a)
}

func (m *Module) handleDeleteAddress(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	if err := m.DeleteAddress(r.Context(), CustomerFrom(r.Context()).ID, id); err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------------ orders

func (m *Module) handleListOrders(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := gocommerce.Page(r)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	list, total, err := m.Orders(r.Context(), CustomerFrom(r.Context()).ID, limit, offset)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	gocommerce.RespondList(w, list, gocommerce.ListMeta{Total: total, Limit: limit, Offset: offset})
}

func (m *Module) handleClaimOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Number string `json:"number"`
		Token  string `json:"token"`
	}
	if err := gocommerce.DecodeJSON(w, r, &in); err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	o, created, err := m.ClaimOrder(r.Context(), CustomerFrom(r.Context()).ID, in.Number, in.Token)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	gocommerce.Respond(w, status, o)
}

func (m *Module) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	o, err := m.Order(r.Context(), CustomerFrom(r.Context()).ID, r.PathValue("number"))
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	gocommerce.Respond(w, http.StatusOK, o)
}

// ------------------------------------------------------------------- admin

func (m *Module) handleAdminList(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := gocommerce.Page(r)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	list, total, err := m.List(r.Context(), CustomerQuery{
		Search: r.URL.Query().Get("q"), Limit: limit, Offset: offset,
	})
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	gocommerce.RespondList(w, list, gocommerce.ListMeta{Total: total, Limit: limit, Offset: offset})
}

func (m *Module) handleAdminGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	c, err := m.customerByID(r.Context(), id)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	gocommerce.Respond(w, http.StatusOK, c)
}

func (m *Module) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	if err := m.Delete(r.Context(), id); err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
