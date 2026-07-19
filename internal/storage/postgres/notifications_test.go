package postgres

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/domain"
)

func TestNotificationRepoScheduleRetryPersistsAttemptAndReleasesLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := New(ctx, "sqlite", filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `CREATE TABLE scheduled_notifications (
		id TEXT PRIMARY KEY,
		attempts INTEGER NOT NULL,
		fire_at DATETIME NOT NULL,
		locked_at DATETIME,
		locked_by TEXT
	)`); err != nil {
		t.Fatal(err)
	}

	id := uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO scheduled_notifications
		(id, attempts, fire_at, locked_at, locked_by) VALUES (?, 0, ?, ?, 'worker-1')`,
		id.String(), time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	next := time.Now().UTC().Add(time.Minute).Truncate(time.Millisecond)
	if err := NewNotificationRepo(db).ScheduleRetry(ctx, id, 1, next); err != nil {
		t.Fatal(err)
	}

	var attempts int
	var fireAt time.Time
	var lockedAt, lockedBy any
	if err := db.QueryRowContext(ctx, `SELECT attempts, fire_at, locked_at, locked_by
		FROM scheduled_notifications WHERE id=?`, id.String()).Scan(&attempts, &fireAt, &lockedAt, &lockedBy); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if !fireAt.Equal(next) {
		t.Fatalf("fire_at = %v, want %v", fireAt, next)
	}
	if lockedAt != nil || lockedBy != nil {
		t.Fatalf("lease was not released: locked_at=%v locked_by=%v", lockedAt, lockedBy)
	}
}

func TestNotificationRepoLeasePendingNormalizesOffsetTimeToUTC(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := New(ctx, "sqlite", filepath.Join(t.TempDir(), "utc-notification.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `CREATE TABLE scheduled_notifications (
		id TEXT PRIMARY KEY,
		reminder_id TEXT NOT NULL,
		fire_at DATETIME NOT NULL,
		text TEXT NOT NULL,
		idempotency_key TEXT NOT NULL UNIQUE,
		status TEXT NOT NULL,
		attempts INTEGER NOT NULL,
		locked_at DATETIME,
		locked_by TEXT,
		sent_at DATETIME,
		created_at DATETIME NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}

	moscow := time.FixedZone("MSK", 3*60*60)
	due := time.Now().Add(-time.Minute).In(moscow)
	n := &domain.ScheduledNotification{
		ReminderID:     uuid.New(),
		FireAt:         due,
		Text:           "digest",
		IdempotencyKey: uuid.NewString(),
	}
	repo := NewNotificationRepo(db)
	if err := repo.Enqueue(ctx, n); err != nil {
		t.Fatal(err)
	}

	leased, err := repo.LeasePending(ctx, "worker-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(leased) != 1 {
		t.Fatalf("leased notifications = %d, want 1; offset time was not stored as UTC", len(leased))
	}
	if !leased[0].FireAt.Equal(due) {
		t.Fatalf("fire_at = %v, want instant %v", leased[0].FireAt, due)
	}
}
