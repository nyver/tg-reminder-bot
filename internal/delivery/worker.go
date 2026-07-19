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
	ScheduleRetry(ctx context.Context, id uuid.UUID, attempts int, fireAt time.Time) error
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
	quiet         QuietModeResolver
}

// QuietModeResolver applies per-user quiet hours at delivery time.
type QuietModeResolver interface {
	IsQuiet(ctx context.Context, userID int64, at time.Time) (bool, error)
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

// SetQuietModeResolver enables user-specific silent Telegram delivery.
func (w *Worker) SetQuietModeResolver(resolver QuietModeResolver) { w.quiet = resolver }

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
			w.retry(ctx, n)
		}
	}()
	w.deliver(ctx, n)
}

// retry persists both the next delivery time and the incremented attempt
// count. Keeping these values in one store operation prevents a notification
// from retrying forever with attempts=0 and makes releasing its lease atomic.
func (w *Worker) retry(ctx context.Context, n domain.ScheduledNotification) {
	attempts := n.Attempts + 1
	observability.NotificationsFailedTotal.Inc()
	if attempts >= maxAttempts {
		if err := w.notifications.MarkFailed(ctx, n.ID, attempts); err != nil {
			w.log.Error("mark notification failed", "notification_id", n.ID, "err", err)
		}
		return
	}
	nextFire := time.Now().UTC().Add(backoffDuration(attempts))
	if err := w.notifications.ScheduleRetry(ctx, n.ID, attempts, nextFire); err != nil {
		w.log.Error("schedule notification retry", "notification_id", n.ID, "err", err)
	}
}

func (w *Worker) deliver(ctx context.Context, n domain.ScheduledNotification) {
	rem, err := w.reminders.Get(ctx, n.ReminderID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Reminder deleted — no point retrying.
			w.log.Warn("reminder lookup failed, reminder gone", "id", n.ReminderID, "err", err)
			_ = w.notifications.MarkFailed(ctx, n.ID, maxAttempts)
			return
		}
		// Transient error (e.g. DB hiccup): back off like a send failure instead
		// of leaving fire_at untouched, which would let LeasePending re-lease
		// this row on every tick in a tight busy-retry loop.
		w.log.Warn("reminder lookup failed, will retry", "id", n.ReminderID, "attempt", n.Attempts+1, "err", err)
		w.retry(ctx, n)
		return
	}

	message := buildOutboundMessage(rem.UserID, n, *rem)
	if w.quiet != nil {
		if quiet, quietErr := w.quiet.IsQuiet(ctx, rem.UserID, time.Now()); quietErr == nil {
			message.Quiet = quiet
		} else {
			w.log.Warn("resolve quiet mode", "user_id", rem.UserID, "err", quietErr)
		}
	}
	if err := w.sender.Send(ctx, message); err != nil {
		w.log.Warn("send failed", "notification_id", n.ID, "attempt", n.Attempts+1, "err", err)
		w.retry(ctx, n)
		return
	}

	if err := w.notifications.MarkSent(ctx, n.ID); err != nil {
		w.log.Error("mark sent failed", "notification_id", n.ID, "err", err)
	}
	observability.NotificationsSentTotal.Inc()
	w.log.Info("notification sent", "notification_id", n.ID, "user_id", rem.UserID)
}

func buildOutboundMessage(userID int64, notification domain.ScheduledNotification, reminder domain.Reminder) OutboundMessage {
	action := func(text, name string) OutboundAction {
		return OutboundAction{Text: text, Entity: "notification", Action: name, ID: notification.ID}
	}
	message := OutboundMessage{UserID: userID, Text: notification.Text}
	switch {
	case reminder.Spec.Trigger == domain.TriggerThreshold:
		message.Actions = [][]OutboundAction{{action("🔄 Проверить снова", "check"), action("⏸ Пауза", "pause")}}
	case reminder.Spec.Trigger == domain.TriggerDigest && reminder.Spec.Event.Type == "rss":
		message.Actions = [][]OutboundAction{{action("▶ Создать ещё раз", "repeat"), action("⏸ Пауза", "pause")}}
	default:
		message.Actions = [][]OutboundAction{
			{action("✅ Выполнено", "done")},
			{action("Через 10 минут", "snooze_10"), action("Через час", "snooze_60")},
			{action("Отложить", "snooze_default"), action("Завтра утром", "snooze_morning")},
		}
	}
	return message
}

func backoffDuration(attempt int) time.Duration {
	secs := math.Pow(2, float64(attempt)) * 5
	if secs > 300 {
		secs = 300
	}
	return time.Duration(secs) * time.Second
}
