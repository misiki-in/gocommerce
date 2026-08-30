// Package msg91 delivers gocommerce notifications as SMS through MSG91.
//
// India requires SMS content to be pre-registered as a DLT template, so this
// module sends a template id and its variables rather than free text — the
// wording lives in your MSG91 account, which is where the regulator expects
// to find it.
//
//	app, err := gocommerce.New(cfg,
//	    msg91.New(msg91.Config{
//	        AuthKey:   os.Getenv("MSG91_AUTH_KEY"),
//	        Templates: map[string]string{
//	            gocommerce.EventOrderShipped: "65a1b2c3d4e5f6",
//	        },
//	    }),
//	)
package msg91

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/misiki/gocommerce"
)

const defaultBaseURL = "https://control.msg91.com"

// Config configures the module.
type Config struct {
	// AuthKey is the MSG91 authentication key. Required.
	AuthKey string
	// Templates maps an event name to a DLT-approved flow template id. An
	// event with no template is not sent — which is the usual case, since
	// most stores text about shipping and nothing else.
	Templates map[string]string
	// DefaultCountryCode is prefixed to recipient numbers that have none,
	// e.g. "91". MSG91 requires the country code.
	DefaultCountryCode string
	// BaseURL overrides the endpoint, for tests.
	BaseURL string
	// Client overrides the HTTP client.
	Client *http.Client
}

// Module is the MSG91 notifier.
type Module struct {
	cfg    Config
	client *http.Client
	log    *slog.Logger
}

// New constructs the module.
func New(cfg Config) *Module { return &Module{cfg: cfg} }

// Name implements gocommerce.Module.
func (m *Module) Name() string { return "notify-msg91" }

// Migrations implements gocommerce.Module. Sending SMS owns no state.
func (m *Module) Migrations() []gocommerce.Migration { return nil }

// Register implements gocommerce.Module.
func (m *Module) Register(app *gocommerce.App) error {
	if strings.TrimSpace(m.cfg.AuthKey) == "" {
		return errors.New("msg91: AuthKey is required")
	}
	if len(m.cfg.Templates) == 0 {
		return errors.New("msg91: at least one event Template id is required, or the module would send nothing")
	}
	if m.cfg.BaseURL == "" {
		m.cfg.BaseURL = defaultBaseURL
	}
	m.client = m.cfg.Client
	if m.client == nil {
		m.client = &http.Client{Timeout: 15 * time.Second}
	}
	m.log = app.Log()

	app.RegisterNotifier(gocommerce.ChannelSMS, m)
	return nil
}

// Notify implements gocommerce.Notifier.
func (m *Module) Notify(ctx context.Context, n gocommerce.Notification) error {
	if n.Channel != gocommerce.ChannelSMS || n.To == "" {
		return nil
	}
	templateID, ok := m.cfg.Templates[n.Event]
	if !ok {
		// No template registered for this event: nothing to send, and not a
		// failure. Most stores text about shipping and nothing else.
		return nil
	}

	recipient := map[string]string{"mobiles": m.normalizeNumber(n.To)}
	// The notification's flat data becomes the template's variables directly,
	// so adding a variable to a DLT template needs no code change here.
	for k, v := range n.Data {
		recipient[k] = v
	}

	payload := map[string]any{
		"template_id": templateID,
		"recipients":  []map[string]string{recipient},
	}
	return m.post(ctx, "/api/v5/flow/", payload)
}

// normalizeNumber strips formatting and applies the default country code,
// because MSG91 rejects a number without one.
func (m *Module) normalizeNumber(raw string) string {
	var digits strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	number := digits.String()
	cc := m.cfg.DefaultCountryCode
	if cc == "" || strings.HasPrefix(number, cc) {
		return number
	}
	// A 10-digit number is a local one that needs the prefix; anything longer
	// probably already carries a country code of its own.
	if len(number) == 10 {
		return cc + number
	}
	return number
}

func (m *Module) post(ctx context.Context, path string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.BaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("authkey", m.cfg.AuthKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("msg91: %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// MSG91 answers 200 with {"type":"error"} for some rejections, so the
		// status code alone is not the whole answer.
		var result struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &result)
		if strings.EqualFold(result.Type, "error") {
			m.log.Error("MSG91 rejected the message", "detail", result.Message)
			return nil // a rejected message will be rejected again; do not retry
		}
		return nil
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
		m.log.Error("MSG91 rejected the request", "status", resp.Status, "detail", string(body))
		return nil
	}
	return fmt.Errorf("msg91: %s returned %s", path, resp.Status)
}
