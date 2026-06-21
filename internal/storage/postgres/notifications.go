package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/domain"
)

const maxAttempts = 5

type NotificationRepo struct{ db *DB }

func NewNotificationRepo(db *DB) *NotificationRepo { return &NotificationRepo{db: db} }

// Enqueue idempotently inserts a notification. Duplicate idempotency keys are silently ignored.
func (r *NotificationRepo) Enqueue(ctx context.Context, n *domain.ScheduledNotification) error {
	if n.ID == uuid.Nil {
		n.ID = uuid.New()
	}
	n.CreatedAt = time.Now().UTC()

	_, err := r.db.ExecContext(ctx, r.db.Rebind(`
		INSERT INTO scheduled_notifications
			(id, reminder_id, fire_at, text, idempotency_key, status, attempts, created_at)
		VALUES ($1,$2,$3,$4,$5,'pending',0,$6)
		ON CONFLICT (idempotency_key) DO NOTHING`),
		n.ID.String(), n.ReminderID.String(), n.FireAt, n.Text, n.IdempotencyKey, n.CreatedAt)
	return err
}

// LeasePending locks up to limit pending notifications that are ready to fire.
func (r *NotificationRepo) LeasePending(ctx context.Context, workerID string, limit int) ([]domain.ScheduledNotification, error) {
	skipLocked := r.db.ForUpdateSkipLocked()
	minutesAgo := r.db.MinutesAgo(2)
	now := r.db.Now()

	q := r.db.Rebind(`
		SELECT id, reminder_id, fire_at, text, idempotency_key, status, attempts, created_at, sent_at
		FROM scheduled_notifications
		WHERE status = 'pending' AND fire_at <= ` + now + `
		  AND (locked_at IS NULL OR locked_at < ` + minutesAgo + `)
		ORDER BY fire_at
		LIMIT $1 ` + skipLocked)

	rows, err := r.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []domain.ScheduledNotification
	for rows.Next() {
		var n domain.ScheduledNotification
		var idStr, remIDStr string
		var sentAt sql.NullTime
		if err := rows.Scan(
			&idStr, &remIDStr, &n.FireAt, &n.Text,
			&n.IdempotencyKey, &n.Status, &n.Attempts, &n.CreatedAt, &sentAt,
		); err != nil {
			return nil, err
		}
		n.ID = mustParseUUID(idStr)
		n.ReminderID = mustParseUUID(remIDStr)
		n.SentAt = PtrTime(sentAt)
		result = append(result, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	ids := make([]string, len(result))
	for i, n := range result {
		ids[i] = n.ID.String()
	}
	return result, r.db.ExecUpdateLocked(ctx, "scheduled_notifications", workerID, ids)
}

func (r *NotificationRepo) MarkSent(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, r.db.Rebind(`
		UPDATE scheduled_notifications
		SET status='sent', sent_at=$1, locked_at=NULL, locked_by=NULL
		WHERE id=$2`),
		time.Now().UTC(), id.String())
	return err
}

func (r *NotificationRepo) MarkFailed(ctx context.Context, id uuid.UUID, attempts int) error {
	status := "pending"
	if attempts >= maxAttempts {
		status = "failed"
	}
	_, err := r.db.ExecContext(ctx, r.db.Rebind(`
		UPDATE scheduled_notifications
		SET status=$1, attempts=$2, locked_at=NULL, locked_by=NULL
		WHERE id=$3`),
		status, attempts, id.String())
	return err
}

func (r *NotificationRepo) ListFailed(ctx context.Context, limit int) ([]domain.ScheduledNotification, error) {
	rows, err := r.db.QueryContext(ctx, r.db.Rebind(`
		SELECT id, reminder_id, fire_at, text, idempotency_key, status, attempts, created_at, sent_at
		FROM scheduled_notifications WHERE status='failed'
		ORDER BY created_at DESC LIMIT $1`), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNotifications(rows)
}

func (r *NotificationRepo) Retry(ctx context.Context, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx, r.db.Rebind(`
		UPDATE scheduled_notifications
		SET status='pending', attempts=0, locked_at=NULL
		WHERE id=$1 AND status='failed'`),
		id.String())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *NotificationRepo) Get(ctx context.Context, id uuid.UUID) (*domain.ScheduledNotification, error) {
	row := r.db.QueryRowContext(ctx, r.db.Rebind(`
		SELECT id, reminder_id, fire_at, text, idempotency_key, status, attempts, created_at, sent_at
		FROM scheduled_notifications WHERE id=$1`), id.String())

	var n domain.ScheduledNotification
	var idStr, remIDStr string
	var sentAt sql.NullTime
	err := row.Scan(&idStr, &remIDStr, &n.FireAt, &n.Text,
		&n.IdempotencyKey, &n.Status, &n.Attempts, &n.CreatedAt, &sentAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	n.ID = mustParseUUID(idStr)
	n.ReminderID = mustParseUUID(remIDStr)
	n.SentAt = PtrTime(sentAt)
	return &n, nil
}

func scanNotifications(rows *sql.Rows) ([]domain.ScheduledNotification, error) {
	var result []domain.ScheduledNotification
	for rows.Next() {
		var n domain.ScheduledNotification
		var idStr, remIDStr string
		var sentAt sql.NullTime
		if err := rows.Scan(&idStr, &remIDStr, &n.FireAt, &n.Text,
			&n.IdempotencyKey, &n.Status, &n.Attempts, &n.CreatedAt, &sentAt); err != nil {
			return nil, err
		}
		n.ID = mustParseUUID(idStr)
		n.ReminderID = mustParseUUID(remIDStr)
		n.SentAt = PtrTime(sentAt)
		result = append(result, n)
	}
	return result, rows.Err()
}
