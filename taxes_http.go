package gocommerce

import "net/http"

func (a *App) mountTaxRoutes() {
	// Admin only. A storefront never reads the rate table: it is told what an
	// order costs, which already has the tax in the answer.
	//
	// Reading a rate comes with the catalog, because it is what a product is
	// charged at. Writing one is settings.write: a tax rate is a claim about
	// the law, it is what an invoice is defended with, and it is not something
	// the person running this week's promotion should be able to change.
	a.HandleAdminFunc("GET /api/admin/tax-rates", a.handleListTaxRates, RightCatalogRead)
	a.HandleAdminFunc("POST /api/admin/tax-rates", a.handleCreateTaxRate, RightSettingsWrite)
	a.HandleAdminFunc("GET /api/admin/tax-rates/{id}", a.handleGetTaxRate, RightCatalogRead)
	a.HandleAdminFunc("PATCH /api/admin/tax-rates/{id}", a.handleUpdateTaxRate, RightSettingsWrite)
	a.HandleAdminFunc("DELETE /api/admin/tax-rates/{id}", a.handleDeleteTaxRate, RightSettingsWrite)
}

func (a *App) handleListTaxRates(w http.ResponseWriter, r *http.Request) {
	list, err := a.taxes.List(r.Context())
	if err != nil {
		RespondError(w, r, err)
		return
	}
	RespondList(w, list, ListMeta{Total: len(list), Limit: len(list), Offset: 0})
}

func (a *App) handleGetTaxRate(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	t, err := a.taxes.Get(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, t)
}

func (a *App) handleCreateTaxRate(w http.ResponseWriter, r *http.Request) {
	var in TaxRateInput
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	t, err := a.taxes.Create(r.Context(), in)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusCreated, t)
}

func (a *App) handleUpdateTaxRate(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var patch TaxRatePatch
	if err := DecodeJSON(w, r, &patch); err != nil {
		RespondError(w, r, err)
		return
	}
	t, err := a.taxes.Update(r.Context(), id, patch)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, t)
}

func (a *App) handleDeleteTaxRate(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	if err := a.taxes.Delete(r.Context(), id); err != nil {
		RespondError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
