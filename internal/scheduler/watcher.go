package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/domain"
	"github.com/nyver2k/remindertgbot/internal/observability"
	"github.com/robfig/cron/v3"
)

const watcherBatchSize = 10

// ReminderStore is the subset used by Watcher.
type ReminderStore interface {
	LeaseDue(ctx context.Context, workerID string, limit int) ([]domain.Reminder, error)
	UpdateNextEval(ctx context.Context, id uuid.UUID, next *time.Time) error
	UpdateStatus(ctx context.Context, id uuid.UUID, status domain.Status) error
	MarkConditionalDue(ctx context.Context) error
}

// NotificationEnqueuer enqueues produced notifications.
type NotificationEnqueuer interface {
	Enqueue(ctx context.Context, n *domain.ScheduledNotification) error
}

// Watcher polls active reminders, evaluates them and enqueues notifications.
type Watcher struct {
	reminders     ReminderStore
	notifications NotificationEnqueuer
	evaluator     *Evaluator
	workerID      string
	tick          time.Duration
	log           *slog.Logger
}

func NewWatcher(
	reminders ReminderStore,
	notifications NotificationEnqueuer,
	evaluator *Evaluator,
	workerID string,
	tick time.Duration,
	log *slog.Logger,
) *Watcher {
	return &Watcher{
		reminders:     reminders,
		notifications: notifications,
		evaluator:     evaluator,
		workerID:      workerID,
		tick:          tick,
		log:           log,
	}
}

func (w *Watcher) Run(ctx context.Context) error {
	w.startup(ctx)

	ticker := time.NewTicker(w.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.tick_(ctx); err != nil {
				w.log.Error("watcher tick failed", "err", err)
			}
		}
	}
}

func (w *Watcher) startup(ctx context.Context) {
	if err := w.reminders.MarkConditionalDue(ctx); err != nil {
		w.log.Warn("startup sweep: mark due failed", "err", err)
		return
	}
	w.log.Info("startup sweep: evaluating conditional reminders")
	if err := w.tick_(ctx); err != nil {
		w.log.Warn("startup sweep: initial tick failed", "err", err)
	}
}

func (w *Watcher) tick_(ctx context.Context) error {
	total := 0
	for {
		batch, err := w.reminders.LeaseDue(ctx, w.workerID, watcherBatchSize)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		total += len(batch)
		for _, rem := range batch {
			w.processReminder(ctx, rem)
		}
		if len(batch) < watcherBatchSize {
			break // no more rows
		}
	}
	if total > 0 {
		w.log.Info("watcher tick: processing", "count", total)
		updateActiveMetrics(ctx, w.reminders)
	}
	return nil
}

func (w *Watcher) processReminder(ctx context.Context, rem domain.Reminder) {
	planned, err := w.evaluator.Evaluate(ctx, rem)
	if err != nil {
		w.log.Error("evaluate reminder", "id", rem.ID, "err", err)
		w.scheduleRetry(ctx, rem.ID)
		return
	}

	enqueueFailed := false
	for _, p := range planned {
		n := &domain.ScheduledNotification{
			ReminderID:     rem.ID,
			FireAt:         p.FireAt,
			Text:           p.Text,
			IdempotencyKey: p.IdempotencyKey,
			Status:         domain.NotificationPending,
		}
		if err := w.notifications.Enqueue(ctx, n); err != nil {
			w.log.Error("enqueue notification", "reminder_id", rem.ID, "err", err)
			enqueueFailed = true
		}
	}
	if enqueueFailed {
		w.scheduleRetry(ctx, rem.ID)
		return
	}

	// Advance recurring/conditional reminders; finish one-shot reminders.
	next, err := nextEval(rem)
	if err != nil {
		w.log.Error("calculate next evaluation", "id", rem.ID, "err", err)
		if updateErr := w.reminders.UpdateStatus(ctx, rem.ID, domain.StatusFailed); updateErr != nil {
			w.log.Error("mark reminder failed", "id", rem.ID, "err", updateErr)
		}
		return
	}
	if next == nil {
		if err := w.reminders.UpdateStatus(ctx, rem.ID, domain.StatusDone); err != nil {
			w.log.Error("finish reminder", "id", rem.ID, "err", err)
		}
		return
	}
	if err := w.reminders.UpdateNextEval(ctx, rem.ID, next); err != nil {
		w.log.Error("advance reminder", "id", rem.ID, "err", err)
	}
}

func (w *Watcher) scheduleRetry(ctx context.Context, id uuid.UUID) {
	delay := w.tick
	if delay < time.Minute {
		delay = time.Minute
	}
	retryAt := time.Now().UTC().Add(delay)
	if err := w.reminders.UpdateNextEval(ctx, id, &retryAt); err != nil {
		w.log.Error("schedule reminder retry", "id", id, "err", err)
	}
}

// nextEval computes the next watcher evaluation time from the cron expression.
// For absolute reminders (no cron), it returns nil, nil.
func nextEval(rem domain.Reminder) (*time.Time, error) {
	if rem.EvalCron == "" {
		return nil, nil
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := parser.Parse(rem.EvalCron)
	if err != nil {
		return nil, err
	}
	next := schedule.Next(time.Now().In(userTZ(rem)))
	return &next, nil
}

func updateActiveMetrics(_ context.Context, _ ReminderStore) {
	// TODO M6: query counts by trigger and update observability.RemindersActive gauge.
	_ = observability.RemindersActive
}
