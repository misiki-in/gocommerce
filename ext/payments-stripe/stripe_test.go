package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/misiki/gocommerce"
	"github.com/misiki/gocommerce/gctest"
)

const testSecret = "whsec_test_secret"

// TestVerifySignature is pure logic and needs no database: it is the one piece
// of this module that stands between the internet and marking orders paid.
func TestVerifySignature(t *testing.T) {
	t.Parallel()

	body := []byte(`{"id":"evt_1","type":"payment_intent.succeeded"}`)
	now := time.Now()

	sign := func(at time.Time, secret string, payload []byte) string {
		ts := strconv.FormatInt(at.Unix(), 10)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts))
		mac.Write([]byte("."))
		mac.Write(payload)
		return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
	}

	tests := []struct {
		name    string
		header  string
		body    []byte
		wantErr string
	}{
		{name: "valid", header: sign(now, testSecret, body), body: body},
		{
			name: "several candidate signatures, one correct",
			// Stripe sends more than one v1 during a secret rotation.
			header: sign(now, testSecret, body) + ",v1=" + strings.Repeat("00", 32),
			body:   body,
		},
		{name: "missing header", header: "", body: body, wantErr: "missing"},
		{name: "malformed header", header: "nonsense", body: body, wantErr: "malformed"},
		{
			name:   "wrong secret",
			header: sign(now, "whsec_someone_elses_secret", body), body: body,
			wantErr: "no signature matched",
		},
		{
			name: "tampered body",
			// The signature is valid for a different payload — which is the
			// whole attack this check exists to stop.
			header: sign(now, testSecret, []byte(`{"id":"evt_1","type":"other"}`)), body: body,
			wantErr: "no signature matched",
		},
		{
			name:   "replay of an old payload",
			header: sign(now.Add(-30*time.Minute), testSecret, body), body: body,
			wantErr: "away from now",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := verifySignature(tc.body, tc.header, testSecret, now)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestRegisterRequiresSecrets(t *testing.T) {
	t.Parallel()

	if err := New(Config{WebhookSecret: "x"}).Register(nil); err == nil {
		t.Error("a missing SecretKey should be refused")
	}
	// A webhook with no signing secret would let anyone mark orders paid, so
	// booting without one must fail rather than warn.
	if err := New(Config{SecretKey: "sk_test"}).Register(nil); err == nil {
		t.Error("a missing WebhookSecret should be refused")
	}
}

// stubStripe stands in for Stripe's API.
func stubStripe(t *testing.T) *httptest.Server {
	t.Helper()
	return gctest.StubHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/payment_intents":
			_ = r.ParseForm()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"pi_test_123","client_secret":"pi_test_123_secret","status":"requires_payment_method","amount":%s}`,
				r.Form.Get("amount"))
		case "/v1/refunds":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"re_test_1","status":"succeeded"}`)
		default:
			http.NotFound(w, r)
		}
	})
}

func newApp(t *testing.T) (*gocommerce.App, *Module) {
	t.Helper()
	server := stubStripe(t)
	mod := New(Config{
		SecretKey:     "sk_test_123",
		WebhookSecret: testSecret,
		BaseURL:       server.URL,
	})
	return gctest.New(t, mod), mod
}

// TestCheckoutAndWebhook is the module's reason to exist, end to end: a card
// checkout that settles when Stripe says so.
func TestCheckoutAndWebhook(t *testing.T) {
	app, _ := newApp(t)
	ctx := context.Background()

	result := gctest.PlaceOrder(t, app, "stripe")
	if result.Payment.Kind != gocommerce.IntentClientAction {
		t.Fatalf("payment kind = %q, want client_action", result.Payment.Kind)
	}
	if got := result.Payment.ClientData["client_secret"]; got != "pi_test_123_secret" {
		t.Errorf("client_secret = %q, want the one Stripe returned", got)
	}
	if result.Order.Status != gocommerce.OrderPending {
		t.Errorf("status = %q, want pending until Stripe confirms", result.Order.Status)
	}

	// The webhook arrives on the engine's own route.
	body := fmt.Sprintf(
		`{"id":"evt_1","type":"payment_intent.succeeded","data":{"object":{"id":"pi_test_123","metadata":{"order_id":"%d"}}}}`,
		result.Order.ID)
	rec := postWebhook(t, app, body, sign(body, testSecret, time.Now()))
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200: %s", rec.Code, rec.Body)
	}

	order, err := app.Order().Get(ctx, result.Order.ID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.PaymentStatus != gocommerce.PaymentPaid {
		t.Errorf("payment status = %q, want paid", order.PaymentStatus)
	}
	// Confirming is what makes the order shippable — the whole point of
	// MarkPaid also confirming.
	if order.Status != gocommerce.OrderConfirmed {
		t.Errorf("status = %q, want confirmed", order.Status)
	}
	if order.PaymentReference != "pi_test_123" {
		t.Errorf("payment reference = %q, want pi_test_123", order.PaymentReference)
	}
}

// TestWebhookReplayIsIdempotent: Stripe retries, and a retry must not settle
// the order twice or double-count the sale.
func TestWebhookReplayIsIdempotent(t *testing.T) {
	app, _ := newApp(t)
	ctx := context.Background()

	result := gctest.PlaceOrder(t, app, "stripe")
	body := fmt.Sprintf(
		`{"id":"evt_replay","type":"payment_intent.succeeded","data":{"object":{"id":"pi_test_123","metadata":{"order_id":"%d"}}}}`,
		result.Order.ID)

	for i := 0; i < 3; i++ {
		rec := postWebhook(t, app, body, sign(body, testSecret, time.Now()))
		if rec.Code != http.StatusOK {
			t.Fatalf("delivery %d: status = %d: %s", i+1, rec.Code, rec.Body)
		}
	}

	var events int
	if err := app.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM payments_stripe_events WHERE id = 'evt_replay'`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Errorf("recorded event rows = %d, want 1", events)
	}

	// The sale happened once, so exactly one unit left the shelf.
	variant, err := app.Products().GetVariantBySKU(ctx, "GCTEST-stripe")
	if err != nil {
		t.Fatalf("get variant: %v", err)
	}
	if variant.StockOnHand != 9 || variant.StockReserved != 0 {
		t.Errorf("stock = (on hand %d, reserved %d), want (9, 0)",
			variant.StockOnHand, variant.StockReserved)
	}
}

// TestWebhookRejectsBadSignature: an unsigned request must not be able to
// mark an order paid.
func TestWebhookRejectsBadSignature(t *testing.T) {
	app, _ := newApp(t)
	result := gctest.PlaceOrder(t, app, "stripe")

	body := fmt.Sprintf(
		`{"id":"evt_forged","type":"payment_intent.succeeded","data":{"object":{"id":"pi_x","metadata":{"order_id":"%d"}}}}`,
		result.Order.ID)

	for _, header := range []string{"", "t=1,v1=deadbeef", sign(body, "the-wrong-secret", time.Now())} {
		rec := postWebhook(t, app, body, header)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status for header %q = %d, want 400", header, rec.Code)
		}
	}

	order, err := app.Order().Get(context.Background(), result.Order.ID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.PaymentStatus != gocommerce.PaymentPending {
		t.Errorf("payment status = %q — a forged webhook changed the order", order.PaymentStatus)
	}
}

// TestRefund proves the optional Refunder capability is wired up.
func TestRefund(t *testing.T) {
	app, _ := newApp(t)
	ctx := context.Background()

	result := gctest.PlaceOrder(t, app, "stripe")
	if _, err := app.Pay().MarkPaid(ctx, result.Order.ID, "pi_test_123"); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	order, err := app.Pay().Refund(ctx, result.Order.ID, 0)
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if order.PaymentStatus != gocommerce.PaymentRefunded {
		t.Errorf("payment status = %q, want refunded", order.PaymentStatus)
	}
}

// ------------------------------------------------------------------ helpers

func sign(body, secret string, at time.Time) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write([]byte(body))
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(t *testing.T, app *gocommerce.App, body, signature string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/checkout/stripe/webhook", strings.NewReader(body))
	if signature != "" {
		req.Header.Set("Stripe-Signature", signature)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec
}
