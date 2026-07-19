package postgres

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/domain"
	sqlitemigrations "github.com/nyver2k/remindertgbot/internal/migrations/sqlite"
	"github.com/pressly/goose/v3"
)

func migratedUITestDB(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()
	db, err := New(ctx, "sqlite", filepath.Join(t.TempDir(), "telegram-ui.db"))
	if err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(sqlitemigrations.FS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := goose.Up(db.DB, "."); err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestReminderUpdateUsesOptimisticLockAndCancelsPending(t *testing.T) {
	db := migratedUITestDB(t)
	ctx := context.Background()
	if _, err := NewUserRepo(db).Upsert(ctx, 42, "UTC"); err != nil {
		t.Fatal(err)
	}
	next := time.Now().UTC().Add(time.Hour)
	reminder := &domain.Reminder{UserID: 42, Kind: domain.KindAbsolute, RawText: "old", Spec: domain.Spec{Message: "old"}, Status: domain.StatusActive, NextEvalAt: &next}
	repo := NewReminderRepo(db)
	if err := repo.Create(ctx, reminder); err != nil {
		t.Fatal(err)
	}
	notification := &domain.ScheduledNotification{ReminderID: reminder.ID, FireAt: next, Text: "old", IdempotencyKey: uuid.NewString()}
	if err := NewNotificationRepo(db).Enqueue(ctx, notification); err != nil {
		t.Fatal(err)
	}

	reminder.RawText = "new"
	reminder.Spec.Message = "new"
	if err := repo.Update(ctx, reminder, 1); err != nil {
		t.Fatal(err)
	}
	if reminder.Version != 2 {
		t.Fatalf("version = %d", reminder.Version)
	}
	if err := repo.Update(ctx, reminder, 1); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale update error = %v", err)
	}
	loaded, err := repo.Get(ctx, reminder.ID)
	if err != nil || loaded.RawText != "new" || loaded.Version != 2 {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	stored, err := NewNotificationRepo(db).Get(ctx, notification.ID)
	if err != nil || stored.Status != domain.NotificationCancelled {
		t.Fatalf("notification=%+v err=%v", stored, err)
	}
}

func TestUserPreferencesRepoAppliesDefaultsAndUpdates(t *testing.T) {
	db := migratedUITestDB(t)
	ctx := context.Background()
	if _, err := NewUserRepo(db).Upsert(ctx, 42, "UTC"); err != nil {
		t.Fatal(err)
	}
	repo := NewUserPreferencesRepo(db, domain.UserPreferences{
		MorningTime: "08:30", QuietStart: "22:00", QuietEnd: "07:00", DefaultSnoozeMinutes: 20,
	})
	prefs, err := repo.GetOrCreate(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if prefs.MorningTime != "08:30" || prefs.QuietStart != "22:00" || prefs.DefaultSnoozeMinutes != 20 {
		t.Fatalf("preferences = %+v", prefs)
	}
	prefs.QuietStart, prefs.QuietEnd, prefs.DefaultSnoozeMinutes = "", "", 30
	if err := repo.Update(ctx, *prefs); err != nil {
		t.Fatal(err)
	}
	updated, err := repo.Get(ctx, 42)
	if err != nil || updated.QuietStart != "" || updated.DefaultSnoozeMinutes != 30 {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
}

func TestNotificationActionRepoIsIdempotent(t *testing.T) {
	db := migratedUITestDB(t)
	ctx := context.Background()
	if _, err := NewUserRepo(db).Upsert(ctx, 42, "UTC"); err != nil {
		t.Fatal(err)
	}
	next := time.Now().UTC().Add(time.Hour)
	reminder := &domain.Reminder{UserID: 42, Kind: domain.KindAbsolute, RawText: "task", Spec: domain.Spec{Message: "task"}, Status: domain.StatusActive, NextEvalAt: &next}
	if err := NewReminderRepo(db).Create(ctx, reminder); err != nil {
		t.Fatal(err)
	}
	notification := &domain.ScheduledNotification{ReminderID: reminder.ID, FireAt: next, Text: "task", IdempotencyKey: uuid.NewString()}
	if err := NewNotificationRepo(db).Enqueue(ctx, notification); err != nil {
		t.Fatal(err)
	}
	action := &domain.NotificationAction{NotificationID: notification.ID, UserID: 42, Action: "done"}
	repo := NewNotificationActionRepo(db)
	if err := repo.Record(ctx, action); err != nil {
		t.Fatal(err)
	}
	if err := repo.Record(ctx, &domain.NotificationAction{NotificationID: notification.ID, UserID: 42, Action: "done"}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("duplicate action error = %v", err)
	}
}

func TestNotificationParentRoundTrip(t *testing.T) {
	db := migratedUITestDB(t)
	ctx := context.Background()
	if _, err := NewUserRepo(db).Upsert(ctx, 42, "UTC"); err != nil {
		t.Fatal(err)
	}
	next := time.Now().UTC().Add(time.Hour)
	reminder := &domain.Reminder{UserID: 42, Kind: domain.KindAbsolute, RawText: "task", Spec: domain.Spec{Message: "task"}, Status: domain.StatusActive, NextEvalAt: &next}
	if err := NewReminderRepo(db).Create(ctx, reminder); err != nil {
		t.Fatal(err)
	}
	repo := NewNotificationRepo(db)
	parent := &domain.ScheduledNotification{ReminderID: reminder.ID, FireAt: next, Text: "task", IdempotencyKey: uuid.NewString()}
	if err := repo.Enqueue(ctx, parent); err != nil {
		t.Fatal(err)
	}
	child := &domain.ScheduledNotification{ReminderID: reminder.ID, FireAt: next.Add(time.Hour), Text: "task", IdempotencyKey: uuid.NewString(), ParentNotificationID: &parent.ID}
	if err := repo.Enqueue(ctx, child); err != nil {
		t.Fatal(err)
	}
	loaded, err := repo.Get(ctx, child.ID)
	if err != nil || loaded.ParentNotificationID == nil || *loaded.ParentNotificationID != parent.ID {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
}
