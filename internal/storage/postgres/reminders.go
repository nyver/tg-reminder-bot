package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	rem.Version = 1

	const q = `
		INSERT INTO reminders
			(id, user_id, kind, raw_text, spec, status, eval_cron, next_eval_at, idempotency_key, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`

	var evalCron *string
	if rem.EvalCron != "" {
		evalCron = &rem.EvalCron
	}
	var idemKey *string
	if rem.IdempotencyKey != "" {
		idemKey = &rem.IdempotencyKey
	}
	result, err := r.db.ExecContext(ctx, r.db.Rebind(q),
		rem.ID.String(), rem.UserID, string(rem.Kind), rem.RawText,
		string(specJSON), string(rem.Status),
		NullString(evalCron), NullTime(rem.NextEvalAt),
		NullString(idemKey),
		now, now,
	)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return domain.ErrAlreadyExists
	}
	return nil
}

func (r *ReminderRepo) Get(ctx context.Context, id uuid.UUID) (*domain.Reminder, error) {
	const q = `
		SELECT id, user_id, kind, raw_text, spec, status, eval_cron, next_eval_at, created_at, updated_at, version
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
		SELECT id, user_id, kind, raw_text, spec, status, eval_cron, next_eval_at, created_at, updated_at, version
		FROM reminders WHERE user_id = $1 AND status NOT IN ('done', 'cancelled')
		ORDER BY created_at DESC`
	rows, err := r.db.QueryContext(ctx, r.db.Rebind(q), userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReminders(rows)
}

// LeaseDue returns reminders due for evaluation and marks them locked atomically.
// SELECT and UPDATE run in the same transaction so no other worker can steal rows
// between the two statements.
func (r *ReminderRepo) LeaseDue(ctx context.Context, workerID string, limit int) ([]domain.Reminder, error) {
	skipLocked := r.db.ForUpdateSkipLocked()
	minutesAgo := r.db.MinutesAgo(5)
	now := r.db.Now()

	selectQ := r.db.Rebind(`
		SELECT id, user_id, kind, raw_text, spec, status, eval_cron, next_eval_at, created_at, updated_at, version
		FROM reminders
		WHERE status = 'active' AND next_eval_at <= ` + now + `
		  AND (locked_at IS NULL OR locked_at < ` + minutesAgo + `)
		ORDER BY next_eval_at
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
	rems, err := scanReminders(rows)
	rows.Close() // must close before UPDATE within the same tx
	if err != nil {
		return nil, err
	}

	if len(rems) > 0 {
		ids := make([]string, len(rems))
		for i, rem := range rems {
			ids[i] = rem.ID.String()
		}
		in := r.db.InClause(2, len(ids))
		args := make([]any, 0, 1+len(ids))
		args = append(args, workerID)
		for _, id := range ids {
			args = append(args, id)
		}
		updateQ := r.db.Rebind(fmt.Sprintf(
			`UPDATE reminders SET locked_at=%s, locked_by=$1 WHERE id %s`, now, in))
		if _, err := tx.ExecContext(ctx, updateQ, args...); err != nil {
			return nil, err
		}

		// Bulk-fetch user TZs so the evaluator can use the correct timezone.
		if tzMap, err := fetchUserTZs(ctx, tx, r.db, rems); err == nil {
			for i := range rems {
				if tz, ok := tzMap[rems[i].UserID]; ok {
					rems[i].UserTZ = tz
				}
			}
		}
	}

	return rems, tx.Commit()
}

// fetchUserTZs returns a userID→tz map for all unique users in the batch.
func fetchUserTZs(ctx context.Context, tx interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, db *DB, rems []domain.Reminder) (map[int64]string, error) {
	seen := make(map[int64]struct{}, len(rems))
	uids := make([]any, 0, len(rems))
	for _, rem := range rems {
		if _, ok := seen[rem.UserID]; !ok {
			seen[rem.UserID] = struct{}{}
			uids = append(uids, rem.UserID)
		}
	}
	in := db.InClause(1, len(uids))
	q := db.Rebind(fmt.Sprintf("SELECT id, tz FROM users WHERE id %s", in))
	rows, err := tx.QueryContext(ctx, q, uids...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tzMap := make(map[int64]string, len(uids))
	for rows.Next() {
		var uid int64
		var tz string
		if rows.Scan(&uid, &tz) == nil {
			tzMap[uid] = tz
		}
	}
	return tzMap, rows.Err()
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
		r.db.Rebind(`UPDATE reminders SET status=$1, updated_at=$2, version=version+1 WHERE id=$3`),
		string(status), time.Now().UTC(), id.String())
	return err
}

func (r *ReminderRepo) Cancel(ctx context.Context, userID int64, id uuid.UUID) error {
	return r.setTerminalStatus(ctx, userID, id, domain.StatusCancelled)
}

// Finish marks a reminder completed while preserving it for audit/history.
func (r *ReminderRepo) Finish(ctx context.Context, userID int64, id uuid.UUID) error {
	return r.setTerminalStatus(ctx, userID, id, domain.StatusDone)
}

func (r *ReminderRepo) setTerminalStatus(ctx context.Context, userID int64, id uuid.UUID, status domain.Status) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.ExecContext(ctx,
		r.db.Rebind(`UPDATE reminders SET status=$1, idempotency_key=NULL, updated_at=$2, version=version+1 WHERE id=$3 AND user_id=$4`),
		string(status), time.Now().UTC(), id.String(), userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrNotFound
	}
	if err := cancelPendingNotifications(ctx, tx, r.db, id); err != nil {
		return err
	}
	return tx.Commit()
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

// MarkConditionalDue clears stale startup locks for active conditional
// reminders and resets next_eval_at for reminders that need immediate probing.
// Digest reminders and scheduled weather events keep their next_eval_at so
// startup does not send an extra digest or an early forecast/alert.
// Uses the dialect-native NOW() so the stored format matches LeaseDue's comparison.
func (r *ReminderRepo) MarkConditionalDue(ctx context.Context) error {
	now := r.db.Now()
	triggerExpr := `spec->>'trigger'`
	eventTypeExpr := `spec->'event'->>'type'`
	if r.db.Dialect == "sqlite" {
		triggerExpr = `json_extract(spec, '$.trigger')`
		eventTypeExpr = `json_extract(spec, '$.event.type')`
	}
	_, err := r.db.ExecContext(ctx, fmt.Sprintf(
		`UPDATE reminders
		 SET next_eval_at=CASE
		       WHEN COALESCE(%s, '') = 'digest'
		         OR (COALESCE(%s, '') = 'anchor' AND COALESCE(%s, '') = 'weather')
		       THEN next_eval_at
		       ELSE %s
		     END,
		     locked_at=NULL,
		     locked_by=NULL
		 WHERE status='active' AND kind='conditional'`, triggerExpr, triggerExpr, eventTypeExpr, now))
	return err
}

func (r *ReminderRepo) Pause(ctx context.Context, userID int64, id uuid.UUID, pause bool) error {
	status := domain.StatusActive
	if pause {
		status = domain.StatusPaused
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.ExecContext(ctx,
		r.db.Rebind(`UPDATE reminders SET status=$1, updated_at=$2, version=version+1 WHERE id=$3 AND user_id=$4`),
		string(status), time.Now().UTC(), id.String(), userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrNotFound
	}
	if pause {
		if err := cancelPendingNotifications(ctx, tx, r.db, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func cancelPendingNotifications(ctx context.Context, tx *sql.Tx, db *DB, reminderID uuid.UUID) error {
	_, err := tx.ExecContext(ctx, db.Rebind(`
		UPDATE scheduled_notifications
		SET status='cancelled', locked_at=NULL, locked_by=NULL
		WHERE reminder_id=$1 AND status='pending'`), reminderID.String())
	return err
}

// Update atomically replaces a reminder when the caller's version is current.
// Pending notifications are cancelled in the same transaction so the watcher
// can recalculate delivery from the new next evaluation time.
func (r *ReminderRepo) Update(ctx context.Context, rem *domain.Reminder, expectedVersion int64) error {
	specJSON, err := json.Marshal(rem.Spec)
	if err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var evalCron *string
	if rem.EvalCron != "" {
		evalCron = &rem.EvalCron
	}
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, r.db.Rebind(`
		UPDATE reminders
		SET kind=$1, raw_text=$2, spec=$3, status=$4, eval_cron=$5,
		    next_eval_at=$6, locked_at=NULL, locked_by=NULL, updated_at=$7,
		    version=version+1, idempotency_key=NULL
		WHERE id=$8 AND user_id=$9 AND version=$10`),
		string(rem.Kind), rem.RawText, string(specJSON), string(rem.Status),
		NullString(evalCron), NullTime(rem.NextEvalAt), now, rem.ID.String(), rem.UserID, expectedVersion)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		var exists int
		err = tx.QueryRowContext(ctx, r.db.Rebind(`SELECT 1 FROM reminders WHERE id=$1 AND user_id=$2`), rem.ID.String(), rem.UserID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		return domain.ErrConflict
	}
	if err := cancelPendingNotifications(ctx, tx, r.db, rem.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	rem.Version = expectedVersion + 1
	rem.UpdatedAt = now
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
		&rem.CreatedAt, &rem.UpdatedAt, &rem.Version,
	); err != nil {
		return nil, err
	}
	id, err := parseUUID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan reminder: %w", err)
	}
	rem.ID = id
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

// parseUUID parses a UUID column value, returning an error instead of
// uuid.Nil on failure so a corrupted/truncated id surfaces as a query error
// rather than silently producing a row whose ID can never be matched again.
func parseUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("parse uuid %q: %w", s, err)
	}
	return id, nil
}
