package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/domain"
)

type ReminderRepo struct{ db *DB }

func NewReminderRepo(db *DB) *ReminderRepo { return &ReminderRepo{db: db} }

func (r *ReminderRepo) Create(ctx context.Context, rem *domain.Reminder) error {
	specJSON, err := json.Marshal(rem.Spec)
	if err != nil {
		return err
	}
	if rem.ID == uuid.Nil {
		rem.ID = uuid.New()
	}
	now := time.Now().UTC()
	rem.CreatedAt = now
	rem.UpdatedAt = now

	const q = `
		INSERT INTO reminders
			(id, user_id, kind, raw_text, spec, status, eval_cron, next_eval_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`

	var evalCron *string
	if rem.EvalCron != "" {
		evalCron = &rem.EvalCron
	}
	_, err = r.db.ExecContext(ctx, r.db.Rebind(q),
		rem.ID.String(), rem.UserID, string(rem.Kind), rem.RawText,
		string(specJSON), string(rem.Status),
		NullString(evalCron), NullTime(rem.NextEvalAt),
		now, now,
	)
	return err
}

func (r *ReminderRepo) Get(ctx context.Context, id uuid.UUID) (*domain.Reminder, error) {
	const q = `
		SELECT id, user_id, kind, raw_text, spec, status, eval_cron, next_eval_at, created_at, updated_at
		FROM reminders WHERE id = $1`
	row := r.db.QueryRowContext(ctx, r.db.Rebind(q), id.String())
	rem, err := scanReminder(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return rem, err
}

func (r *ReminderRepo) ListByUser(ctx context.Context, userID int64) ([]domain.Reminder, error) {
	const q = `
		SELECT id, user_id, kind, raw_text, spec, status, eval_cron, next_eval_at, created_at, updated_at
		FROM reminders WHERE user_id = $1 AND status != 'done'
		ORDER BY created_at DESC`
	rows, err := r.db.QueryContext(ctx, r.db.Rebind(q), userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReminders(rows)
}

// LeaseDue returns reminders due for evaluation and marks them locked.
func (r *ReminderRepo) LeaseDue(ctx context.Context, workerID string, limit int) ([]domain.Reminder, error) {
	skipLocked := r.db.ForUpdateSkipLocked()
	minutesAgo := r.db.MinutesAgo(5)
	now := r.db.Now()

	q := r.db.Rebind(`
		SELECT id, user_id, kind, raw_text, spec, status, eval_cron, next_eval_at, created_at, updated_at
		FROM reminders
		WHERE status = 'active' AND next_eval_at <= ` + now + `
		  AND (locked_at IS NULL OR locked_at < ` + minutesAgo + `)
		ORDER BY next_eval_at
		LIMIT $1 ` + skipLocked)

	rows, err := r.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rems, err := scanReminders(rows)
	if err != nil {
		return nil, err
	}

	ids := make([]string, len(rems))
	for i, rem := range rems {
		ids[i] = rem.ID.String()
	}
	return rems, r.db.ExecUpdateLocked(ctx, "reminders", workerID, ids)
}

func (r *ReminderRepo) UpdateNextEval(ctx context.Context, id uuid.UUID, next *time.Time) error {
	q := r.db.Rebind(`
		UPDATE reminders SET next_eval_at=$1, locked_at=NULL, locked_by=NULL, updated_at=$2
		WHERE id=$3`)
	_, err := r.db.ExecContext(ctx, q, NullTime(next), time.Now().UTC(), id.String())
	return err
}

func (r *ReminderRepo) UpdateStatus(ctx context.Context, id uuid.UUID, status domain.Status) error {
	_, err := r.db.ExecContext(ctx,
		r.db.Rebind(`UPDATE reminders SET status=$1, updated_at=$2 WHERE id=$3`),
		string(status), time.Now().UTC(), id.String())
	return err
}

func (r *ReminderRepo) Cancel(ctx context.Context, userID int64, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx,
		r.db.Rebind(`UPDATE reminders SET status='done', updated_at=$1 WHERE id=$2 AND user_id=$3`),
		time.Now().UTC(), id.String(), userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Remove permanently deletes a user's reminder and its dependent records.
func (r *ReminderRepo) Remove(ctx context.Context, userID int64, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx,
		r.db.Rebind(`DELETE FROM reminders WHERE id=$1 AND user_id=$2`),
		id.String(), userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *ReminderRepo) Pause(ctx context.Context, userID int64, id uuid.UUID, pause bool) error {
	status := domain.StatusActive
	if pause {
		status = domain.StatusPaused
	}
	res, err := r.db.ExecContext(ctx,
		r.db.Rebind(`UPDATE reminders SET status=$1, updated_at=$2 WHERE id=$3 AND user_id=$4`),
		string(status), time.Now().UTC(), id.String(), userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanReminder(row rowScanner) (*domain.Reminder, error) {
	rem := &domain.Reminder{}
	var idStr string
	var specJSON []byte
	var evalCron sql.NullString
	var nextEvalAt sql.NullTime

	if err := row.Scan(
		&idStr, &rem.UserID, &rem.Kind, &rem.RawText,
		&specJSON, &rem.Status, &evalCron, &nextEvalAt,
		&rem.CreatedAt, &rem.UpdatedAt,
	); err != nil {
		return nil, err
	}
	rem.ID = mustParseUUID(idStr)
	rem.EvalCron = evalCron.String
	rem.NextEvalAt = PtrTime(nextEvalAt)
	if err := json.Unmarshal(specJSON, &rem.Spec); err != nil {
		return nil, err
	}
	return rem, nil
}

func scanReminders(rows *sql.Rows) ([]domain.Reminder, error) {
	var result []domain.Reminder
	for rows.Next() {
		rem, err := scanReminder(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *rem)
	}
	return result, rows.Err()
}

func mustParseUUID(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil
	}
	return id
}
