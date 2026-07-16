package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type CacheRepo struct{ db *DB }

func NewCacheRepo(db *DB) *CacheRepo { return &CacheRepo{db: db} }

func (r *CacheRepo) Get(ctx context.Context, key string) (json.RawMessage, bool, error) {
	now := r.db.Now()
	q := r.db.Rebind(`
		SELECT value FROM provider_cache
		WHERE key=$1 AND ` + ttlExpiry(r.db.Dialect) + ` > ` + now)
	row := r.db.QueryRowContext(ctx, q, key)
	var val string
	if err := row.Scan(&val); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("cache get: %w", err)
	}
	return json.RawMessage(val), true, nil
}

func (r *CacheRepo) GetStale(ctx context.Context, key string) (json.RawMessage, bool, error) {
	row := r.db.QueryRowContext(ctx, r.db.Rebind(`SELECT value FROM provider_cache WHERE key=$1`), key)
	var val string
	if err := row.Scan(&val); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("cache get stale: %w", err)
	}
	return json.RawMessage(val), true, nil
}

func (r *CacheRepo) Set(ctx context.Context, key string, value json.RawMessage, ttl time.Duration) error {
	now := r.db.Now()
	_, err := r.db.ExecContext(ctx, r.db.Rebind(`
		INSERT INTO provider_cache (key, value, fetched_at, ttl_seconds)
		VALUES ($1,$2,`+now+`,$3)
		ON CONFLICT (key) DO UPDATE
		SET value=excluded.value, fetched_at=`+now+`, ttl_seconds=excluded.ttl_seconds`),
		key, string(value), int(ttl.Seconds()))
	return err
}

// ttlExpiry returns a SQL expression for cache expiry, dialect-aware.
func ttlExpiry(dialect string) string {
	if dialect == "sqlite" {
		return "datetime(fetched_at, '+' || ttl_seconds || ' seconds')"
	}
	return "fetched_at + (ttl_seconds || ' seconds')::interval"
}
