package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	gocommerce "github.com/misiki/gocommerce/core"
	"github.com/misiki/gocommerce/gctest"
)

// The hashes these tests make are thrown away with the schema; there is
// nothing to protect and no reason to spend 600k iterations per sign-in.
func init() { pbkdf2Iterations = 1_000 }

// capture is a notifier that remembers what it was asked to send, so a test
// can read the reset token out of the "email".
type capture struct {
	mu   sync.Mutex
	sent []gocommerce.Notification
}

func (c *capture) Notify(_ context.Context, n gocommerce.Notification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, n)
	return nil
}

func (c *capture) last() *gocommerce.Notification {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) == 0 {
		return nil
	}
	return &c.sent[len(c.sent)-1]
}

func newApp(t *testing.T) (*gocommerce.App, *capture) {
	t.Helper()
	mail := &capture{}
	app := gctest.New(t, New(Config{Notifier: mail}))
	return app, mail
}

// as sends a request carrying a shopper session.
func as(t *testing.T, app *gocommerce.App, token, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec
}

func register(t *testing.T, app *gocommerce.App, email string) AuthResponse {
	t.Helper()
	rec := gctest.Request(t, app, http.MethodPost, "/x/identity/register", map[string]any{
		"email": email, "password": "correct horse", "name": "Ada Lovelace", "phone": "+441234",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("register %s = %d: %s", email, rec.Code, rec.Body)
	}
	var auth AuthResponse
	gctest.DecodeData(t, rec, &auth)
	if auth.Token == "" || auth.Record == nil || auth.Record.Email != email {
		t.Fatalf("register returned %+v", auth)
	}
	return auth
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (%s)", err, rec.Body)
	}
	return env.Error.Code
}

func TestRegisterLoginAndLogout(t *testing.T) {
	app, _ := newApp(t)
	auth := register(t, app, "ada@example.com")

	// The token is a credential.
	rec := as(t, app, auth.Token, http.MethodGet, "/x/identity/me", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d: %s", rec.Code, rec.Body)
	}
	var me Customer
	gctest.DecodeData(t, rec, &me)
	if me.Name != "Ada Lovelace" || me.Phone != "+441234" {
		t.Errorf("me = %+v", me)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("password")) {
		t.Error("the password hash leaked into the response")
	}

	// No token is no session; a made-up one is no session.
	if rec := as(t, app, "", http.MethodGet, "/x/identity/me", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("me without a token = %d, want 401", rec.Code)
	}
	if rec := as(t, app, "nope", http.MethodGet, "/x/identity/me", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("me with a bad token = %d, want 401", rec.Code)
	}
	// An admin token is not a shopper.
	if rec := gctest.AdminRequest(t, app, http.MethodGet, "/x/identity/me", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("me with the admin token = %d, want 401", rec.Code)
	}

	// Sign in, any capitalisation.
	rec = gctest.Request(t, app, http.MethodPost, "/x/identity/login", map[string]any{
		"email": "Ada@Example.com", "password": "correct horse",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body)
	}
	var login AuthResponse
	gctest.DecodeData(t, rec, &login)
	if login.Token == auth.Token {
		t.Error("login reused the registration token")
	}

	// Wrong password and unknown email are indistinguishable.
	wrong := gctest.Request(t, app, http.MethodPost, "/x/identity/login", map[string]any{
		"email": "ada@example.com", "password": "wrong",
	})
	unknown := gctest.Request(t, app, http.MethodPost, "/x/identity/login", map[string]any{
		"email": "nobody@example.com", "password": "correct horse",
	})
	if wrong.Code != http.StatusBadRequest || unknown.Code != http.StatusBadRequest {
		t.Fatalf("wrong = %d, unknown = %d, want 400 for both", wrong.Code, unknown.Code)
	}
	if errorCode(t, wrong) != "invalid_credentials" || errorCode(t, unknown) != "invalid_credentials" {
		t.Errorf("codes = %q / %q", errorCode(t, wrong), errorCode(t, unknown))
	}

	// Refresh keeps the token; logout ends exactly this session.
	if rec := as(t, app, login.Token, http.MethodPost, "/x/identity/refresh", nil); rec.Code != http.StatusOK {
		t.Errorf("refresh = %d: %s", rec.Code, rec.Body)
	}
	if rec := as(t, app, login.Token, http.MethodPost, "/x/identity/logout", nil); rec.Code != http.StatusNoContent {
		t.Errorf("logout = %d: %s", rec.Code, rec.Body)
	}
	if rec := as(t, app, login.Token, http.MethodGet, "/x/identity/me", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("me after logout = %d, want 401", rec.Code)
	}
	if rec := as(t, app, auth.Token, http.MethodGet, "/x/identity/me", nil); rec.Code != http.StatusOK {
		t.Errorf("the other session should survive a logout: %d", rec.Code)
	}
}

func TestRegisterValidates(t *testing.T) {
	app, _ := newApp(t)
	post := func(body map[string]any) *httptest.ResponseRecorder {
		return gctest.Request(t, app, http.MethodPost, "/x/identity/register", body)
	}
	if rec := post(map[string]any{"email": "ada@example.com", "password": "short"}); rec.Code != http.StatusBadRequest {
		t.Errorf("short password = %d, want 400", rec.Code)
	}
	if rec := post(map[string]any{"email": "not an email", "password": "correct horse"}); rec.Code != http.StatusBadRequest {
		t.Errorf("bad email = %d, want 400", rec.Code)
	}
	register(t, app, "ada@example.com")
	if rec := post(map[string]any{"email": "ADA@example.com", "password": "correct horse"}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate email = %d, want 409", rec.Code)
	}
}

func TestProfileAndPasswordChange(t *testing.T) {
	app, _ := newApp(t)
	auth := register(t, app, "ada@example.com")
	other := gctest.Request(t, app, http.MethodPost, "/x/identity/login", map[string]any{
		"email": "ada@example.com", "password": "correct horse",
	})
	var second AuthResponse
	gctest.DecodeData(t, other, &second)

	rec := as(t, app, auth.Token, http.MethodPatch, "/x/identity/me", map[string]any{"name": "Countess", "phone": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch me = %d: %s", rec.Code, rec.Body)
	}
	var me Customer
	gctest.DecodeData(t, rec, &me)
	if me.Name != "Countess" || me.Phone != "" || me.Email != "ada@example.com" {
		t.Errorf("me = %+v", me)
	}

	if rec := as(t, app, auth.Token, http.MethodPut, "/x/identity/me/password", map[string]any{
		"current_password": "wrong", "password": "battery staple",
	}); rec.Code != http.StatusBadRequest {
		t.Errorf("change with the wrong current password = %d, want 400", rec.Code)
	}
	rec = as(t, app, auth.Token, http.MethodPut, "/x/identity/me/password", map[string]any{
		"current_password": "correct horse", "password": "battery staple",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("change password = %d: %s", rec.Code, rec.Body)
	}
	var fresh AuthResponse
	gctest.DecodeData(t, rec, &fresh)

	// Every session that predates the change is gone, including the caller's
	// old one; the response carried its replacement.
	if rec := as(t, app, auth.Token, http.MethodGet, "/x/identity/me", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("old session after password change = %d, want 401", rec.Code)
	}
	if rec := as(t, app, second.Token, http.MethodGet, "/x/identity/me", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("other session after password change = %d, want 401", rec.Code)
	}
	if rec := as(t, app, fresh.Token, http.MethodGet, "/x/identity/me", nil); rec.Code != http.StatusOK {
		t.Errorf("replacement session = %d, want 200", rec.Code)
	}
	if rec := gctest.Request(t, app, http.MethodPost, "/x/identity/login", map[string]any{
		"email": "ada@example.com", "password": "battery staple",
	}); rec.Code != http.StatusOK {
		t.Errorf("login with the new password = %d: %s", rec.Code, rec.Body)
	}
}

func TestPasswordReset(t *testing.T) {
	app, mail := newApp(t)
	auth := register(t, app, "ada@example.com")

	// An unknown address gets the same answer and no email.
	rec := gctest.Request(t, app, http.MethodPost, "/x/identity/password-reset", map[string]any{"email": "nobody@example.com"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("reset for an unknown email = %d, want 202", rec.Code)
	}
	if mail.last() != nil {
		t.Fatal("an unknown address must not produce an email")
	}

	rec = gctest.Request(t, app, http.MethodPost, "/x/identity/password-reset", map[string]any{"email": "ADA@example.com"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("reset = %d: %s", rec.Code, rec.Body)
	}
	note := mail.last()
	if note == nil {
		t.Fatal("no reset email was sent")
	}
	if note.Event != EventPasswordReset || note.Channel != gocommerce.ChannelEmail || note.To != "ada@example.com" {
		t.Errorf("notification = %+v", note)
	}
	token := note.Data["reset_token"]
	if token == "" {
		t.Fatalf("no reset_token in %v", note.Data)
	}
	if _, ok := note.Data["reset_url"]; ok {
		t.Error("reset_url should be absent when no ResetURL is configured")
	}

	if rec := gctest.Request(t, app, http.MethodPost, "/x/identity/password-reset/confirm", map[string]any{
		"token": "bogus", "password": "battery staple",
	}); rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_token" {
		t.Errorf("confirm with a bad token = %d %s", rec.Code, rec.Body)
	}
	rec = gctest.Request(t, app, http.MethodPost, "/x/identity/password-reset/confirm", map[string]any{
		"token": token, "password": "battery staple",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm = %d: %s", rec.Code, rec.Body)
	}
	var fresh AuthResponse
	gctest.DecodeData(t, rec, &fresh)
	if rec := as(t, app, fresh.Token, http.MethodGet, "/x/identity/me", nil); rec.Code != http.StatusOK {
		t.Errorf("session from the reset = %d", rec.Code)
	}
	// The reset ended every session that existed before it, and a token is
	// single-use.
	if rec := as(t, app, auth.Token, http.MethodGet, "/x/identity/me", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("session from before the reset = %d, want 401", rec.Code)
	}
	if rec := gctest.Request(t, app, http.MethodPost, "/x/identity/password-reset/confirm", map[string]any{
		"token": token, "password": "another one",
	}); rec.Code != http.StatusBadRequest {
		t.Errorf("reused token = %d, want 400", rec.Code)
	}
}

func TestResetURLIsTheStoresNotTheClients(t *testing.T) {
	mail := &capture{}
	app := gctest.New(t, New(Config{
		Notifier: mail, ResetURL: "https://shop.example/reset?token={token}",
	}))
	register(t, app, "ada@example.com")
	gctest.Request(t, app, http.MethodPost, "/x/identity/password-reset", map[string]any{"email": "ada@example.com"})
	note := mail.last()
	if note == nil {
		t.Fatal("no reset email")
	}
	want := "https://shop.example/reset?token=" + note.Data["reset_token"]
	if note.Data["reset_url"] != want {
		t.Errorf("reset_url = %q, want %q", note.Data["reset_url"], want)
	}
}

func TestAddressBook(t *testing.T) {
	app, _ := newApp(t)
	ada := register(t, app, "ada@example.com")
	bob := register(t, app, "bob@example.com")

	// A book needs a deliverable address.
	if rec := as(t, app, ada.Token, http.MethodPost, "/x/identity/me/addresses", map[string]any{
		"line1": "1 Analytical Way",
	}); rec.Code != http.StatusBadRequest {
		t.Errorf("address without city/postcode/country = %d, want 400", rec.Code)
	}

	save := func(token, label string) Address {
		rec := as(t, app, token, http.MethodPost, "/x/identity/me/addresses", map[string]any{
			"label": label, "name": "Ada", "line1": "1 Analytical Way", "city": "London",
			"postal_code": "N1 1AA", "country": "GB",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("save %s = %d: %s", label, rec.Code, rec.Body)
		}
		var a Address
		gctest.DecodeData(t, rec, &a)
		return a
	}
	home := save(ada.Token, "Home")
	if !home.IsDefault {
		t.Error("the first address should be the default")
	}
	office := save(ada.Token, "Office")
	if office.IsDefault {
		t.Error("a second address should not steal the default")
	}

	// Moving the default un-defaults the other.
	rec := as(t, app, ada.Token, http.MethodPatch, "/x/identity/me/addresses/"+strconv.FormatInt(office.ID, 10),
		map[string]any{"is_default": true, "label": "Work"})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body)
	}
	rec = as(t, app, ada.Token, http.MethodGet, "/x/identity/me/addresses", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body)
	}
	var list []Address
	gctest.DecodeData(t, rec, &list)
	if len(list) != 2 || !list[0].IsDefault || list[0].Label != "Work" || list[1].IsDefault {
		t.Errorf("list = %+v", list)
	}

	// Bob's book is Bob's.
	if rec := as(t, app, bob.Token, http.MethodGet, "/x/identity/me/addresses/"+strconv.FormatInt(home.ID, 10), nil); rec.Code != http.StatusNotFound {
		t.Errorf("another account reading the address = %d, want 404", rec.Code)
	}
	if rec := as(t, app, bob.Token, http.MethodDelete, "/x/identity/me/addresses/"+strconv.FormatInt(home.ID, 10), nil); rec.Code != http.StatusNotFound {
		t.Errorf("another account deleting the address = %d, want 404", rec.Code)
	}

	// Deleting the default hands it to what is left.
	if rec := as(t, app, ada.Token, http.MethodDelete, "/x/identity/me/addresses/"+strconv.FormatInt(office.ID, 10), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body)
	}
	rec = as(t, app, ada.Token, http.MethodGet, "/x/identity/me/addresses/"+strconv.FormatInt(home.ID, 10), nil)
	var remaining Address
	gctest.DecodeData(t, rec, &remaining)
	if !remaining.IsDefault {
		t.Error("the remaining address should have become the default")
	}
}

func TestOrderHistoryIsByClaim(t *testing.T) {
	app, _ := newApp(t)
	ada := register(t, app, "ada@example.com")
	bob := register(t, app, "bob@example.com")

	// Checkout is unchanged: a guest places the order, and holds its token.
	placed := gctest.PlaceOrder(t, app, "cod")
	number, token := placed.Order.Number, placed.Order.AccessToken
	if token == "" {
		t.Fatal("checkout returned no access token")
	}

	// Nothing yet.
	rec := as(t, app, ada.Token, http.MethodGet, "/x/identity/me/orders", nil)
	var none []gocommerce.Order
	gctest.DecodeData(t, rec, &none)
	if len(none) != 0 {
		t.Fatalf("history before any claim = %d orders", len(none))
	}

	// The number alone proves nothing.
	if rec := as(t, app, ada.Token, http.MethodPost, "/x/identity/me/orders", map[string]any{
		"number": number, "token": "guess",
	}); rec.Code != http.StatusNotFound {
		t.Errorf("claim with a wrong token = %d, want 404", rec.Code)
	}
	rec = as(t, app, ada.Token, http.MethodPost, "/x/identity/me/orders", map[string]any{"number": number, "token": token})
	if rec.Code != http.StatusCreated {
		t.Fatalf("claim = %d: %s", rec.Code, rec.Body)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("access_token")) {
		t.Error("the access token must not come back in the history")
	}
	// Claiming again is fine; claiming from another account is not.
	if rec := as(t, app, ada.Token, http.MethodPost, "/x/identity/me/orders", map[string]any{"number": number, "token": token}); rec.Code != http.StatusOK {
		t.Errorf("second claim = %d, want 200", rec.Code)
	}
	if rec := as(t, app, bob.Token, http.MethodPost, "/x/identity/me/orders", map[string]any{"number": number, "token": token}); rec.Code != http.StatusConflict {
		t.Errorf("claim from another account = %d, want 409", rec.Code)
	}

	rec = as(t, app, ada.Token, http.MethodGet, "/x/identity/me/orders", nil)
	var history []gocommerce.Order
	gctest.DecodeData(t, rec, &history)
	if len(history) != 1 || history[0].Number != number {
		t.Errorf("history = %+v", history)
	}
	if rec := as(t, app, ada.Token, http.MethodGet, "/x/identity/me/orders/"+number, nil); rec.Code != http.StatusOK {
		t.Errorf("get from history = %d: %s", rec.Code, rec.Body)
	}
	if rec := as(t, app, bob.Token, http.MethodGet, "/x/identity/me/orders/"+number, nil); rec.Code != http.StatusNotFound {
		t.Errorf("another account reading the order = %d, want 404", rec.Code)
	}
}

func TestAdminSeesAccountsAndCanDeleteOne(t *testing.T) {
	app, _ := newApp(t)
	ada := register(t, app, "ada@example.com")
	register(t, app, "bob@example.com")

	rec := gctest.AdminRequest(t, app, http.MethodGet, "/api/admin/x/identity/customers?q=ada%40", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin list = %d: %s", rec.Code, rec.Body)
	}
	var list []Customer
	gctest.DecodeData(t, rec, &list)
	if len(list) != 1 || list[0].Email != "ada@example.com" {
		t.Errorf("list = %+v", list)
	}
	// A shopper session opens no admin door.
	if rec := as(t, app, ada.Token, http.MethodGet, "/api/admin/x/identity/customers", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("admin list with a shopper token = %d, want 401", rec.Code)
	}

	id := strconv.FormatInt(ada.Record.ID, 10)
	if rec := gctest.AdminRequest(t, app, http.MethodDelete, "/api/admin/x/identity/customers/"+id, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body)
	}
	if rec := as(t, app, ada.Token, http.MethodGet, "/x/identity/me", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("session of a deleted account = %d, want 401", rec.Code)
	}
	if rec := gctest.AdminRequest(t, app, http.MethodDelete, "/api/admin/x/identity/customers/"+id, nil); rec.Code != http.StatusNotFound {
		t.Errorf("deleting twice = %d, want 404", rec.Code)
	}
}

// TestEveryRouteIsDocumented is the module's copy of core's contract check:
// a route this module serves must appear in the merged OpenAPI document.
func TestEveryRouteIsDocumented(t *testing.T) {
	app, _ := newApp(t)
	paths, err := app.SpecPaths()
	if err != nil {
		t.Fatalf("SpecPaths: %v", err)
	}
	documented := map[string]bool{}
	for _, p := range paths {
		documented[p] = true
	}
	var served int
	for _, r := range app.Routes() {
		if r.Owner != "identity" {
			continue
		}
		served++
		if !documented[r.Path] {
			t.Errorf("route %s %s is served but absent from the contract", r.Method, r.Path)
		}
	}
	if served == 0 {
		t.Fatal("the module registered no routes")
	}
}
