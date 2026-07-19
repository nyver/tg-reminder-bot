package postgres

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/domain"
)

func TestReminderRepoRemove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := New(ctx, "sqlite", filepath.Join(t.TempDir(), "remove.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schema := `
		CREATE TABLE users (id INTEGER PRIMARY KEY);
		CREATE TABLE reminders (id TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id));
		CREATE TABLE scheduled_notifications (
			id TEXT PRIMARY KEY,
			reminder_id TEXT NOT NULL REFERENCES reminders(id) ON DELETE CASCADE
		);
		CREATE TABLE observations (
			id TEXT PRIMARY KEY,
			reminder_id TEXT NOT NULL REFERENCES reminders(id) ON DELETE CASCADE
		);`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		t.Fatal(err)
	}

	reminderID := uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES (1), (2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO reminders (id, user_id) VALUES (?, 1)`, reminderID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO scheduled_notifications (id, reminder_id) VALUES (?, ?)`, uuid.NewString(), reminderID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO observations (id, reminder_id) VALUES (?, ?)`, uuid.NewString(), reminderID.String()); err != nil {
		t.Fatal(err)
	}

	repo := NewReminderRepo(db)
	if err := repo.Remove(ctx, 2, reminderID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("remove as another user: %v", err)
	}
	if err := repo.Remove(ctx, 1, reminderID); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{"reminders", "scheduled_notifications", "observations"} {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("%s contains %d rows after removal", table, count)
		}
	}
}

func TestMarkConditionalDuePreservesScheduledConditionalReminders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := New(ctx, "sqlite", filepath.Join(t.TempDir(), "due.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schema := `
		CREATE TABLE reminders (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			spec TEXT NOT NULL,
			status TEXT NOT NULL,
			next_eval_at TIMESTAMP,
			locked_at TIMESTAMP,
			locked_by TEXT
		);`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		t.Fatal(err)
	}

	anchorID := uuid.NewString()
	digestID := uuid.NewString()
	weatherID := uuid.NewString()
	future := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO reminders (id, kind, spec, status, next_eval_at, locked_at, locked_by)
		 VALUES (?, 'conditional', '{"trigger":"anchor","event":{"type":"tv_program"}}', 'active', ?, ?, 'old-worker')`,
		anchorID, future, future,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO reminders (id, kind, spec, status, next_eval_at, locked_at, locked_by)
		 VALUES (?, 'conditional', '{"trigger":"anchor","event":{"type":"weather"}}', 'active', ?, ?, 'old-worker')`,
		weatherID, future, future,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO reminders (id, kind, spec, status, next_eval_at, locked_at, locked_by)
		 VALUES (?, 'conditional', '{"trigger":"digest","event":{"type":"rss"}}', 'active', ?, ?, 'old-worker')`,
		digestID, future, future,
	); err != nil {
		t.Fatal(err)
	}

	repo := NewReminderRepo(db)
	if err := repo.MarkConditionalDue(ctx); err != nil {
		t.Fatal(err)
	}

	var anchorLocked sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT locked_by FROM reminders WHERE id = ?`, anchorID).Scan(&anchorLocked); err != nil {
		t.Fatal(err)
	}
	if anchorLocked.Valid {
		t.Fatalf("anchor locked_by still set: %q", anchorLocked.String)
	}

	var digestNext time.Time
	var digestLocked sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT next_eval_at, locked_by FROM reminders WHERE id = ?`, digestID).Scan(&digestNext, &digestLocked); err != nil {
		t.Fatal(err)
	}
	if !digestNext.Equal(future) {
		t.Fatalf("digest next_eval_at = %v, want unchanged %v", digestNext, future)
	}
	if digestLocked.Valid {
		t.Fatalf("digest locked_by still set: %q", digestLocked.String)
	}

	var weatherNext time.Time
	var weatherLocked sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT next_eval_at, locked_by FROM reminders WHERE id = ?`, weatherID).Scan(&weatherNext, &weatherLocked); err != nil {
		t.Fatal(err)
	}
	if !weatherNext.Equal(future) {
		t.Fatalf("weather next_eval_at = %v, want unchanged %v", weatherNext, future)
	}
	if weatherLocked.Valid {
		t.Fatalf("weather locked_by still set: %q", weatherLocked.String)
	}
}

func TestReminderRepoLeaseDueNormalizesOffsetTimeToUTC(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := New(ctx, "sqlite", filepath.Join(t.TempDir(), "utc-reminder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schema := `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			tz TEXT NOT NULL
		);
		CREATE TABLE reminders (
			id TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			raw_text TEXT NOT NULL,
			spec TEXT NOT NULL,
			status TEXT NOT NULL,
			eval_cron TEXT,
			next_eval_at DATETIME,
			idempotency_key TEXT,
			locked_at DATETIME,
			locked_by TEXT,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			version INTEGER NOT NULL DEFAULT 1
		);
		CREATE UNIQUE INDEX idx_reminders_idempotency
			ON reminders (idempotency_key) WHERE idempotency_key IS NOT NULL;`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id, tz) VALUES (42, 'Europe/Moscow')`); err != nil {
		t.Fatal(err)
	}

	moscow := time.FixedZone("MSK", 3*60*60)
	due := time.Now().Add(-time.Minute).In(moscow)
	rem := &domain.Reminder{
		UserID:         42,
		Kind:           domain.KindConditional,
		RawText:        "RSS digest",
		Spec:           domain.Spec{Trigger: domain.TriggerDigest, Event: domain.EventSpec{Type: "rss"}},
		Status:         domain.StatusActive,
		EvalCron:       "0 18 * * *",
		NextEvalAt:     &due,
		IdempotencyKey: uuid.NewString(),
	}
	repo := NewReminderRepo(db)
	if err := repo.Create(ctx, rem); err != nil {
		t.Fatal(err)
	}

	leased, err := repo.LeaseDue(ctx, "worker-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(leased) != 1 {
		t.Fatalf("leased reminders = %d, want 1; offset time was not stored as UTC", len(leased))
	}
	if leased[0].NextEvalAt == nil || !leased[0].NextEvalAt.Equal(due) {
		t.Fatalf("next_eval_at = %v, want instant %v", leased[0].NextEvalAt, due)
	}
}
