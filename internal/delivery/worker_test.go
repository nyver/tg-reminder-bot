package delivery

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/domain"
)

var errSendFailed = errors.New("send failed")

var testLog = slog.New(slog.NewTextHandler(os.Stdout, nil))

type mockNotifStore struct {
	lease    []domain.ScheduledNotification
	markSent map[uuid.UUID]bool
	markFail map[uuid.UUID]int
	updateFa map[uuid.UUID]time.Time
	retries  map[uuid.UUID]int
}

func (m *mockNotifStore) LeasePending(ctx context.Context, workerID string, limit int) ([]domain.ScheduledNotification, error) {
	return m.lease, nil
}
func (m *mockNotifStore) MarkSent(ctx context.Context, id uuid.UUID) error {
	if m.markSent == nil {
		m.markSent = map[uuid.UUID]bool{}
	}
	m.markSent[id] = true
	return nil
}
func (m *mockNotifStore) MarkFailed(ctx context.Context, id uuid.UUID, attempts int) error {
	if m.markFail == nil {
		m.markFail = map[uuid.UUID]int{}
	}
	m.markFail[id] = attempts
	return nil
}
func (m *mockNotifStore) ScheduleRetry(ctx context.Context, id uuid.UUID, attempts int, fireAt time.Time) error {
	if m.updateFa == nil {
		m.updateFa = map[uuid.UUID]time.Time{}
	}
	if m.retries == nil {
		m.retries = map[uuid.UUID]int{}
	}
	m.updateFa[id] = fireAt
	m.retries[id] = attempts
	return nil
}

type mockReminderStore struct {
	rem *domain.Reminder
	err error
}

func (m *mockReminderStore) Get(ctx context.Context, id uuid.UUID) (*domain.Reminder, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.rem, nil
}

type mockSender struct{ err error }

func (m *mockSender) Send(ctx context.Context, message OutboundMessage) error {
	return m.err
}

type panicSender struct{}

func (panicSender) Send(ctx context.Context, message OutboundMessage) error {
	panic("boom")
}

func TestBuildOutboundMessageUsesReminderSpecificActions(t *testing.T) {
	notification := domain.ScheduledNotification{ID: uuid.New(), Text: "message"}
	cases := []struct {
		name       string
		reminder   domain.Reminder
		wantAction string
	}{
		{"task", domain.Reminder{}, "done"},
		{"threshold", domain.Reminder{Spec: domain.Spec{Trigger: domain.TriggerThreshold}}, "check"},
		{"rss", domain.Reminder{Spec: domain.Spec{Trigger: domain.TriggerDigest, Event: domain.EventSpec{Type: "rss"}}}, "repeat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			message := buildOutboundMessage(42, notification, tc.reminder)
			if message.UserID != 42 || message.Text != "message" || len(message.Actions) == 0 || len(message.Actions[0]) == 0 {
				t.Fatalf("message = %+v", message)
			}
			if message.Actions[0][0].Action != tc.wantAction || message.Actions[0][0].ID != notification.ID {
				t.Fatalf("first action = %+v", message.Actions[0][0])
			}
		})
	}
}

// TestDeliverSafeRecoversPanic guards against a regression where a panic
// delivering a single notification (e.g. a sender bug) would crash the whole
// worker process and halt delivery for every other user's notifications.
func TestDeliverSafeRecoversPanic(t *testing.T) {
	remID := uuid.New()
	notifID := uuid.New()
	notif := domain.ScheduledNotification{ID: notifID, ReminderID: remID, FireAt: time.Now().UTC()}

	store := &mockNotifStore{lease: []domain.ScheduledNotification{notif}}
	remStore := &mockReminderStore{rem: &domain.Reminder{ID: remID, UserID: 123}}

	w := NewWorker(store, remStore, panicSender{}, "test", time.Second, testLog)
	w.deliverSafe(context.Background(), notif) // must not panic
	if store.retries[notifID] != 1 {
		t.Fatalf("panic retry attempts = %d, want 1", store.retries[notifID])
	}
}

func TestDeliverSendsOnSuccess(t *testing.T) {
	remID := uuid.New()
	notifID := uuid.New()
	notif := domain.ScheduledNotification{ID: notifID, ReminderID: remID, FireAt: time.Now().UTC()}

	store := &mockNotifStore{lease: []domain.ScheduledNotification{notif}}
	remStore := &mockReminderStore{rem: &domain.Reminder{ID: remID, UserID: 123}}
	sender := &mockSender{err: nil}

	w := NewWorker(store, remStore, sender, "test", time.Second, testLog)
	w.deliver(context.Background(), notif)

	if !store.markSent[notifID] {
		t.Fatal("expected notification to be marked as sent")
	}
}

func TestDeliverUpdatesFireAtOnSendFailure(t *testing.T) {
	remID := uuid.New()
	notifID := uuid.New()
	notif := domain.ScheduledNotification{ID: notifID, ReminderID: remID, FireAt: time.Now().UTC(), Attempts: 0}

	store := &mockNotifStore{lease: []domain.ScheduledNotification{notif}}
	remStore := &mockReminderStore{rem: &domain.Reminder{ID: remID, UserID: 123}}
	sender := &mockSender{err: errSendFailed}

	w := NewWorker(store, remStore, sender, "test", time.Second, testLog)
	w.deliver(context.Background(), notif)

	if _, ok := store.markSent[notifID]; ok {
		t.Fatal("notification should NOT be marked as sent")
	}
	// Check that ScheduleRetry was called with a future time (backoff).
	if len(store.updateFa) == 0 {
		t.Fatal("expected ScheduleRetry to be called")
	}
	if store.retries[notifID] != 1 {
		t.Fatalf("persisted attempts = %d, want 1", store.retries[notifID])
	}
	nextFire := store.updateFa[notifID]
	if nextFire.Before(time.Now().UTC()) {
		t.Fatalf("expected future fire_at, got %v", nextFire)
	}
	// Backoff should be 2^1 * 5 = 10 seconds for attempt 1.
	if nextFire.Before(time.Now().UTC().Add(9*time.Second)) || nextFire.After(time.Now().UTC().Add(20*time.Second)) {
		t.Fatalf("unexpected backoff: %v", nextFire.Sub(time.Now().UTC()))
	}
}

// TestDeliverBacksOffOnTransientReminderLookupError guards against a
// regression where a transient reminder-lookup error (e.g. a DB hiccup, as
// opposed to domain.ErrNotFound) went straight to MarkFailed without
// advancing fire_at. Because MarkFailed leaves fire_at untouched while
// attempts < maxAttempts, the notification stayed 'pending' with a past
// fire_at and LeasePending would re-lease it on every subsequent tick — a
// tight busy-retry loop instead of the exponential backoff used for send
// failures.
func TestDeliverBacksOffOnTransientReminderLookupError(t *testing.T) {
	remID := uuid.New()
	notifID := uuid.New()
	notif := domain.ScheduledNotification{ID: notifID, ReminderID: remID, FireAt: time.Now().UTC(), Attempts: 0}

	store := &mockNotifStore{lease: []domain.ScheduledNotification{notif}}
	remStore := &mockReminderStore{err: errors.New("db hiccup")}
	sender := &mockSender{}

	w := NewWorker(store, remStore, sender, "test", time.Second, testLog)
	w.deliver(context.Background(), notif)

	if _, ok := store.markFail[notifID]; ok {
		t.Fatal("a transient lookup error should back off, not mark the notification permanently failed")
	}
	nextFire, ok := store.updateFa[notifID]
	if !ok {
		t.Fatal("expected ScheduleRetry to be called for backoff")
	}
	if !nextFire.After(time.Now().UTC()) {
		t.Fatalf("expected a future fire_at, got %v", nextFire)
	}
}

// TestDeliverDropsNotificationWhenReminderDeleted verifies that a
// domain.ErrNotFound reminder lookup (the reminder was deleted) still marks
// the notification permanently failed immediately, since retrying can never
// succeed.
func TestDeliverDropsNotificationWhenReminderDeleted(t *testing.T) {
	remID := uuid.New()
	notifID := uuid.New()
	notif := domain.ScheduledNotification{ID: notifID, ReminderID: remID, FireAt: time.Now().UTC(), Attempts: 0}

	store := &mockNotifStore{lease: []domain.ScheduledNotification{notif}}
	remStore := &mockReminderStore{err: domain.ErrNotFound}
	sender := &mockSender{}

	w := NewWorker(store, remStore, sender, "test", time.Second, testLog)
	w.deliver(context.Background(), notif)

	if len(store.updateFa) > 0 {
		t.Fatal("a deleted reminder should not be retried via backoff")
	}
	attempts, ok := store.markFail[notifID]
	if !ok || attempts < maxAttempts {
		t.Fatalf("expected notification marked failed at maxAttempts, got attempts=%d ok=%v", attempts, ok)
	}
}

func TestDeliverMarksFailedAfterMaxAttempts(t *testing.T) {
	remID := uuid.New()
	notifID := uuid.New()
	notif := domain.ScheduledNotification{ID: notifID, ReminderID: remID, FireAt: time.Now().UTC(), Attempts: 4}

	store := &mockNotifStore{lease: []domain.ScheduledNotification{notif}}
	remStore := &mockReminderStore{rem: &domain.Reminder{ID: remID, UserID: 123}}
	sender := &mockSender{err: errSendFailed}

	w := NewWorker(store, remStore, sender, "test", time.Second, testLog)
	w.deliver(context.Background(), notif)

	if len(store.updateFa) > 0 {
		t.Fatal("ScheduleRetry should NOT be called at max attempts")
	}
	attempts, ok := store.markFail[notifID]
	if !ok {
		t.Fatal("expected notification to be marked as failed")
	}
	if attempts != 5 {
		t.Fatalf("expected 5 attempts, got %d", attempts)
	}
}
