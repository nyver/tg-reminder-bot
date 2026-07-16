package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/clock"
	"github.com/nyver2k/remindertgbot/internal/domain"
	"github.com/nyver2k/remindertgbot/internal/provider"
)

type watcherReminderStore struct {
	next     map[uuid.UUID]*time.Time
	statuses map[uuid.UUID]domain.Status
}

func (s *watcherReminderStore) LeaseDue(context.Context, string, int) ([]domain.Reminder, error) {
	return nil, nil
}

func (s *watcherReminderStore) UpdateNextEval(_ context.Context, id uuid.UUID, next *time.Time) error {
	if s.next == nil {
		s.next = make(map[uuid.UUID]*time.Time)
	}
	s.next[id] = next
	return nil
}

func (s *watcherReminderStore) UpdateStatus(_ context.Context, id uuid.UUID, status domain.Status) error {
	if s.statuses == nil {
		s.statuses = make(map[uuid.UUID]domain.Status)
	}
	s.statuses[id] = status
	return nil
}

func (*watcherReminderStore) MarkConditionalDue(context.Context) error { return nil }

type watcherNotificationStore struct {
	err      error
	enqueued []*domain.ScheduledNotification
}

func (s *watcherNotificationStore) Enqueue(_ context.Context, n *domain.ScheduledNotification) error {
	s.enqueued = append(s.enqueued, n)
	return s.err
}

func newTestWatcher(store *watcherReminderStore, notifications *watcherNotificationStore, now time.Time) *Watcher {
	evaluator := NewEvaluator(provider.NewRegistry(), nil, clock.NewFake(now), 30, nil)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewWatcher(store, notifications, evaluator, "test-worker", time.Second, log)
}

func TestWatcherRetriesReminderWhenEnqueueFails(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := &watcherReminderStore{}
	notifications := &watcherNotificationStore{err: errors.New("queue unavailable")}
	watcher := newTestWatcher(store, notifications, now)
	rem := domain.Reminder{
		ID:       uuid.New(),
		Kind:     domain.KindRecurring,
		EvalCron: "0 18 * * *",
		Spec:     domain.Spec{Message: "test"},
	}

	started := time.Now().UTC()
	watcher.processReminder(context.Background(), rem)

	retryAt := store.next[rem.ID]
	if retryAt == nil {
		t.Fatal("expected enqueue failure to schedule a retry")
	}
	if retryAt.Before(started.Add(59*time.Second)) || retryAt.After(started.Add(61*time.Second)) {
		t.Fatalf("retry scheduled at %v, want approximately one minute later", retryAt)
	}
	if _, ok := store.statuses[rem.ID]; ok {
		t.Fatal("reminder status must not advance after enqueue failure")
	}
}

func TestWatcherRetriesReminderWhenEvaluationFails(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := &watcherReminderStore{}
	watcher := newTestWatcher(store, &watcherNotificationStore{}, now)
	rem := domain.Reminder{ID: uuid.New(), Kind: domain.KindConditional}

	watcher.processReminder(context.Background(), rem)

	if store.next[rem.ID] == nil {
		t.Fatal("expected evaluation failure to schedule a retry")
	}
}

func TestWatcherMarksReminderFailedForInvalidCron(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := &watcherReminderStore{}
	watcher := newTestWatcher(store, &watcherNotificationStore{}, now)
	rem := domain.Reminder{
		ID:       uuid.New(),
		Kind:     domain.KindRecurring,
		EvalCron: "invalid cron",
		Spec:     domain.Spec{Message: "test"},
	}

	watcher.processReminder(context.Background(), rem)

	if got := store.statuses[rem.ID]; got != domain.StatusFailed {
		t.Fatalf("status = %q, want %q", got, domain.StatusFailed)
	}
}

func TestWatcherFinishesSuccessfulOneShotReminder(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := &watcherReminderStore{}
	notifications := &watcherNotificationStore{}
	watcher := newTestWatcher(store, notifications, now)
	rem := domain.Reminder{
		ID:   uuid.New(),
		Kind: domain.KindAbsolute,
		Spec: domain.Spec{Message: "test"},
	}

	watcher.processReminder(context.Background(), rem)

	if len(notifications.enqueued) != 1 {
		t.Fatalf("enqueued %d notifications, want 1", len(notifications.enqueued))
	}
	if got := store.statuses[rem.ID]; got != domain.StatusDone {
		t.Fatalf("status = %q, want %q", got, domain.StatusDone)
	}
}
