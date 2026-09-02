// Package sendgrid delivers gocommerce notifications as email through
// SendGrid.
//
// It talks to SendGrid's v3 REST API over net/http rather than through the
// vendor SDK: sending one email is a single JSON POST, and taking a dependency
// for it would put SendGrid's transitive tree into the dependency graph of
// every store that installs this module.
//
//	app, err := gocommerce.New(cfg,
//	    sendgrid.New(sendgrid.Config{
//	        APIKey: os.Getenv("SENDGRID_API_KEY"),
//	        From:   "orders@example.com",
//	    }),
//	)
package sendgrid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/misiki/gocommerce/core"
)

const defaultBaseURL = "https://api.sendgrid.com"

// Config configures the module.
type Config struct {
	// APIKey is a SendGrid API key with Mail Send permission. Required.
	APIKey string
	// From is the sender address. Required.
	From string
	// FromName is the sender's display name.
	FromName string
	// ReplyTo is optional.
	ReplyTo string
	// Subjects overrides the built-in subject line for an event, keyed by
	// event name. Templates receive the notification's Data map.
	Subjects map[string]string
	// Bodies overrides the built-in plain-text body for an event.
	Bodies map[string]string
	// BaseURL overrides the SendGrid endpoint, for tests.
	BaseURL string
	// Client overrides the HTTP client.
	Client *http.Client
}

// Module is the SendGrid notifier.
type Module struct {
	cfg      Config
	client   *http.Client
	log      interface{ Error(string, ...any) }
	subjects map[string]*template.Template
	bodies   map[string]*template.Template
}

// New constructs the module.
func New(cfg Config) *Module { return &Module{cfg: cfg} }

// Name implements gocommerce.Module.
func (m *Module) Name() string { return "notify-sendgrid" }

// Migrations implements gocommerce.Module. Sending email owns no state.
func (m *Module) Migrations() []gocommerce.Migration { return nil }

// Register implements gocommerce.Module.
func (m *Module) Register(app *gocommerce.App) error {
	if strings.TrimSpace(m.cfg.APIKey) == "" {
		return errors.New("sendgrid: APIKey is required")
	}
	if strings.TrimSpace(m.cfg.From) == "" {
		return errors.New("sendgrid: From is required")
	}
	if m.cfg.BaseURL == "" {
		m.cfg.BaseURL = defaultBaseURL
	}
	m.client = m.cfg.Client
	if m.client == nil {
		m.client = &http.Client{Timeout: 15 * time.Second}
	}
	m.log = app.Log()

	if err := m.compileTemplates(); err != nil {
		return err
	}
	app.RegisterNotifier(gocommerce.ChannelEmail, m)
	return nil
}

// compileTemplates prepares the copy. Templates live here rather than in the
// engine because wording and branding are the store's business: the engine
// hands over values, not opinions about how to phrase them.
func (m *Module) compileTemplates() error {
	m.subjects = map[string]*template.Template{}
	m.bodies = map[string]*template.Template{}

	merge := func(defaults, overrides map[string]string, into map[string]*template.Template, kind string) error {
		combined := map[string]string{}
		for k, v := range defaults {
			combined[k] = v
		}
		for k, v := range overrides {
			combined[k] = v
		}
		for event, text := range combined {
			tpl, err := template.New(kind + ":" + event).Parse(text)
			if err != nil {
				return fmt.Errorf("sendgrid: %s template for %s: %w", kind, event, err)
			}
			into[event] = tpl
		}
		return nil
	}

	if err := merge(defaultSubjects, m.cfg.Subjects, m.subjects, "subject"); err != nil {
		return err
	}
	return merge(defaultBodies, m.cfg.Bodies, m.bodies, "body")
}

var defaultSubjects = map[string]string{
	gocommerce.EventOrderCreated:   "Order {{.order_number}} confirmed",
	gocommerce.EventOrderPaid:      "Payment received for order {{.order_number}}",
	gocommerce.EventOrderShipped:   "Order {{.order_number}} is on its way",
	gocommerce.EventOrderDelivered: "Order {{.order_number}} was delivered",
	gocommerce.EventOrderCancelled: "Order {{.order_number}} was cancelled",
}

var defaultBodies = map[string]string{
	gocommerce.EventOrderCreated: `Hello {{.customer_name}},

Thanks for your order {{.order_number}}.

{{.items_summary}}

We'll email you again when it ships.`,

	gocommerce.EventOrderPaid: `Hello {{.customer_name}},

We've received your payment for order {{.order_number}}.

{{.items_summary}}`,

	gocommerce.EventOrderShipped: `Hello {{.customer_name}},

Order {{.order_number}} has shipped.
{{if .tracking}}Tracking number: {{.tracking}}{{end}}

{{.items_summary}}`,

	gocommerce.EventOrderDelivered: `Hello {{.customer_name}},

Order {{.order_number}} has been delivered. We hope you like it.`,

	gocommerce.EventOrderCancelled: `Hello {{.customer_name}},

Order {{.order_number}} has been cancelled.
{{if .reason}}Reason: {{.reason}}{{end}}`,
}

// Notify implements gocommerce.Notifier.
func (m *Module) Notify(ctx context.Context, n gocommerce.Notification) error {
	if n.Channel != gocommerce.ChannelEmail || n.To == "" {
		return nil
	}
	subjectTpl, ok := m.subjects[n.Event]
	if !ok {
		// An event with no template is not an error: a store may add events
		// this module has never heard of.
		return nil
	}
	subject, err := render(subjectTpl, n.Data)
	if err != nil {
		return err
	}
	body, err := render(m.bodies[n.Event], n.Data)
	if err != nil {
		return err
	}
	return m.send(ctx, n.To, subject, body)
}

func render(tpl *template.Template, data map[string]string) (string, error) {
	if tpl == nil {
		return "", nil
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

type mailPayload struct {
	Personalizations []personalization `json:"personalizations"`
	From             emailAddress      `json:"from"`
	ReplyTo          *emailAddress     `json:"reply_to,omitempty"`
	Subject          string            `json:"subject"`
	Content          []mailContent     `json:"content"`
}

type personalization struct {
	To []emailAddress `json:"to"`
}

type emailAddress struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

type mailContent struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

func (m *Module) send(ctx context.Context, to, subject, body string) error {
	payload := mailPayload{
		Personalizations: []personalization{{To: []emailAddress{{Email: to}}}},
		From:             emailAddress{Email: m.cfg.From, Name: m.cfg.FromName},
		Subject:          subject,
		Content:          []mailContent{{Type: "text/plain", Value: body}},
	}
	if m.cfg.ReplyTo != "" {
		payload.ReplyTo = &emailAddress{Email: m.cfg.ReplyTo}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.cfg.BaseURL+"/v3/mail/send", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		// Returning the error asks the outbox to retry with backoff, which is
		// the right answer to a vendor being briefly unreachable.
		return fmt.Errorf("sendgrid: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	err = fmt.Errorf("sendgrid: send returned %s: %s", resp.Status, strings.TrimSpace(string(detail)))
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
		// A rejected message will be rejected again. Log it and let the event
		// settle, rather than retrying a bad address twelve times.
		m.log.Error("sendgrid rejected the message", "status", resp.Status, "to", to, "detail", string(detail))
		return nil
	}
	return err
}
