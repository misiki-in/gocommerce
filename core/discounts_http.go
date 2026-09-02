package gocommerce

import (
	"net/http"
	"strings"
)

func (a *App) mountDiscountRoutes() {
	// The public surface is two routes on the cart. A storefront never reads
	// the discount table: it offers a code and is told what it is worth, which
	// is the only question it has.
	a.HandleFunc("PUT /api/carts/{token}/discount", a.handleSetCartDiscount)
	a.HandleFunc("DELETE /api/carts/{token}/discount", a.handleClearCartDiscount)

	// A promotion is catalog work: a manager runs one, staff may look at it.
	a.HandleAdminFunc("GET /api/admin/discounts", a.handleListDiscounts, RightDiscountsRead)
	a.HandleAdminFunc("POST /api/admin/discounts", a.handleCreateDiscount, RightDiscountsWrite)
	a.HandleAdminFunc("GET /api/admin/discounts/{id}", a.handleGetDiscount, RightDiscountsRead)
	a.HandleAdminFunc("PATCH /api/admin/discounts/{id}", a.handleUpdateDiscount, RightDiscountsWrite)
	a.HandleAdminFunc("DELETE /api/admin/discounts/{id}", a.handleDeleteDiscount, RightDiscountsWrite)
}

// -------------------------------------------------------------------- public

// handleSetCartDiscount attaches a code to a cart and reports what it is worth.
//
// The amount it answers with is a preview, not a promise: nothing is consumed
// here, and checkout decides again under its own lock. A code that is valid now
// and exhausted in ten minutes is refused then, which is the only honest place
// to refuse it.
func (a *App) handleSetCartDiscount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
		// Optional: `once_per_email` cannot be previewed without it, and a
		// storefront usually knows the address by the time it asks.
		Email string `json:"email"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	code := strings.TrimSpace(in.Code)
	if code == "" {
		RespondError(w, r, Validationf("a code is required"))
		return
	}

	cart, err := a.carts.GetByToken(r.Context(), r.PathValue("token"))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	applied, err := a.discounts.Preview(r.Context(), code, in.Email, cart.Subtotal.AmountMinor)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	if _, err := a.db.ExecContext(r.Context(),
		`UPDATE carts SET discount_code = $2, updated_at = now() WHERE token = $1`,
		r.PathValue("token"), code); err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, applied)
}

func (a *App) handleClearCartDiscount(w http.ResponseWriter, r *http.Request) {
	res, err := a.db.ExecContext(r.Context(),
		`UPDATE carts SET discount_code = '', updated_at = now() WHERE token = $1`,
		r.PathValue("token"))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	if n, err := res.RowsAffected(); err != nil {
		RespondError(w, r, err)
		return
	} else if n == 0 {
		RespondError(w, r, NotFoundf("cart not found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --------------------------------------------------------------------- admin

func (a *App) handleListDiscounts(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := Page(r)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	q := DiscountQuery{Search: r.URL.Query().Get("q"), Limit: limit, Offset: offset}
	if v := r.URL.Query().Get("active"); v != "" {
		active := v == "1" || strings.EqualFold(v, "true")
		q.Active = &active
	}
	list, total, err := a.discounts.List(r.Context(), q)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	RespondList(w, list, ListMeta{Total: total, Limit: limit, Offset: offset})
}

func (a *App) handleGetDiscount(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	d, err := a.discounts.Get(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, d)
}

func (a *App) handleCreateDiscount(w http.ResponseWriter, r *http.Request) {
	var in DiscountInput
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	d, err := a.discounts.Create(r.Context(), in)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusCreated, d)
}

func (a *App) handleUpdateDiscount(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var patch DiscountPatch
	if err := DecodeJSON(w, r, &patch); err != nil {
		RespondError(w, r, err)
		return
	}
	d, err := a.discounts.Update(r.Context(), id, patch)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, d)
}

func (a *App) handleDeleteDiscount(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	if err := a.discounts.Delete(r.Context(), id); err != nil {
		RespondError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
