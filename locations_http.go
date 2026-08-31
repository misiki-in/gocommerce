package gocommerce

import "net/http"

func (a *App) mountLocationRoutes() {
	// Admin only. A storefront is told what it can have, not where it is: a
	// shopper who learns which warehouse is short has learned something about
	// the business, not about their order.
	//
	// Reading comes with the catalog, because "where is this" is a question
	// about a product. Writing is locations.write — opening and closing places
	// redirects every future reservation in the store, which is a different
	// kind of act from adjusting a count.
	a.HandleAdminFunc("GET /api/admin/locations", a.handleListLocations, RightLocationsRead)
	a.HandleAdminFunc("POST /api/admin/locations", a.handleCreateLocation, RightLocationsWrite)
	a.HandleAdminFunc("GET /api/admin/locations/{id}", a.handleGetLocation, RightLocationsRead)
	a.HandleAdminFunc("PATCH /api/admin/locations/{id}", a.handleUpdateLocation, RightLocationsWrite)
	a.HandleAdminFunc("DELETE /api/admin/locations/{id}", a.handleDeleteLocation, RightLocationsWrite)
	a.HandleAdminFunc("POST /api/admin/locations/{id}/default", a.handleSetDefaultLocation, RightLocationsWrite)

	// Where one variant's stock is, and moving it. Both are inventory.write
	// rather than locations.write: a transfer changes counts, which is exactly
	// what the person doing the stock take is trusted to do.
	a.HandleAdminFunc("GET /api/admin/variants/{id}/stock", a.handleVariantStock, RightInventoryRead)
	a.HandleAdminFunc("POST /api/admin/variants/{id}/stock/transfer", a.handleTransferStock, RightInventoryWrite)
}

func (a *App) handleListLocations(w http.ResponseWriter, r *http.Request) {
	list, err := a.locations.List(r.Context())
	if err != nil {
		RespondError(w, r, err)
		return
	}
	RespondList(w, list, ListMeta{Total: len(list), Limit: len(list), Offset: 0})
}

func (a *App) handleGetLocation(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	l, err := a.locations.Get(r.Context(), id)
	respondOr(w, r, l, err)
}

func (a *App) handleCreateLocation(w http.ResponseWriter, r *http.Request) {
	var in LocationInput
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	l, err := a.locations.Create(r.Context(), in)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusCreated, l)
}

func (a *App) handleUpdateLocation(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var patch LocationPatch
	if err := DecodeJSON(w, r, &patch); err != nil {
		RespondError(w, r, err)
		return
	}
	l, err := a.locations.Update(r.Context(), id, patch)
	respondOr(w, r, l, err)
}

func (a *App) handleDeleteLocation(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	if err := a.locations.Delete(r.Context(), id); err != nil {
		RespondError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleSetDefaultLocation(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	l, err := a.locations.SetDefault(r.Context(), id)
	respondOr(w, r, l, err)
}

func (a *App) handleVariantStock(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	rows, err := a.inventory.ByLocation(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	RespondList(w, rows, ListMeta{Total: len(rows), Limit: len(rows), Offset: 0})
}

func (a *App) handleTransferStock(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var in struct {
		// From has no default: moving stock out of "wherever" is not a thing
		// anyone means to do, and guessing would take units off a shelf the
		// operator never named. To may be omitted for the default location.
		From     int64 `json:"from_location_id"`
		To       int64 `json:"to_location_id"`
		Quantity int   `json:"quantity"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	if in.From == 0 {
		RespondError(w, r, Validationf("from_location_id is required"))
		return
	}
	v, err := a.inventory.Move(r.Context(), id, in.From, in.To, in.Quantity)
	respondOr(w, r, v, err)
}
