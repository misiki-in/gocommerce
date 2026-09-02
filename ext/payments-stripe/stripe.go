// Package stripe takes card payments through Stripe.
//
// It speaks Stripe's REST API over net/http rather than through stripe-go.
// Creating a PaymentIntent is one form POST and verifying a webhook is one
// HMAC, so the SDK would buy little and would put its whole dependency tree
// into the graph of every store that installs this module.
//
//	app, err := gocommerce.New(cfg,
//	    stripe.New(stripe.Config{
//	        SecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
//	        WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
//	    }),
//	)
//
// Stripe then calls POST /api/checkout/stripe/webhook, a route the engine
// owns and hands to this module with the body untouched.
package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/misiki/gocommerce/core"
)

const (
	defaultBaseURL = "https://api.stripe.com"
	// webhookTolerance rejects replays of an old signed payload. Five minutes
	// is Stripe's own recommendation.
	webhookTolerance = 5 * time.Minute
	maxWebhookBytes  = 1 << 20
)

// Config configures the module.
type Config struct {
	// SecretKey is the Stripe secret API key. Required.
	SecretKey string
	// WebhookSecret is the signing secret of the webhook endpoint. Required:
	// without it any caller could mark orders paid.
	WebhookSecret string
	// BaseURL overrides the Stripe endpoint, for tests.
	BaseURL string
	// Client overrides the HTTP client.
	Client *http.Client
}

// Module is the Stripe payment provider.
type Module struct {
	cfg    Config
	client *http.Client
	log    *slog.Logger
	db     *sql.DB
	pay    *gocommerce.Payments
	orders *gocommerce.Orders
}

// New constructs the module.
func New(cfg Config) *Module { return &Module{cfg: cfg} }

// Name implements gocommerce.Module.
func (m *Module) Name() string { return "payments-stripe" }

// Migrations implements gocommerce.Module.
//
// The one table this module owns exists to make webhook handling idempotent:
// Stripe will deliver the same event more than once, and settling an order
// twice must be impossible rather than merely unlikely.
func (m *Module) Migrations() []gocommerce.Migration {
	return []gocommerce.Migration{{
		ID: "0001_events",
		SQL: `
			CREATE TABLE payments_stripe_events (
			    id          text        PRIMARY KEY,
			    type        text        NOT NULL,
			    order_id    bigint,
			    received_at timestamptz NOT NULL DEFAULT now()
			);
			CREATE INDEX payments_stripe_events_received_idx
			    ON payments_stripe_events (received_at);`,
	}}
}

// Register implements gocommerce.Module.
func (m *Module) Register(app *gocommerce.App) error {
	if strings.TrimSpace(m.cfg.SecretKey) == "" {
		return errors.New("stripe: SecretKey is required")
	}
	if strings.TrimSpace(m.cfg.WebhookSecret) == "" {
		return errors.New("stripe: WebhookSecret is required — without it, anyone could mark orders paid")
	}
	if m.cfg.BaseURL == "" {
		m.cfg.BaseURL = defaultBaseURL
	}
	m.client = m.cfg.Client
	if m.client == nil {
		m.client = &http.Client{Timeout: 20 * time.Second}
	}
	m.log = app.Log()
	m.db = app.DB()
	m.pay = app.Pay()
	m.orders = app.Order()

	// Registering the provider is the whole installation. The webhook route
	// comes free: the engine serves /api/checkout/stripe/webhook and hands it
	// to Webhook() below.
	app.RegisterPayment(m)
	return nil
}

// Code implements gocommerce.PaymentProvider.
func (m *Module) Code() string { return "stripe" }

// Initiate creates a PaymentIntent and hands its client secret to the
// storefront. It runs after the checkout transaction has committed, so a slow
// response from Stripe never holds a database lock.
func (m *Module) Initiate(ctx context.Context, order *gocommerce.Order, opts gocommerce.PayOptions) (gocommerce.PaymentIntent, error) {
	form := url.Values{}
	form.Set("amount", strconv.FormatInt(order.Total.AmountMinor, 10))
	form.Set("currency", strings.ToLower(order.Total.Currency))
	form.Set("automatic_payment_methods[enabled]", "true")
	form.Set("metadata[order_id]", strconv.FormatInt(order.ID, 10))
	form.Set("metadata[order_number]", order.Number)
	if order.Email != "" {
		form.Set("receipt_email", order.Email)
	}
	for k, v := range opts.Data {
		// Client-supplied extras are namespaced so they cannot overwrite the
		// fields this module depends on.
		form.Set("metadata[client_"+k+"]", v)
	}

	var intent struct {
		ID           string `json:"id"`
		ClientSecret string `json:"client_secret"`
		Status       string `json:"status"`
	}
	if err := m.post(ctx, "/v1/payment_intents", form, &intent); err != nil {
		return gocommerce.PaymentIntent{}, err
	}

	return gocommerce.PaymentIntent{
		Kind:      gocommerce.IntentClientAction,
		Provider:  m.Code(),
		Reference: intent.ID,
		ClientData: map[string]string{
			"client_secret": intent.ClientSecret,
			"status":        intent.Status,
		},
	}, nil
}

// Refund implements gocommerce.Refunder.
func (m *Module) Refund(ctx context.Context, order *gocommerce.Order, amountMinor int64) error {
	if order.PaymentReference == "" {
		return errors.New("stripe: this order has no payment reference to refund")
	}
	form := url.Values{}
	form.Set("payment_intent", order.PaymentReference)
	form.Set("amount", strconv.FormatInt(amountMinor, 10))

	var refund struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := m.post(ctx, "/v1/refunds", form, &refund); err != nil {
		return err
	}
	if refund.Status == "failed" {
		return fmt.Errorf("stripe: refund %s failed", refund.ID)
	}
	return nil
}

// Webhook implements gocommerce.WebhookProvider.
func (m *Module) Webhook() http.Handler {
	return http.HandlerFunc(m.handleWebhook)
}

func (m *Module) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes))
	if err != nil {
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}
	if err := verifySignature(body, r.Header.Get("Stripe-Signature"), m.cfg.WebhookSecret, time.Now()); err != nil {
		m.log.Warn("rejected a Stripe webhook", "error", err)
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	var event struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			Object struct {
				ID       string            `json:"id"`
				Metadata map[string]string `json:"metadata"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "malformed event", http.StatusBadRequest)
		return
	}

	orderID, _ := strconv.ParseInt(event.Data.Object.Metadata["order_id"], 10, 64)

	// Claim the event id. Stripe retries, and a second delivery of the same
	// event must not settle the order a second time. INSERT ... ON CONFLICT
	// DO NOTHING makes the claim atomic even across application instances.
	res, err := m.db.ExecContext(r.Context(), `
		INSERT INTO payments_stripe_events (id, type, order_id)
		VALUES ($1, $2, nullif($3, 0))
		ON CONFLICT (id) DO NOTHING`, event.ID, event.Type, orderID)
	if err != nil {
		m.log.Error("could not record the Stripe event", "event_id", event.ID, "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Already handled. Answering 200 stops Stripe retrying it forever.
		w.WriteHeader(http.StatusOK)
		return
	}

	if orderID == 0 {
		// An event for something we did not start — a dashboard refund, say.
		// Acknowledge it rather than making Stripe retry what we will never
		// act on.
		w.WriteHeader(http.StatusOK)
		return
	}

	switch event.Type {
	case "payment_intent.succeeded":
		// The engine performs the transition and writes the event. This
		// module never touches an order row itself.
		if _, err := m.pay.MarkPaid(r.Context(), orderID, event.Data.Object.ID); err != nil {
			m.log.Error("could not mark the order paid", "order_id", orderID, "error", err)
			m.releaseClaim(r.Context(), event.ID)
			http.Error(w, "could not apply payment", http.StatusInternalServerError)
			return
		}
	case "payment_intent.payment_failed":
		if _, err := m.pay.MarkFailed(r.Context(), orderID, "stripe reported a failed payment"); err != nil {
			m.log.Error("could not mark the payment failed", "order_id", orderID, "error", err)
			m.releaseClaim(r.Context(), event.ID)
			http.Error(w, "could not apply failure", http.StatusInternalServerError)
			return
		}
	case "charge.refunded":
		m.log.Info("Stripe reported a refund", "order_id", orderID)
	}

	w.WriteHeader(http.StatusOK)
}

// releaseClaim un-claims an event whose handling failed, so Stripe's retry
// finds work to do instead of being told the event was already handled.
func (m *Module) releaseClaim(ctx context.Context, eventID string) {
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM payments_stripe_events WHERE id = $1`, eventID); err != nil {
		m.log.Error("could not release the event claim", "event_id", eventID, "error", err)
	}
}

// verifySignature checks Stripe's `t=...,v1=...` header.
//
// The signed payload is the timestamp, a dot, and the raw body — which is why
// the engine hands webhook routes their bytes untouched. The timestamp is
// checked too: a valid signature over an old body is a replay.
func verifySignature(body []byte, header, secret string, now time.Time) error {
	if header == "" {
		return errors.New("missing Stripe-Signature header")
	}
	var timestamp string
	var signatures []string
	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch key {
		case "t":
			timestamp = value
		case "v1":
			signatures = append(signatures, value)
		}
	}
	if timestamp == "" || len(signatures) == 0 {
		return errors.New("malformed Stripe-Signature header")
	}

	secs, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("malformed timestamp in Stripe-Signature")
	}
	age := now.Sub(time.Unix(secs, 0))
	if age > webhookTolerance || age < -webhookTolerance {
		return fmt.Errorf("signature timestamp is %s away from now", age.Round(time.Second))
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := mac.Sum(nil)

	for _, candidate := range signatures {
		got, err := hex.DecodeString(candidate)
		if err != nil {
			continue
		}
		if hmac.Equal(got, expected) {
			return nil
		}
	}
	return errors.New("no signature matched")
}

// post sends a form-encoded request and decodes the JSON reply.
func (m *Module) post(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.cfg.BaseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.SecretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("stripe: %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("stripe: %s: read response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr struct {
			Error struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Error.Message != "" {
			return fmt.Errorf("stripe: %s: %s", path, apiErr.Error.Message)
		}
		return fmt.Errorf("stripe: %s returned %s", path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}
