// Package razorpay takes payments through Razorpay.
//
// Like the Stripe module, it speaks the vendor's REST API over net/http: an
// order is one JSON POST and a webhook is one HMAC, so the SDK would add a
// dependency without adding capability.
//
//	app, err := gocommerce.New(cfg,
//	    razorpay.New(razorpay.Config{
//	        KeyID:         os.Getenv("RAZORPAY_KEY_ID"),
//	        KeySecret:     os.Getenv("RAZORPAY_KEY_SECRET"),
//	        WebhookSecret: os.Getenv("RAZORPAY_WEBHOOK_SECRET"),
//	    }),
//	)
package razorpay

import (
	"bytes"
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
	"strconv"
	"strings"
	"time"

	"github.com/misiki/gocommerce/core"
)

const (
	defaultBaseURL  = "https://api.razorpay.com"
	maxWebhookBytes = 1 << 20
)

// Config configures the module.
type Config struct {
	// KeyID and KeySecret are the API credentials. Required.
	KeyID, KeySecret string
	// WebhookSecret signs the webhook. Required: without it any caller could
	// mark orders paid.
	WebhookSecret string
	// Hosted uses Razorpay's hosted checkout page and returns a redirect
	// intent instead of data for the in-page widget.
	Hosted bool
	// BaseURL overrides the endpoint, for tests.
	BaseURL string
	// Client overrides the HTTP client.
	Client *http.Client
}

// Module is the Razorpay payment provider.
type Module struct {
	cfg    Config
	client *http.Client
	log    *slog.Logger
	db     *sql.DB
	pay    *gocommerce.Payments
}

// New constructs the module.
func New(cfg Config) *Module { return &Module{cfg: cfg} }

// Name implements gocommerce.Module.
func (m *Module) Name() string { return "payments-razorpay" }

// Migrations implements gocommerce.Module: one table, for webhook idempotency.
func (m *Module) Migrations() []gocommerce.Migration {
	return []gocommerce.Migration{{
		ID: "0001_events",
		SQL: `
			CREATE TABLE payments_razorpay_events (
			    id          text        PRIMARY KEY,
			    event       text        NOT NULL,
			    order_id    bigint,
			    received_at timestamptz NOT NULL DEFAULT now()
			);`,
	}}
}

// Register implements gocommerce.Module.
func (m *Module) Register(app *gocommerce.App) error {
	switch {
	case strings.TrimSpace(m.cfg.KeyID) == "":
		return errors.New("razorpay: KeyID is required")
	case strings.TrimSpace(m.cfg.KeySecret) == "":
		return errors.New("razorpay: KeySecret is required")
	case strings.TrimSpace(m.cfg.WebhookSecret) == "":
		return errors.New("razorpay: WebhookSecret is required — without it, anyone could mark orders paid")
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

	app.RegisterPayment(m)
	return nil
}

// Code implements gocommerce.PaymentProvider.
func (m *Module) Code() string { return "razorpay" }

// Initiate creates a Razorpay order for the checkout to be completed against.
func (m *Module) Initiate(ctx context.Context, order *gocommerce.Order, opts gocommerce.PayOptions) (gocommerce.PaymentIntent, error) {
	payload := map[string]any{
		"amount":   order.Total.AmountMinor,
		"currency": strings.ToUpper(order.Total.Currency),
		"receipt":  order.Number,
		"notes": map[string]string{
			"order_id":     strconv.FormatInt(order.ID, 10),
			"order_number": order.Number,
		},
	}

	var created struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	if err := m.post(ctx, "/v1/orders", payload, &created); err != nil {
		return gocommerce.PaymentIntent{}, err
	}

	intent := gocommerce.PaymentIntent{
		Provider:  m.Code(),
		Reference: created.ID,
		ClientData: map[string]string{
			"razorpay_order_id": created.ID,
			"key_id":            m.cfg.KeyID,
			"amount":            strconv.FormatInt(created.Amount, 10),
			"currency":          created.Currency,
		},
	}
	if m.cfg.Hosted {
		// The hosted page needs somewhere to send the shopper back to.
		intent.Kind = gocommerce.IntentRedirect
		if opts.ReturnURL != "" {
			intent.ClientData["callback_url"] = opts.ReturnURL
		}
	} else {
		intent.Kind = gocommerce.IntentClientAction
	}
	return intent, nil
}

// Refund implements gocommerce.Refunder.
func (m *Module) Refund(ctx context.Context, order *gocommerce.Order, amountMinor int64) error {
	paymentID, err := m.paymentIDFor(ctx, order.ID)
	if err != nil {
		return err
	}
	return m.post(ctx, "/v1/payments/"+paymentID+"/refund",
		map[string]any{"amount": amountMinor}, nil)
}

// paymentIDFor finds the captured payment behind an order. Razorpay refunds
// against the payment, not the order, and the two are different objects.
func (m *Module) paymentIDFor(ctx context.Context, orderID int64) (string, error) {
	var paymentID sql.NullString
	err := m.db.QueryRowContext(ctx, `
		SELECT id FROM payments_razorpay_events
		WHERE order_id = $1 AND event = 'payment.captured'
		ORDER BY received_at DESC LIMIT 1`, orderID).Scan(&paymentID)
	if errors.Is(err, sql.ErrNoRows) || !paymentID.Valid {
		return "", errors.New("razorpay: no captured payment recorded for this order")
	}
	if err != nil {
		return "", err
	}
	// The row id is the webhook event id; the payment id is stored alongside.
	var pid string
	if err := m.db.QueryRowContext(ctx, `
		SELECT coalesce(nullif(split_part(id, ':', 2), ''), id)
		FROM payments_razorpay_events WHERE id = $1`, paymentID.String).Scan(&pid); err != nil {
		return "", err
	}
	return pid, nil
}

// Webhook implements gocommerce.WebhookProvider.
func (m *Module) Webhook() http.Handler { return http.HandlerFunc(m.handleWebhook) }

func (m *Module) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes))
	if err != nil {
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}
	if !validSignature(body, r.Header.Get("X-Razorpay-Signature"), m.cfg.WebhookSecret) {
		m.log.Warn("rejected a Razorpay webhook with an invalid signature")
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	var event struct {
		Event   string `json:"event"`
		Payload struct {
			Payment struct {
				Entity struct {
					ID    string            `json:"id"`
					Notes map[string]string `json:"notes"`
				} `json:"entity"`
			} `json:"payment"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "malformed event", http.StatusBadRequest)
		return
	}

	entity := event.Payload.Payment.Entity
	orderID, _ := strconv.ParseInt(entity.Notes["order_id"], 10, 64)
	// Razorpay has no per-delivery event id, so the payment id plus the event
	// name is the natural idempotency key: the same payment being captured is
	// the same fact however many times we are told about it.
	eventKey := event.Event + ":" + entity.ID

	res, err := m.db.ExecContext(r.Context(), `
		INSERT INTO payments_razorpay_events (id, event, order_id)
		VALUES ($1, $2, nullif($3, 0))
		ON CONFLICT (id) DO NOTHING`, eventKey, event.Event, orderID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		w.WriteHeader(http.StatusOK) // already handled
		return
	}
	if orderID == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	switch event.Event {
	case "payment.captured":
		if _, err := m.pay.MarkPaid(r.Context(), orderID, entity.ID); err != nil {
			m.log.Error("could not mark the order paid", "order_id", orderID, "error", err)
			m.releaseClaim(r.Context(), eventKey)
			http.Error(w, "could not apply payment", http.StatusInternalServerError)
			return
		}
	case "payment.failed":
		if _, err := m.pay.MarkFailed(r.Context(), orderID, "razorpay reported a failed payment"); err != nil {
			m.log.Error("could not mark the payment failed", "order_id", orderID, "error", err)
			m.releaseClaim(r.Context(), eventKey)
			http.Error(w, "could not apply failure", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (m *Module) releaseClaim(ctx context.Context, id string) {
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM payments_razorpay_events WHERE id = $1`, id); err != nil {
		m.log.Error("could not release the event claim", "id", id, "error", err)
	}
}

// validSignature checks Razorpay's hex HMAC-SHA256 of the raw body.
func validSignature(body []byte, header, secret string) bool {
	if header == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	got, err := hex.DecodeString(strings.TrimSpace(header))
	if err != nil {
		return false
	}
	return hmac.Equal(got, mac.Sum(nil))
}

func (m *Module) post(ctx context.Context, path string, payload any, out any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.BaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.SetBasicAuth(m.cfg.KeyID, m.cfg.KeySecret)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("razorpay: %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("razorpay: %s: read response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr struct {
			Error struct {
				Description string `json:"description"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Error.Description != "" {
			return fmt.Errorf("razorpay: %s: %s", path, apiErr.Error.Description)
		}
		return fmt.Errorf("razorpay: %s returned %s", path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}
