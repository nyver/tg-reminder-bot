package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/domain"
)

// NotificationActionRepo stores the idempotency and audit record for a callback.
type NotificationActionRepo struct{ db *DB }

func NewNotificationActionRepo(db *DB) *NotificationActionRepo {
	return &NotificationActionRepo{db: db}
}

func (r *NotificationActionRepo) Record(ctx context.Context, action *domain.NotificationAction) error {
	if action.ID == uuid.Nil {
		action.ID = uuid.New()
	}
	if action.Payload == nil {
		action.Payload = json.RawMessage("{}")
	}
	action.CreatedAt = time.Now().UTC()
	res, err := r.db.ExecContext(ctx, r.db.Rebind(`
		INSERT INTO notification_actions
			(id, notification_id, user_id, action, payload, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (notification_id, user_id, action) DO NOTHING`),
		action.ID.String(), action.NotificationID.String(), action.UserID,
		action.Action, string(action.Payload), action.CreatedAt)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return domain.ErrAlreadyExists
	}
	return nil
}
