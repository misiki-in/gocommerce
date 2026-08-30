package gocommerce

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
)

// notifierSet holds the delivery backends, per channel.
type notifierSet struct {
	mu        sync.RWMutex
	byChannel map[string][]Notifier
	log       *slog.Logger
}

func (n *notifierSet) add(channel string, no Notifier) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.byChannel == nil {
		n.byChannel = map[string][]Notifier{}
	}
	n.byChannel[channel] = append(n.byChannel[channel], no)
}

func (n *notifierSet) forChannel(channel string) []Notifier {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.byChannel[channel]
}

// send delivers on one channel. Every notifier gets the message even if an
// earlier one failed, so a broken vendor does not silence the others; the
// aggregated error asks the outbox to retry the whole event.
func (n *notifierSet) send(ctx context.Context, note Notification) error {
	targets := n.forChannel(note.Channel)
	if len(targets) == 0 {
		return nil
	}
	var failures []error
	for _, target := range targets {
		if err := target.Notify(ctx, note); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// logNotifier is the built-in backend: it writes the message to the log and
// nothing else. The engine defines the notification abstraction and emits the
// triggers; actually delivering email or SMS is a vendor's job, and a vendor
// is a module.
type logNotifier struct{ log *slog.Logger }

func (l logNotifier) Notify(ctx context.Context, n Notification) error {
	l.log.Info("notification (no delivery backend installed)",
		"event", n.Event, "channel", n.Channel, "to", n.To,
		"language", n.Language, "order", n.Data["order_number"])
	return nil
}

// subscribeNotifications wires order events to notification delivery.
//
// Notifications are downstream consumers of committed events, never part of
// the checkout transaction: a vendor outage must not be able to fail a sale.
func (a *App) subscribeNotifications() {
	a.bus.subscribe("order.*", coreMigrationOwner, func(ctx context.Context, e Event) error {
		var ev OrderEvent
		if err := e.Decode(&ev); err != nil {
			return fmt.Errorf("decode %s payload: %w", e.Name, err)
		}
		return a.notifyOrder(ctx, e.Name, &ev)
	})
}

func (a *App) notifyOrder(ctx context.Context, event string, ev *OrderEvent) error {
	data := orderNotificationData(ev)
	lang := ev.Language
	if lang == "" {
		lang = a.cfg.DefaultLanguage
	}

	var failures []error
	if ev.Email != "" {
		if err := a.notifier.send(ctx, Notification{
			Event: event, Channel: ChannelEmail, To: ev.Email, Language: lang, Data: data,
		}); err != nil {
			failures = append(failures, fmt.Errorf("email: %w", err))
		}
	}
	if ev.Phone != "" {
		if err := a.notifier.send(ctx, Notification{
			Event: event, Channel: ChannelSMS, To: ev.Phone, Language: lang, Data: data,
		}); err != nil {
			failures = append(failures, fmt.Errorf("sms: %w", err))
		}
	}
	return errors.Join(failures...)
}

// orderNotificationData flattens an order event into template data. It is flat
// strings on purpose: a notifier module owns its templates, and the engine
// hands it values rather than opinions about wording.
func orderNotificationData(ev *OrderEvent) map[string]string {
	data := map[string]string{
		"order_id":       strconv.FormatInt(ev.OrderID, 10),
		"order_number":   ev.Number,
		"order_status":   ev.Status,
		"payment_status": ev.PaymentStatus,
		"payment_method": ev.Provider,
		"currency":       ev.Currency,
		"total_minor":    strconv.FormatInt(ev.TotalMinor, 10),
		"item_count":     strconv.Itoa(len(ev.Lines)),
		"customer_name":  ev.Name,
		"customer_email": ev.Email,
	}
	if ev.Tracking != "" {
		data["tracking"] = ev.Tracking
	}
	if ev.Reason != "" {
		data["reason"] = ev.Reason
	}
	// A one-line summary covers the common template without forcing every
	// notifier to walk the line array.
	summary := ""
	for i, l := range ev.Lines {
		if i > 0 {
			summary += ", "
		}
		summary += strconv.Itoa(l.Quantity) + " x " + l.Title
		if l.VariantLabel != "" {
			summary += " (" + l.VariantLabel + ")"
		}
	}
	data["items_summary"] = summary
	return data
}
