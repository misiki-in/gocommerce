package gocommerce

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"
)

// Outbox delivery policy.
const (
	// outboxMaxAttempts before a row is parked as dead. Dead-lettering beats
	// deleting: an event nobody could deliver is evidence, and evidence should
	// survive long enough to be looked at.
	outboxMaxAttempts = 12
	// outboxVisibility is how long a claimed row is hidden from other workers.
	// If the process dies mid-delivery the row reappears after this, which is
	// the at-least-once guarantee doing its job.
	outboxVisibility = 60 * time.Second
	outboxMaxBackoff = 15 * time.Minute
)

// outbox writes and delivers durable events.
//
// The problem it solves: committing an order and then publishing its event are
// two operations, and a process that dies between them loses the event while
// keeping the order — a paid order nobody was told about. Writing the event to
// a table inside the same transaction makes that gap impossible.
type outbox struct {
	db        *sql.DB
	bus       *eventBus
	log       *slog.Logger
	batchSize int
	poll      time.Duration

	// delivered is signalled after every successful pass; tests wait on it
	// instead of sleeping.
	delivered chan struct{}
	// wake lets a request that just wrote an event ask for immediate
	// delivery, so the common case does not wait out the poll interval.
	wake chan struct{}
}

func (o *outbox) nudge() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// write appends an event to the outbox inside the caller's transaction. It is
// the only way core state changes announce themselves.
func (o *outbox) write(ctx context.Context, tx *sql.Tx, name string, aggregateType string, aggregateID int64, payload any) error {
	id, err := newUUID()
	if err != nil {
		return err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", name, err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO outbox_events (event_id, event_name, event_version, aggregate_type, aggregate_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id, name, 1, aggregateType, aggregateID, data)
	if err != nil {
		return fmt.Errorf("write outbox event %s: %w", name, err)
	}
	return nil
}

// run is the dispatcher loop, started with the app and stopped with it.
func (o *outbox) run(ctx context.Context) {
	timer := time.NewTimer(o.poll)
	defer timer.Stop()

	for {
		n, err := o.deliverBatch(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			o.log.Error("outbox pass failed", "error", err)
		case n > 0:
			// Work found: come straight back for more rather than waiting out
			// the poll interval on a backlog.
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-o.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(o.poll)
		case <-timer.C:
			timer.Reset(o.poll)
		}
	}
}

// deliverBatch claims a batch of unpublished events and delivers them,
// returning how many were claimed.
//
// The claim uses FOR UPDATE SKIP LOCKED, which is what makes running several
// application instances safe: each worker takes rows no other worker holds,
// with no coordination beyond the database.
func (o *outbox) deliverBatch(ctx context.Context) (int, error) {
	rows, err := o.db.QueryContext(ctx, `
		UPDATE outbox_events o
		SET attempts = o.attempts + 1,
		    available_at = now() + make_interval(secs => $2)
		FROM (
			SELECT id FROM outbox_events
			WHERE published_at IS NULL AND NOT dead AND available_at <= now()
			ORDER BY id
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		) AS claimed
		WHERE o.id = claimed.id
		RETURNING o.id, o.event_id, o.event_name, o.event_version,
		          o.aggregate_type, o.aggregate_id, o.payload, o.created_at, o.attempts`,
		o.batchSize, outboxVisibility.Seconds())
	if err != nil {
		return 0, fmt.Errorf("claim outbox batch: %w", err)
	}

	type claimed struct {
		rowID    int64
		attempts int
		event    Event
	}
	var batch []claimed
	for rows.Next() {
		var c claimed
		if err := rows.Scan(&c.rowID, &c.event.ID, &c.event.Name, &c.event.Version,
			&c.event.AggregateType, &c.event.AggregateID, &c.event.Data,
			&c.event.At, &c.attempts); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan outbox row: %w", err)
		}
		batch = append(batch, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("read outbox batch: %w", err)
	}
	rows.Close()

	for _, c := range batch {
		if err := o.bus.dispatch(ctx, c.event); err != nil {
			o.fail(ctx, c.rowID, c.attempts, c.event, err)
			continue
		}
		if err := o.markPublished(ctx, c.rowID); err != nil {
			// The handlers already ran. Failing to record that means the event
			// is redelivered later, which is exactly what at-least-once
			// promises and why handlers must be idempotent.
			o.log.Error("could not mark event published",
				"event_id", c.event.ID, "error", err)
		}
	}

	if len(batch) > 0 {
		o.signal()
	}
	return len(batch), nil
}

func (o *outbox) markPublished(ctx context.Context, rowID int64) error {
	_, err := o.db.ExecContext(ctx,
		`UPDATE outbox_events SET published_at = now(), last_error = NULL WHERE id = $1`, rowID)
	return err
}

// fail schedules a retry with exponential backoff, or parks the row when it
// has failed too many times.
func (o *outbox) fail(ctx context.Context, rowID int64, attempts int, e Event, cause error) {
	if attempts >= outboxMaxAttempts {
		if _, err := o.db.ExecContext(ctx,
			`UPDATE outbox_events SET dead = true, last_error = $2 WHERE id = $1`,
			rowID, cause.Error()); err != nil {
			o.log.Error("could not dead-letter event", "event_id", e.ID, "error", err)
		}
		o.log.Error("event dead-lettered after repeated failures",
			"event", e.Name, "event_id", e.ID, "attempts", attempts, "error", cause)
		return
	}

	delay := outboxBackoff(attempts)
	if _, err := o.db.ExecContext(ctx, `
		UPDATE outbox_events
		SET available_at = now() + make_interval(secs => $2), last_error = $3
		WHERE id = $1`, rowID, delay.Seconds(), cause.Error()); err != nil {
		o.log.Error("could not schedule event retry", "event_id", e.ID, "error", err)
	}
	o.log.Warn("event delivery failed, will retry",
		"event", e.Name, "event_id", e.ID, "attempts", attempts, "retry_in", delay)
}

func outboxBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	secs := math.Pow(2, float64(attempts-1))
	d := time.Duration(secs) * time.Second
	if d > outboxMaxBackoff || d <= 0 {
		return outboxMaxBackoff
	}
	return d
}

func (o *outbox) signal() {
	select {
	case o.delivered <- struct{}{}:
	default:
	}
}

// DrainOutbox delivers every event that is due, returning how many were
// processed. Tests and the reference CLI use it to make delivery synchronous
// without waiting on the dispatcher's poll interval.
func (a *App) DrainOutbox(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := a.outbox.deliverBatch(ctx)
		if err != nil {
			return total, err
		}
		total += n
		if n == 0 {
			return total, nil
		}
	}
}

// PendingEvents reports how many events are waiting to be delivered, and how
// many were parked as undeliverable.
func (a *App) PendingEvents(ctx context.Context) (pending, dead int, err error) {
	err = a.db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE published_at IS NULL AND NOT dead),
		       count(*) FILTER (WHERE dead)
		FROM outbox_events`).Scan(&pending, &dead)
	return pending, dead, err
}
