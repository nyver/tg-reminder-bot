package postgres

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

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
