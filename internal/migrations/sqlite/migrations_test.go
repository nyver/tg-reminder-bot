package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

func TestNormalizeScheduledTimesUTC(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "migrations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	goose.SetBaseFS(FS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", 4); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	reminderID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO reminders
		(id, user_id, kind, raw_text, spec, status, next_eval_at)
		VALUES (?, 1, 'conditional', 'RSS digest', '{"trigger":"digest"}', 'active', ?)`,
		reminderID, "2026-07-19 18:00:00 +0300 MSK"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO scheduled_notifications
		(id, reminder_id, fire_at, text, idempotency_key)
		VALUES (?, ?, ?, 'digest', ?)`, uuid.NewString(), reminderID,
		"2026-07-19 18:00:00.123456789 +0300 MSK", uuid.NewString()); err != nil {
		t.Fatal(err)
	}

	if err := goose.Up(db, "."); err != nil {
		t.Fatal(err)
	}

	var nextEvalAt, fireAt string
	if err := db.QueryRowContext(ctx, `SELECT next_eval_at FROM reminders WHERE id = ?`, reminderID).Scan(&nextEvalAt); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT fire_at FROM scheduled_notifications WHERE reminder_id = ?`, reminderID).Scan(&fireAt); err != nil {
		t.Fatal(err)
	}
	const want = "2026-07-19T15:00:00Z"
	if nextEvalAt != want {
		t.Fatalf("next_eval_at = %q, want %q", nextEvalAt, want)
	}
	if fireAt != want {
		t.Fatalf("fire_at = %q, want %q", fireAt, want)
	}
}
