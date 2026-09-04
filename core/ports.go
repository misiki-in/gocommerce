package gocommerce

import (
	"context"
	"net/http"
)

// The ports are the places where composition matters: the engine owns the
// commerce state machine, and these interfaces are how an external capability
// plugs into it. Nothing else is abstracted — an interface for something that
// is not actually replaceable is a cost with no buyer.

// ---------------------------------------------------------------- payments

// PaymentProvider turns an order into a payment attempt. Its code appears in
// the checkout URL: POST /api/checkout/{code}.
//
// A provider never writes to core tables. It reports what happened by calling
// [Payments.MarkPaid] or [Payments.MarkFailed], and the engine performs the
// state transition and writes the event — so there is exactly one place where
// an order becomes paid, no matter which gateway said so.
type PaymentProvider interface {
	Code() string
	// Initiate runs after the checkout transaction has committed, never while
	// core write locks are held: a gateway that is slow or down must not be
	// able to stall the database.
	Initiate(ctx context.Context, order *Order, opts PayOptions) (PaymentIntent, error)
}

// WebhookProvider is an optional capability. A provider that implements it is
// served at POST /api/checkout/{code}/webhook with the raw request body, so a
// gateway gets a documented callback URL without mounting any routes itself.
type WebhookProvider interface {
	Webhook() http.Handler
}

// Refunder is an optional capability. Cash on delivery does not implement it,
// which is exactly why refunding is not a required method on every provider.
type Refunder interface {
	Refund(ctx context.Context, order *Order, amountMinor int64) error
}

// PayOptions carries client-supplied data through checkout to the provider,
// so a gateway can receive what it needs without the engine's checkout body
// growing a field per integration.
type PayOptions struct {
	ReturnURL string            `json:"return_url,omitempty"`
	Data      map[string]string `json:"data,omitempty"`
}

// PaymentIntent tells the client what to do next.
type PaymentIntent struct {
	// Kind is "none" when nothing more is needed (cash on delivery),
	// "client_action" when the client completes payment in the page, or
	// "redirect" when it must send the shopper elsewhere.
	Kind       string            `json:"kind"`
	Provider   string            `json:"provider"`
	Reference  string            `json:"reference,omitempty"`
	ClientData map[string]string `json:"client_data,omitempty"`
}

// Payment intent kinds.
const (
	IntentNone         = "none"
	IntentClientAction = "client_action"
	IntentRedirect     = "redirect"
)

// ------------------------------------------------------------- fulfillment

// FulfillmentProvider books a shipment. The engine persists the result, moves
// the order to shipped and writes the event; the provider only talks to the
// carrier.
type FulfillmentProvider interface {
	Code() string
	Ship(ctx context.Context, order *Order, req ShipRequest) (Shipment, error)
}

// ShipRequest is what an operator asked for. Meta is the pass-through that
// lets a carrier module accept its own options — courier choice, pickup slot —
// without the engine learning any carrier's vocabulary.
type ShipRequest struct {
	Tracking string `json:"tracking,omitempty"`
	// Carrier is who will be carrying it, as a code from [Carriers]. An
	// operator who can see the label knows; leaving it empty makes the engine
	// work it out from the tracking number, which is right when the number
	// says so and is all it can do when it does not.
	Carrier string            `json:"carrier,omitempty"`
	Meta    map[string]string `json:"meta,omitempty"`
}

// Shipment is what the carrier returned.
type Shipment struct {
	Provider string `json:"provider"`
	Tracking string `json:"tracking,omitempty"`
	// Carrier is who is carrying the parcel, as a code from [Carriers]. A
	// provider that booked the shipment has been told and should say; leaving
	// it empty makes the engine work it out from the tracking number, which is
	// what the manual provider relies on.
	Carrier  string `json:"carrier,omitempty"`
	LabelURL string `json:"label_url,omitempty"`
}

// ------------------------------------------------------------ notifications

// Notification channels. The set is closed in v1 because the dispatcher has to
// know which recipient field feeds a channel; a novel channel subscribes to
// the event bus directly instead.
const (
	ChannelEmail = "email"
	ChannelSMS   = "sms"
)

// Notifier delivers a message on one channel. Templates belong to the notifier
// module: the engine sends data, never rendered copy, because wording and
// branding are the store's business and not the engine's.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// Notification is a message to send, in the language the shopper used.
type Notification struct {
	Event    string            `json:"event"`
	Channel  string            `json:"channel"`
	To       string            `json:"to"`
	Language string            `json:"language"`
	Data     map[string]string `json:"data"`
}

// --------------------------------------------------------------- i18n

// Translator supplies per-language overrides for catalog content. It is
// optional: a single-language store never registers one and pays nothing.
//
// Lookups are batched by id so that rendering a page of products costs one
// call rather than one per product.
type Translator interface {
	// Translate returns, for each requested id, the fields that differ in
	// lang. Kind is "product" or "variant". Missing ids and missing fields
	// simply keep their stored values.
	Translate(ctx context.Context, lang, kind string, ids []int64) (map[int64]map[string]string, error)
}

// Translatable kinds.
const (
	KindProduct = "product"
	KindVariant = "variant"
)

// ------------------------------------------------------------- registration

// RegisterPayment adds a payment method. A module may replace a built-in
// method by using its code; two modules claiming the same code is a startup
// error, because silently picking one would make the winner depend on
// argument order.
func (a *App) RegisterPayment(p PaymentProvider) {
	code := p.Code()
	if code == "" {
		a.regErrf("module %q: payment provider has an empty code", a.ownerName())
		return
	}
	if owner, exists := a.paymentOwners[code]; exists && owner != coreMigrationOwner {
		a.regErrf("payment code %q is already provided by module %q", code, owner)
		return
	}
	a.payments.providers[code] = p
	a.paymentOwners[code] = a.ownerName()
	a.log.Info("payment method registered", "code", code, "module", a.ownerName())
}

// RegisterFulfillment adds a fulfillment backend, with the same collision rule.
func (a *App) RegisterFulfillment(f FulfillmentProvider) {
	code := f.Code()
	if code == "" {
		a.regErrf("module %q: fulfillment provider has an empty code", a.ownerName())
		return
	}
	if owner, exists := a.fulfillmentOwners[code]; exists && owner != coreMigrationOwner {
		a.regErrf("fulfillment code %q is already provided by module %q", code, owner)
		return
	}
	a.fulfillment.providers[code] = f
	a.fulfillmentOwners[code] = a.ownerName()
	a.log.Info("fulfillment provider registered", "code", code, "module", a.ownerName())
}

// RegisterNotifier adds a delivery backend for one channel. Notifiers append
// rather than replace, so a store can send an email and mirror it to an audit
// sink; the built-in logger stands down for any channel that has a real
// notifier.
func (a *App) RegisterNotifier(channel string, n Notifier) {
	switch channel {
	case ChannelEmail, ChannelSMS:
	default:
		a.regErrf("module %q: unknown notification channel %q (want %q or %q)",
			a.ownerName(), channel, ChannelEmail, ChannelSMS)
		return
	}
	a.notifier.add(channel, n)
	a.log.Info("notifier registered", "channel", channel, "module", a.ownerName())
}

// RegisterTranslator installs the catalog translator. Only one may exist:
// merging overlapping translations from several sources has no obviously
// correct answer, so the engine declines to guess.
func (a *App) RegisterTranslator(t Translator) {
	if a.translator != nil {
		a.regErrf("module %q: a translator is already registered by %q", a.ownerName(), a.translatorOwner)
		return
	}
	a.translator = t
	a.translatorOwner = a.ownerName()
	a.log.Info("translator registered", "module", a.ownerName())
}

// Subscribe registers an event handler. Patterns are an exact name
// ("order.paid"), a prefix ("order.*") or "*".
//
// Handlers must be idempotent: delivery is at-least-once, and a redelivery
// after a crash is normal operation rather than an error.
func (a *App) Subscribe(pattern string, h EventHandler) {
	a.bus.subscribe(pattern, a.ownerName(), h)
}
