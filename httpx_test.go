package gocommerce

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIErrorIs(t *testing.T) {
	t.Parallel()

	// errors.Is compares by code, so a call site is free to write a specific
	// message without breaking a caller's classification.
	if !errors.Is(NotFoundf("product %d does not exist", 7), ErrNotFound) {
		t.Error("a specific not-found error should satisfy errors.Is(err, ErrNotFound)")
	}
	if errors.Is(Conflictf("price changed"), ErrNotFound) {
		t.Error("a conflict must not be classified as not-found")
	}

	wrapped := errors.New("connection reset")
	internal := Internalf(wrapped, "could not load order")
	if !errors.Is(internal, wrapped) {
		t.Error("Internalf should unwrap to the underlying error")
	}
}

func TestRespondErrorHidesInternalDetail(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/products", nil)
	secret := "pq: relation \"orders\" does not exist"
	RespondError(rec, req, Internalf(errors.New(secret), "loading %s failed", "orders"))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, secret) || strings.Contains(body, "orders") {
		t.Errorf("internal detail leaked to the client: %s", body)
	}

	var got envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil || got.Error.Code != "internal_error" {
		t.Errorf("error envelope = %+v, want code internal_error", got.Error)
	}
}

func TestRespondErrorPassesClientDetail(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/checkout/cod", nil)
	err := Conflictf("cart is stale").WithDetails([]map[string]any{
		{"variant_id": 42, "reason": "price_changed"},
	})
	RespondError(rec, req, err)

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"cart is stale", "price_changed", `"code":"conflict"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %s missing %q", body, want)
		}
	}
}

func TestPage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
		wantErr    bool
	}{
		{name: "defaults", query: "", wantLimit: DefaultLimit},
		{name: "limit and offset", query: "?limit=10&offset=20", wantLimit: 10, wantOffset: 20},
		{name: "limit at the cap", query: "?limit=200", wantLimit: MaxLimit},
		{name: "limit above the cap", query: "?limit=201", wantErr: true},
		{name: "limit of zero", query: "?limit=0", wantErr: true},
		{name: "negative limit", query: "?limit=-1", wantErr: true},
		{name: "non-numeric limit", query: "?limit=abc", wantErr: true},
		{name: "negative offset", query: "?offset=-1", wantErr: true},

		// Pages are 1-based, because that is what "page 3 of 12" means.
		{name: "first page", query: "?page=1", wantLimit: DefaultLimit, wantOffset: 0},
		{name: "second page at the default limit", query: "?page=2", wantLimit: DefaultLimit, wantOffset: 50},
		{name: "page with an explicit limit", query: "?page=3&limit=20", wantLimit: 20, wantOffset: 40},
		{name: "limit order does not matter", query: "?limit=20&page=3", wantLimit: 20, wantOffset: 40},
		{name: "page zero", query: "?page=0", wantErr: true},
		{name: "negative page", query: "?page=-2", wantErr: true},
		{name: "non-numeric page", query: "?page=two", wantErr: true},

		{
			name: "page wins over offset",
			// Both given: honour the page. It is the more specific intent, and
			// obeying the offset would land the client somewhere it did not ask
			// for.
			query: "?limit=10&offset=999&page=2", wantLimit: 10, wantOffset: 10,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/products"+tc.query, nil)
			limit, offset, err := Page(req)
			switch {
			case tc.wantErr:
				if err == nil {
					t.Errorf("Page(%q) expected an error", tc.query)
				}
			case err != nil:
				t.Errorf("Page(%q) unexpected error: %v", tc.query, err)
			case limit != tc.wantLimit || offset != tc.wantOffset:
				t.Errorf("Page(%q) = (%d, %d), want (%d, %d)",
					tc.query, limit, offset, tc.wantLimit, tc.wantOffset)
			}
		})
	}
}

func TestListMetaDerivesPage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		meta           ListMeta
		wantPage       int
		wantTotalPages int
	}{
		{name: "first page", meta: ListMeta{Total: 95, Limit: 20, Offset: 0}, wantPage: 1, wantTotalPages: 5},
		{name: "third page", meta: ListMeta{Total: 95, Limit: 20, Offset: 40}, wantPage: 3, wantTotalPages: 5},
		{name: "last partial page", meta: ListMeta{Total: 95, Limit: 20, Offset: 80}, wantPage: 5, wantTotalPages: 5},
		{name: "exactly full", meta: ListMeta{Total: 100, Limit: 20, Offset: 0}, wantPage: 1, wantTotalPages: 5},
		{name: "nothing to show", meta: ListMeta{Total: 0, Limit: 20, Offset: 0}, wantPage: 1, wantTotalPages: 0},
		{
			name: "an offset that is not a whole page",
			// Still has an answer: the page the offset falls inside.
			meta: ListMeta{Total: 95, Limit: 20, Offset: 25}, wantPage: 2, wantTotalPages: 5,
		},
		{name: "missing limit falls back to the default", meta: ListMeta{Total: 10}, wantPage: 1, wantTotalPages: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			meta := tc.meta
			meta.derive()
			if meta.Page != tc.wantPage || meta.TotalPages != tc.wantTotalPages {
				t.Errorf("derive(%+v) gave page %d of %d, want page %d of %d",
					tc.meta, meta.Page, meta.TotalPages, tc.wantPage, tc.wantTotalPages)
			}
		})
	}
}

func TestBearerToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		header string
		want   string
		ok     bool
	}{
		{header: "Bearer abc", want: "abc", ok: true},
		{header: "bearer abc", want: "abc", ok: true}, // scheme is case-insensitive
		{header: "Bearer  abc ", want: "abc", ok: true},
		{header: "", ok: false},
		{header: "Basic abc", ok: false},
		{header: "Bearer", ok: false},
	}

	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		got, ok := bearerToken(req)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("bearerToken(%q) = (%q, %v), want (%q, %v)", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

func TestValidAdminToken(t *testing.T) {
	t.Parallel()

	a := &App{cfg: Config{AdminTokens: []string{"first-token", "second-token"}}}

	// Several tokens are accepted at once so one can be rotated out without
	// a window where no token works.
	for _, token := range []string{"first-token", "second-token"} {
		if !a.validAdminToken(token) {
			t.Errorf("token %q should be accepted", token)
		}
	}
	for _, token := range []string{"", "first", "first-token ", "third-token"} {
		if a.validAdminToken(token) {
			t.Errorf("token %q should be rejected", token)
		}
	}
}

func TestDecodeJSON(t *testing.T) {
	t.Parallel()

	type payload struct {
		Name string `json:"name"`
	}

	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "valid", body: `{"name":"widget"}`},
		{name: "unknown field is rejected", body: `{"nmae":"widget"}`, wantErr: "malformed JSON"},
		{name: "malformed", body: `{`, wantErr: "malformed JSON"},
		{name: "trailing content", body: `{"name":"a"} {"name":"b"}`, wantErr: "single JSON value"},
		{name: "empty body", body: ``, wantErr: "malformed JSON"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			var v payload
			err := DecodeJSON(httptest.NewRecorder(), req, &v)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
