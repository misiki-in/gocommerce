package gocommerce

import (
	"fmt"
	"net/http"
	"time"
)

func (a *App) mountTransferRoutes() {
	a.HandleAdminFunc("GET /api/admin/export/admin-products", a.handleExportProducts, RightSettingsWrite)
	a.HandleAdminFunc("POST /api/admin/import/products", a.handleImportProducts, RightSettingsWrite)
	a.HandleAdminFunc("GET /api/admin/export/admin-orders", a.handleExportOrders, RightSettingsWrite)
	a.HandleAdminFunc("POST /api/admin/import/orders", a.handleImportOrders, RightSettingsWrite)
}

func (a *App) handleExportProducts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="products-%s.csv"`, time.Now().UTC().Format("2006-01-02")))
	if err := a.transfer.ExportProducts(r.Context(), w); err != nil {
		// The response has already begun, so the status line is spent. Log it
		// and let the truncated file be the signal — pretending it succeeded
		// would be worse.
		a.log.Error("product export failed midway", "error", err)
	}
}

func (a *App) handleExportOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := OrderQuery{Status: q.Get("status")}
	var err error
	if query.From, err = parseDate(q.Get("from")); err != nil {
		RespondError(w, r, err)
		return
	}
	if query.To, err = parseDate(q.Get("to")); err != nil {
		RespondError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="orders-%s.csv"`, time.Now().UTC().Format("2006-01-02")))
	if err := a.transfer.ExportOrders(r.Context(), w, query); err != nil {
		a.log.Error("order export failed midway", "error", err)
	}
}

func (a *App) handleImportProducts(w http.ResponseWriter, r *http.Request) {
	body := limitedBody(w, r, maxUploadBytes)
	result, err := a.transfer.ImportProducts(r.Context(), body, boolParam(r, "dry_run"))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, result)
}

func (a *App) handleImportOrders(w http.ResponseWriter, r *http.Request) {
	body := limitedBody(w, r, maxUploadBytes)
	// Events are off unless explicitly asked for: importing history must not
	// email five thousand people about orders they placed last year.
	result, err := a.transfer.ImportOrders(r.Context(), body,
		boolParam(r, "dry_run"), boolParam(r, "fire_events"))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	a.nudgeOutbox()
	Respond(w, http.StatusOK, result)
}

func boolParam(r *http.Request, name string) bool {
	switch r.URL.Query().Get(name) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
