package delivery

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/domain"
	"github.com/nyver2k/remindertgbot/internal/observability"
)

const (
	batchSize   = 20
	maxAttempts = 5
)

// NotificationStore is the subset of postgres.NotificationRepo used by Worker.
type NotificationStore interface {
	LeasePending(ctx context.Context, workerID string, limit int) ([]domain.ScheduledNotification, error)
	MarkSent(ctx context.Context, id uuid.UUID) error
	MarkFailed(ctx context.Context, id uuid.UUID, attempts int) error
	UpdateFireAt(ctx context.Context, id uuid.UUID, fireAt time.Time) error
}

// ReminderStore is used to resolve UserID for delivery.
type ReminderStore interface {
	Get(ctx context.Context, id uuid.UUID) (*domain.Reminder, error)
}

// Worker polls the notifications queue and delivers messages.
type Worker struct {
	notifications NotificationStore
	reminders     ReminderStore
	sender        Sender
	workerID      string
	tick          time.Duration
	log           *slog.Logger
}

func NewWorker(
	notifications NotificationStore,
	reminders ReminderStore,
	sender Sender,
	workerID string,
	tick time.Duration,
	log *slog.Logger,
) *Worker {
	return &Worker{
		notifications: notifications,
		reminders:     reminders,
		sender:        sender,
		workerID:      workerID,
		tick:          tick,
		log:           log,
	}
}

func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.process(ctx); err != nil {
				w.log.Error("delivery tick failed", "err", err)
			}
		}
	}
}

func (w *Worker) process(ctx context.Context) error {
	batch, err := w.notifications.LeasePending(ctx, w.workerID, batchSize)
	if err != nil {
		return err
	}

	for _, n := range batch {
		w.deliverSafe(ctx, n)
	}
	return nil
}

// deliverSafe recovers from a panic delivering a single notification so one
// malformed notification cannot crash the whole worker process and halt
// delivery for every other user's notifications.
func (w *Worker) deliverSafe(ctx context.Context, n domain.ScheduledNotification) {
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("panic delivering notification",
				"notification_id", n.ID,
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}
	}()
	w.deliver(ctx, n)
}

func (w *Worker) deliver(ctx context.Context, n domain.ScheduledNotification) {
	rem, err := w.reminders.Get(ctx, n.ReminderID)
	if err != nil {
		attempts := n.Attempts + 1
		if errors.Is(err, domain.ErrNotFound) {
			// Reminder deleted — no point retrying.
			w.log.Warn("reminder lookup failed, reminder gone", "id", n.ReminderID, "err", err)
			_ = w.notifications.MarkFailed(ctx, n.ID, maxAttempts)
			return
		}
		// Transient error (e.g. DB hiccup): back off like a send failure instead
		// of leaving fire_at untouched, which would let LeasePending re-lease
		// this row on every tick in a tight busy-retry loop.
		w.log.Warn("reminder lookup failed, will retry", "id", n.ReminderID, "attempt", attempts, "err", err)
		observability.NotificationsFailedTotal.Inc()
		if attempts >= maxAttempts {
			_ = w.notifications.MarkFailed(ctx, n.ID, attempts)
			return
		}
		delay := backoffDuration(attempts)
		nextFire := time.Now().UTC().Add(delay)
		if err := w.notifications.UpdateFireAt(ctx, n.ID, nextFire); err != nil {
			w.log.Error("update fire_at failed", "notification_id", n.ID, "err", err)
		}
		return
	}

	if err := w.sender.Send(ctx, rem.UserID, n.Text); err != nil {
		attempts := n.Attempts + 1
		w.log.Warn("send failed", "notification_id", n.ID, "attempt", attempts, "err", err)
		observability.NotificationsFailedTotal.Inc()

		// Apply exponential backoff by scheduling the next delivery attempt.
		// After maxAttempts, mark as permanently failed.
		if attempts >= maxAttempts {
			_ = w.notifications.MarkFailed(ctx, n.ID, attempts)
			return
		}
		delay := backoffDuration(attempts)
		nextFire := time.Now().UTC().Add(delay)
		if err := w.notifications.UpdateFireAt(ctx, n.ID, nextFire); err != nil {
			w.log.Error("update fire_at failed", "notification_id", n.ID, "err", err)
		}
		return
	}

	if err := w.notifications.MarkSent(ctx, n.ID); err != nil {
		w.log.Error("mark sent failed", "notification_id", n.ID, "err", err)
	}
	observability.NotificationsSentTotal.Inc()
	w.log.Info("notification sent", "notification_id", n.ID, "user_id", rem.UserID)
}

func backoffDuration(attempt int) time.Duration {
	secs := math.Pow(2, float64(attempt)) * 5
	if secs > 300 {
		secs = 300
	}
	return time.Duration(secs) * time.Second
}
