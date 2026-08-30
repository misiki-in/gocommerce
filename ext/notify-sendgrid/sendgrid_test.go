package sendgrid

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/misiki/gocommerce"
	"github.com/misiki/gocommerce/gctest"
)

// captured is one message the stub SendGrid received.
type captured struct {
	To      string
	Subject string
	Body    string
}

type stub struct {
	mu       sync.Mutex
	messages []captured
	status   int
}

func (s *stub) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var payload mailPayload
	_ = json.Unmarshal(body, &payload)

	s.mu.Lock()
	msg := captured{Subject: payload.Subject}
	if len(payload.Personalizations) > 0 && len(payload.Personalizations[0].To) > 0 {
		msg.To = payload.Personalizations[0].To[0].Email
	}
	if len(payload.Content) > 0 {
		msg.Body = payload.Content[0].Value
	}
	s.messages = append(s.messages, msg)
	status := s.status
	s.mu.Unlock()

	if status == 0 {
		status = http.StatusAccepted
	}
	w.WriteHeader(status)
}

func (s *stub) all() []captured {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]captured, len(s.messages))
	copy(out, s.messages)
	return out
}

func TestRegisterRequiresConfig(t *testing.T) {
	t.Parallel()
	if err := New(Config{From: "a@b.com"}).Register(nil); err == nil {
		t.Error("a missing APIKey should be refused")
	}
	if err := New(Config{APIKey: "k"}).Register(nil); err == nil {
		t.Error("a missing From address should be refused")
	}
}

// TestSendsOrderEmails is the seam this module exists to fill: the engine
// emits events and this turns them into email.
func TestSendsOrderEmails(t *testing.T) {
	s := &stub{}
	server := gctest.StubHTTP(t, s.handler)

	app := gctest.New(t, New(Config{
		APIKey: "SG.test", From: "orders@example.com", FromName: "Example",
		BaseURL: server.URL,
	}))

	result := gctest.PlaceOrder(t, app, gocommerce.CodeCOD)
	gctest.DrainOutbox(t, app)

	messages := s.all()
	if len(messages) != 1 {
		t.Fatalf("messages sent = %d, want 1", len(messages))
	}
	msg := messages[0]
	if msg.To != "gctest@example.com" {
		t.Errorf("recipient = %q, want the shopper's address", msg.To)
	}
	if !strings.Contains(msg.Subject, result.Order.Number) {
		t.Errorf("subject %q should name the order", msg.Subject)
	}
	if !strings.Contains(msg.Body, "GC Test") {
		t.Errorf("body %q should greet the customer by name", msg.Body)
	}

	// Shipping produces a second, different email.
	if _, err := app.Ship().Create(context.Background(), result.Order.ID,
		gocommerce.ProviderManual, gocommerce.ShipRequest{Tracking: "TRACK-9"}); err != nil {
		t.Fatalf("ship: %v", err)
	}
	gctest.DrainOutbox(t, app)

	messages = s.all()
	if len(messages) != 2 {
		t.Fatalf("messages sent = %d, want 2", len(messages))
	}
	shipped := messages[1]
	if !strings.Contains(shipped.Subject, "on its way") {
		t.Errorf("shipping subject = %q", shipped.Subject)
	}
	if !strings.Contains(shipped.Body, "TRACK-9") {
		t.Errorf("shipping body should carry the tracking number: %q", shipped.Body)
	}
	gctest.AssertOutboxEmpty(t, app)
}

// TestCustomTemplates: wording belongs to the store, so it must be
// overridable without editing this module.
func TestCustomTemplates(t *testing.T) {
	s := &stub{}
	server := gctest.StubHTTP(t, s.handler)

	app := gctest.New(t, New(Config{
		APIKey: "SG.test", From: "orders@example.com", BaseURL: server.URL,
		Subjects: map[string]string{
			gocommerce.EventOrderCreated: "Merci pour votre commande {{.order_number}}",
		},
		Bodies: map[string]string{
			gocommerce.EventOrderCreated: "Bonjour {{.customer_name}} — {{.items_summary}}",
		},
	}))

	gctest.PlaceOrder(t, app, gocommerce.CodeCOD)
	gctest.DrainOutbox(t, app)

	messages := s.all()
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages))
	}
	if !strings.HasPrefix(messages[0].Subject, "Merci") {
		t.Errorf("subject = %q, want the override", messages[0].Subject)
	}
	if !strings.HasPrefix(messages[0].Body, "Bonjour") {
		t.Errorf("body = %q, want the override", messages[0].Body)
	}
}

// TestRejectedMessageDoesNotRetryForever: a permanently bad address should
// not consume twelve delivery attempts.
func TestRejectedMessageDoesNotRetryForever(t *testing.T) {
	s := &stub{status: http.StatusBadRequest}
	server := gctest.StubHTTP(t, s.handler)

	app := gctest.New(t, New(Config{
		APIKey: "SG.test", From: "orders@example.com", BaseURL: server.URL,
	}))

	gctest.PlaceOrder(t, app, gocommerce.CodeCOD)
	gctest.DrainOutbox(t, app)

	// The event is considered delivered even though SendGrid refused it: a
	// 400 will be a 400 again, so retrying would only delay every event
	// behind it.
	gctest.AssertOutboxEmpty(t, app)
}
