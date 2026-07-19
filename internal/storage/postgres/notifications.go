package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	var parentID *string
	if n.ParentNotificationID != nil && *n.ParentNotificationID != uuid.Nil {
		value := n.ParentNotificationID.String()
		parentID = &value
	}

	_, err := r.db.ExecContext(ctx, r.db.Rebind(`
		INSERT INTO scheduled_notifications
			(id, reminder_id, fire_at, text, idempotency_key, status, attempts, created_at, parent_notification_id)
		VALUES ($1,$2,$3,$4,$5,'pending',0,$6,$7)
		ON CONFLICT (idempotency_key) DO NOTHING`),
		n.ID.String(), n.ReminderID.String(), n.FireAt.UTC(), n.Text, n.IdempotencyKey, n.CreatedAt, NullString(parentID))
	return err
}

// LeasePending locks up to limit pending notifications that are ready to fire.
// SELECT and UPDATE run in the same transaction so no other worker can steal rows
// between the two statements.
func (r *NotificationRepo) LeasePending(ctx context.Context, workerID string, limit int) ([]domain.ScheduledNotification, error) {
	skipLocked := r.db.ForUpdateSkipLocked()
	minutesAgo := r.db.MinutesAgo(2)
	now := r.db.Now()

	selectQ := r.db.Rebind(`
		SELECT id, reminder_id, fire_at, text, idempotency_key, status, attempts, created_at, sent_at, parent_notification_id
		FROM scheduled_notifications
		WHERE status = 'pending' AND fire_at <= ` + now + `
		  AND (locked_at IS NULL OR locked_at < ` + minutesAgo + `)
		ORDER BY fire_at
		LIMIT $1 ` + skipLocked)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.QueryContext(ctx, selectQ, limit)
	if err != nil {
		return nil, err
	}
	result, err := scanNotifications(rows)
	rows.Close() // must close before UPDATE within the same tx
	if err != nil {
		return nil, err
	}

	if len(result) > 0 {
		ids := make([]string, len(result))
		for i, n := range result {
			ids[i] = n.ID.String()
		}
		in := r.db.InClause(2, len(ids))
		args := make([]any, 0, 1+len(ids))
		args = append(args, workerID)
		for _, id := range ids {
			args = append(args, id)
		}
		updateQ := r.db.Rebind(fmt.Sprintf(
			`UPDATE scheduled_notifications SET locked_at=%s, locked_by=$1 WHERE id %s`, now, in))
		if _, err := tx.ExecContext(ctx, updateQ, args...); err != nil {
			return nil, err
		}
	}

	return result, tx.Commit()
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

// ScheduleRetry atomically records a failed attempt, schedules the next
// delivery and releases the lease.
func (r *NotificationRepo) ScheduleRetry(ctx context.Context, id uuid.UUID, attempts int, fireAt time.Time) error {
	_, err := r.db.ExecContext(ctx, r.db.Rebind(`
		UPDATE scheduled_notifications
		SET attempts=$1, fire_at=$2, locked_at=NULL, locked_by=NULL
		WHERE id=$3`),
		attempts, fireAt.UTC(), id.String())
	return err
}

func (r *NotificationRepo) ListFailed(ctx context.Context, limit int) ([]domain.ScheduledNotification, error) {
	rows, err := r.db.QueryContext(ctx, r.db.Rebind(`
		SELECT id, reminder_id, fire_at, text, idempotency_key, status, attempts, created_at, sent_at, parent_notification_id
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
		SET status='pending', attempts=0, locked_at=NULL, locked_by=NULL
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
		SELECT id, reminder_id, fire_at, text, idempotency_key, status, attempts, created_at, sent_at, parent_notification_id
		FROM scheduled_notifications WHERE id=$1`), id.String())

	var n domain.ScheduledNotification
	var idStr, remIDStr string
	var sentAt sql.NullTime
	var parentID sql.NullString
	err := row.Scan(&idStr, &remIDStr, &n.FireAt, &n.Text,
		&n.IdempotencyKey, &n.Status, &n.Attempts, &n.CreatedAt, &sentAt, &parentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	parsedID, err := parseUUID(idStr)
	if err != nil {
		return nil, fmt.Errorf("get notification: %w", err)
	}
	remID, err := parseUUID(remIDStr)
	if err != nil {
		return nil, fmt.Errorf("get notification: %w", err)
	}
	n.ID = parsedID
	n.ReminderID = remID
	n.SentAt = PtrTime(sentAt)
	if parentID.Valid {
		parsed, err := parseUUID(parentID.String)
		if err != nil {
			return nil, fmt.Errorf("get parent notification: %w", err)
		}
		n.ParentNotificationID = &parsed
	}
	return &n, nil
}

func scanNotifications(rows *sql.Rows) ([]domain.ScheduledNotification, error) {
	var result []domain.ScheduledNotification
	for rows.Next() {
		var n domain.ScheduledNotification
		var idStr, remIDStr string
		var sentAt sql.NullTime
		var parentID sql.NullString
		if err := rows.Scan(&idStr, &remIDStr, &n.FireAt, &n.Text,
			&n.IdempotencyKey, &n.Status, &n.Attempts, &n.CreatedAt, &sentAt, &parentID); err != nil {
			return nil, err
		}
		id, err := parseUUID(idStr)
		if err != nil {
			return nil, fmt.Errorf("scan notification: %w", err)
		}
		remID, err := parseUUID(remIDStr)
		if err != nil {
			return nil, fmt.Errorf("scan notification: %w", err)
		}
		n.ID = id
		n.ReminderID = remID
		n.SentAt = PtrTime(sentAt)
		if parentID.Valid {
			parent, err := parseUUID(parentID.String)
			if err != nil {
				return nil, fmt.Errorf("scan parent notification: %w", err)
			}
			n.ParentNotificationID = &parent
		}
		result = append(result, n)
	}
	return result, rows.Err()
}
