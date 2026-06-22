package postgres

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHousekeepingRepo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := New(ctx, "sqlite", filepath.Join(t.TempDir(), "hk.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schema := `
		CREATE TABLE users (id INTEGER PRIMARY KEY, tz TEXT NOT NULL DEFAULT 'Europe/Moscow', created_at DATETIME NOT NULL DEFAULT (datetime('now')));
		CREATE TABLE reminders (
			id TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id),
			kind TEXT NOT NULL DEFAULT 'conditional', raw_text TEXT NOT NULL DEFAULT '',
			spec TEXT NOT NULL DEFAULT '{}', status TEXT NOT NULL DEFAULT 'active',
			eval_cron TEXT, next_eval_at DATETIME, idempotency_key TEXT,
			locked_at DATETIME, locked_by TEXT,
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE scheduled_notifications (
			id TEXT PRIMARY KEY, reminder_id TEXT NOT NULL REFERENCES reminders(id) ON DELETE CASCADE,
			fire_at DATETIME NOT NULL, text TEXT NOT NULL DEFAULT '',
			idempotency_key TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0,
			locked_at DATETIME, locked_by TEXT, sent_at DATETIME,
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			UNIQUE (idempotency_key)
		);
		CREATE TABLE observations (
			id TEXT PRIMARY KEY, reminder_id TEXT NOT NULL REFERENCES reminders(id) ON DELETE CASCADE,
			value INTEGER NOT NULL DEFAULT 0, currency TEXT NOT NULL DEFAULT 'RUB',
			available INTEGER NOT NULL DEFAULT 1, raw TEXT,
			observed_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE provider_cache (
			key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '{}',
			fetched_at DATETIME NOT NULL DEFAULT (datetime('now')),
			ttl_seconds INTEGER NOT NULL DEFAULT 60
		);`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	recent := now.Add(-1 * time.Hour)
	notifOld := now.Add(-8 * 24 * time.Hour)   // 8 days ago — beyond 7-day notification retention
	reminderOld := now.Add(-31 * 24 * time.Hour) // 31 days ago — beyond 30-day reminder retention
	obsOld := now.Add(-48 * time.Hour)           // 2 days ago — beyond 1-day observation safety window

	if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}

	// reminders: one active, one done-old (>30d), one done-recent (<30d)
	activeID := uuid.NewString()
	doneOldID := uuid.NewString()
	doneRecentID := uuid.NewString()
	for _, row := range []struct {
		id, status string
		updatedAt  time.Time
	}{
		{activeID, "active", recent},
		{doneOldID, "done", reminderOld},
		{doneRecentID, "done", recent},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO reminders (id, user_id, status, updated_at) VALUES (?,1,?,?)`,
			row.id, row.status, row.updatedAt); err != nil {
			t.Fatal(err)
		}
	}

	// notifications attached to activeID
	for _, row := range []struct {
		id, status string
		fireAt     time.Time
	}{
		{uuid.NewString(), "sent", notifOld},    // old sent → deleted
		{uuid.NewString(), "failed", notifOld},  // old failed → deleted
		{uuid.NewString(), "sent", recent},      // recent sent → kept
		{uuid.NewString(), "pending", notifOld}, // old pending → kept (only sent/failed pruned)
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO scheduled_notifications (id, reminder_id, fire_at, idempotency_key, status) VALUES (?,?,?,?,?)`,
			row.id, activeID, row.fireAt, row.id, row.status); err != nil {
			t.Fatal(err)
		}
	}

	// observations: 15 old for activeID, only 10 should survive; plus 1 recent (protected)
	for i := 0; i < 15; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO observations (id, reminder_id, observed_at) VALUES (?,?,?)`,
			uuid.NewString(), activeID, obsOld.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO observations (id, reminder_id, observed_at) VALUES (?,?,?)`,
		uuid.NewString(), activeID, recent); err != nil {
		t.Fatal(err)
	}

	// provider_cache: one expired (ttl=1s, inserted 8 days ago), one fresh.
	// Use SQLite datetime() so the stored format matches what ttlExpiry produces.
	if _, err := db.ExecContext(ctx, `INSERT INTO provider_cache (key, value, fetched_at, ttl_seconds)`+
		` VALUES ('expired','{}',`+db.DaysAgo(8)+`,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO provider_cache (key, value, fetched_at, ttl_seconds)`+
		` VALUES ('fresh','{}',`+db.Now()+`,86400)`); err != nil {
		t.Fatal(err)
	}

	repo := NewHousekeepingRepo(db)

	if n, err := repo.PruneNotifications(ctx, 7); err != nil {
		t.Fatalf("PruneNotifications: %v", err)
	} else if n != 2 {
		t.Errorf("PruneNotifications: deleted %d, want 2", n)
	}

	// 15 old observations; top-10 by recency = 1 recent + 9 old → 6 old deleted.
	if n, err := repo.PruneObservations(ctx, 10); err != nil {
		t.Fatalf("PruneObservations: %v", err)
	} else if n != 6 {
		t.Errorf("PruneObservations: deleted %d, want 6", n)
	}

	if n, err := repo.PruneExpiredCache(ctx); err != nil {
		t.Fatalf("PruneExpiredCache: %v", err)
	} else if n != 1 {
		t.Errorf("PruneExpiredCache: deleted %d, want 1", n)
	}

	if n, err := repo.PruneDoneReminders(ctx, 30); err != nil {
		t.Fatalf("PruneDoneReminders: %v", err)
	} else if n != 1 { // only doneOldID (31 days), doneRecentID is kept
		t.Errorf("PruneDoneReminders: deleted %d, want 1", n)
	}

	if err := repo.OptimizeDB(ctx); err != nil {
		t.Fatalf("OptimizeDB: %v", err)
	}
}
