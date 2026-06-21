package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nyver2k/remindertgbot/internal/domain"
)

type UserRepo struct{ db *DB }

func NewUserRepo(db *DB) *UserRepo { return &UserRepo{db: db} }

func (r *UserRepo) Upsert(ctx context.Context, userID int64, tz string) (*domain.User, error) {
	var q string
	if r.db.Dialect == "sqlite" {
		q = `INSERT INTO users (id, tz) VALUES ($1, $2)
		     ON CONFLICT (id) DO UPDATE SET tz = excluded.tz
		     RETURNING id, tz, created_at`
	} else {
		q = `INSERT INTO users (id, tz) VALUES ($1, $2)
		     ON CONFLICT (id) DO UPDATE SET tz = EXCLUDED.tz
		     RETURNING id, tz, created_at`
	}
	row := r.db.QueryRowContext(ctx, r.db.Rebind(q), userID, tz)
	return scanUser(row)
}

func (r *UserRepo) Get(ctx context.Context, userID int64) (*domain.User, error) {
	const q = `SELECT id, tz, created_at FROM users WHERE id = $1`
	row := r.db.QueryRowContext(ctx, r.db.Rebind(q), userID)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return u, err
}

func (r *UserRepo) GetOrCreate(ctx context.Context, userID int64) (*domain.User, error) {
	u, err := r.Get(ctx, userID)
	if errors.Is(err, domain.ErrNotFound) {
		return r.Upsert(ctx, userID, "Europe/Moscow")
	}
	return u, err
}

func (r *UserRepo) SetTZ(ctx context.Context, userID int64, tz string) error {
	_, err := r.db.ExecContext(ctx,
		r.db.Rebind(`UPDATE users SET tz = $1 WHERE id = $2`), tz, userID)
	return err
}

func scanUser(row *sql.Row) (*domain.User, error) {
	u := &domain.User{}
	if err := row.Scan(&u.ID, &u.TZ, &u.CreatedAt); err != nil {
		return nil, err
	}
	return u, nil
}
