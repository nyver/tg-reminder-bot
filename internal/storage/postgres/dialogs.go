package postgres

import (
	"context"
	"encoding/json"

	"github.com/nyver2k/remindertgbot/internal/domain"
)

type DialogRepo struct{ db *DB }

func NewDialogRepo(db *DB) *DialogRepo { return &DialogRepo{db: db} }

func (r *DialogRepo) Get(ctx context.Context, userID int64) (*domain.Dialog, error) {
	row := r.db.QueryRowContext(ctx, r.db.Rebind(
		`SELECT user_id, state, context, updated_at FROM dialog_states WHERE user_id=$1`),
		userID)

	d := &domain.Dialog{}
	var ctxStr string
	err := row.Scan(&d.UserID, &d.State, &ctxStr, &d.UpdatedAt)
	if err != nil {
		// Return empty idle state for first-time users.
		return &domain.Dialog{
			UserID:  userID,
			State:   domain.DialogIdle,
			Context: json.RawMessage("{}"),
		}, nil
	}
	d.Context = json.RawMessage(ctxStr)
	return d, nil
}

func (r *DialogRepo) Set(ctx context.Context, d *domain.Dialog) error {
	ctxStr := "{}"
	if d.Context != nil {
		ctxStr = string(d.Context)
	}
	now := r.db.Now()
	_, err := r.db.ExecContext(ctx, r.db.Rebind(`
		INSERT INTO dialog_states (user_id, state, context, updated_at)
		VALUES ($1,$2,$3,`+now+`)
		ON CONFLICT (user_id) DO UPDATE
		SET state=excluded.state, context=excluded.context, updated_at=`+now),
		d.UserID, string(d.State), ctxStr)
	return err
}

func (r *DialogRepo) Reset(ctx context.Context, userID int64) error {
	return r.Set(ctx, &domain.Dialog{
		UserID:  userID,
		State:   domain.DialogIdle,
		Context: json.RawMessage("{}"),
	})
}
