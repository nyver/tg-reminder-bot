package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/nyver2k/remindertgbot/internal/domain"
)

// UserPreferencesRepo persists Telegram UI preferences for both supported SQL dialects.
type UserPreferencesRepo struct {
	db       *DB
	defaults domain.UserPreferences
}

func NewUserPreferencesRepo(db *DB, values ...domain.UserPreferences) *UserPreferencesRepo {
	defaults := domain.UserPreferences{MorningTime: "09:00", DefaultSnoozeMinutes: 10}
	if len(values) > 0 {
		defaults = values[0]
	}
	return &UserPreferencesRepo{db: db, defaults: defaults}
}

func (r *UserPreferencesRepo) GetOrCreate(ctx context.Context, userID int64) (*domain.UserPreferences, error) {
	var quietStart, quietEnd *string
	if r.defaults.QuietStart != "" {
		quietStart = &r.defaults.QuietStart
	}
	if r.defaults.QuietEnd != "" {
		quietEnd = &r.defaults.QuietEnd
	}
	_, err := r.db.ExecContext(ctx, r.db.Rebind(`
		INSERT INTO user_ui_preferences
			(user_id, quiet_start, quiet_end, morning_time, default_snooze_minutes)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (user_id) DO NOTHING`), userID, NullString(quietStart), NullString(quietEnd),
		r.defaults.MorningTime, r.defaults.DefaultSnoozeMinutes)
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, userID)
}

func (r *UserPreferencesRepo) Get(ctx context.Context, userID int64) (*domain.UserPreferences, error) {
	row := r.db.QueryRowContext(ctx, r.db.Rebind(`
		SELECT user_id, quiet_start, quiet_end, morning_time, default_snooze_minutes, updated_at
		FROM user_ui_preferences WHERE user_id=$1`), userID)
	var p domain.UserPreferences
	var quietStart, quietEnd sql.NullString
	if err := row.Scan(&p.UserID, &quietStart, &quietEnd, &p.MorningTime, &p.DefaultSnoozeMinutes, &p.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	p.QuietStart = quietStart.String
	p.QuietEnd = quietEnd.String
	return &p, nil
}

func (r *UserPreferencesRepo) Update(ctx context.Context, p domain.UserPreferences) error {
	var quietStart, quietEnd *string
	if p.QuietStart != "" {
		quietStart = &p.QuietStart
	}
	if p.QuietEnd != "" {
		quietEnd = &p.QuietEnd
	}
	res, err := r.db.ExecContext(ctx, r.db.Rebind(`
		UPDATE user_ui_preferences
		SET quiet_start=$1, quiet_end=$2, morning_time=$3,
		    default_snooze_minutes=$4, updated_at=$5
		WHERE user_id=$6`), NullString(quietStart), NullString(quietEnd),
		p.MorningTime, p.DefaultSnoozeMinutes, time.Now().UTC(), p.UserID)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return domain.ErrNotFound
	}
	return nil
}
