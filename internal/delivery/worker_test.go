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
func (m *mockNotifStore) UpdateFireAt(ctx context.Context, id uuid.UUID, fireAt time.Time) error {
	if m.updateFa == nil {
		m.updateFa = map[uuid.UUID]time.Time{}
	}
	m.updateFa[id] = fireAt
	return nil
}

type mockReminderStore struct{ rem *domain.Reminder }

func (m *mockReminderStore) Get(ctx context.Context, id uuid.UUID) (*domain.Reminder, error) {
	return m.rem, nil
}

type mockSender struct{ err error }

func (m *mockSender) Send(ctx context.Context, userID int64, text string) error {
	return m.err
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
	// Check that UpdateFireAt was called with a future time (backoff).
	if len(store.updateFa) == 0 {
		t.Fatal("expected UpdateFireAt to be called")
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
		t.Fatal("UpdateFireAt should NOT be called at max attempts")
	}
	attempts, ok := store.markFail[notifID]
	if !ok {
		t.Fatal("expected notification to be marked as failed")
	}
	if attempts != 5 {
		t.Fatalf("expected 5 attempts, got %d", attempts)
	}
}
