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

type ObservationRepo struct{ db *DB }

func NewObservationRepo(db *DB) *ObservationRepo { return &ObservationRepo{db: db} }

func (r *ObservationRepo) Save(ctx context.Context, obs *domain.Observation) error {
	if obs.ID == uuid.Nil {
		obs.ID = uuid.New()
	}
	if obs.ObservedAt.IsZero() {
		obs.ObservedAt = time.Now().UTC()
	}
	raw := obs.Raw
	if raw == nil {
		raw = json.RawMessage("null")
	}
	_, err := r.db.ExecContext(ctx, r.db.Rebind(`
		INSERT INTO observations (id, reminder_id, value, currency, available, raw, observed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`),
		obs.ID.String(), obs.ReminderID.String(),
		obs.Value, obs.Currency, obs.Available, string(raw), obs.ObservedAt)
	return err
}

func (r *ObservationRepo) Last(ctx context.Context, reminderID uuid.UUID) (*domain.Observation, error) {
	row := r.db.QueryRowContext(ctx, r.db.Rebind(`
		SELECT id, reminder_id, value, currency, available, raw, observed_at
		FROM observations
		WHERE reminder_id=$1
		ORDER BY observed_at DESC
		LIMIT 1`), reminderID.String())

	obs, err := scanObservation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return obs, err
}

func (r *ObservationRepo) List(ctx context.Context, reminderID uuid.UUID, limit int) ([]domain.Observation, error) {
	rows, err := r.db.QueryContext(ctx, r.db.Rebind(`
		SELECT id, reminder_id, value, currency, available, raw, observed_at
		FROM observations WHERE reminder_id=$1
		ORDER BY observed_at DESC LIMIT $2`), reminderID.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.Observation
	for rows.Next() {
		obs, err := scanObservation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *obs)
	}
	return result, rows.Err()
}

func scanObservation(row rowScanner) (*domain.Observation, error) {
	obs := &domain.Observation{}
	var idStr, remIDStr string
	var rawStr string
	if err := row.Scan(
		&idStr, &remIDStr, &obs.Value, &obs.Currency,
		&obs.Available, &rawStr, &obs.ObservedAt,
	); err != nil {
		return nil, err
	}
	obs.ID = mustParseUUID(idStr)
	obs.ReminderID = mustParseUUID(remIDStr)
	obs.Raw = json.RawMessage(rawStr)
	return obs, nil
}
