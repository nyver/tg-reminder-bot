package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
	_ "modernc.org/sqlite"             // sqlite driver
)

// DB wraps database/sql.DB with dialect-aware helpers.
type DB struct {
	*sql.DB
	Dialect string // "postgres" | "sqlite"
}

// New opens a connection pool for the given driver and DSN.
// driver must be "postgres" or "sqlite".
func New(ctx context.Context, driver, dsn string) (*DB, error) {
	sqlDriver := driverName(driver)
	sqldb, err := sql.Open(sqlDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := sqldb.PingContext(ctx); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	if driver == "sqlite" {
		// SQLite PRAGMAs are connection-local. A single connection also prevents
		// database/sql from creating competing writers inside one process.
		sqldb.SetMaxOpenConns(1)
		// WAL mode improves concurrent reads; busy_timeout reduces write contention.
		if _, err := sqldb.ExecContext(ctx,
			`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON`); err != nil {
			_ = sqldb.Close()
			return nil, fmt.Errorf("sqlite pragmas: %w", err)
		}
	}
	return &DB{DB: sqldb, Dialect: driver}, nil
}

func driverName(driver string) string {
	switch driver {
	case "postgres":
		return "pgx"
	case "sqlite":
		return "sqlite"
	default:
		return driver
	}
}

// Rebind converts PostgreSQL $1,$2,... placeholders to SQLite ? style.
func (db *DB) Rebind(query string) string {
	if db.Dialect != "sqlite" {
		return query
	}
	return reBindRegex.ReplaceAllString(query, "?")
}

var reBindRegex = regexp.MustCompile(`\$\d+`)

// Now returns the SQL expression for the current timestamp.
func (db *DB) Now() string {
	if db.Dialect == "sqlite" {
		return "datetime('now')"
	}
	return "now()"
}

// MinutesAgo returns a SQL expression for N minutes in the past.
func (db *DB) MinutesAgo(n int) string {
	if db.Dialect == "sqlite" {
		return fmt.Sprintf("datetime('now', '-%d minutes')", n)
	}
	return fmt.Sprintf("now() - interval '%d minutes'", n)
}

// ForUpdateSkipLocked returns the FOR UPDATE SKIP LOCKED clause (empty for SQLite).
func (db *DB) ForUpdateSkipLocked() string {
	if db.Dialect == "sqlite" {
		return "" // SQLite uses WAL + locked_at timeout instead
	}
	return "FOR UPDATE SKIP LOCKED"
}

// InClause builds a parameterised IN clause starting at parameter index start.
// Returns the SQL fragment (e.g. "IN ($2,$3,$4)") and the args to append.
func (db *DB) InClause(start int, n int) string {
	if db.Dialect == "sqlite" {
		return "IN (" + strings.Repeat("?,", n)[:2*n-1] + ")"
	}
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = fmt.Sprintf("$%d", start+i)
	}
	return "IN (" + strings.Join(parts, ",") + ")"
}

// ExecUpdateLocked sets locked_at/locked_by on a batch of UUIDs.
func (db *DB) ExecUpdateLocked(ctx context.Context, table, workerID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	now := db.Now()
	in := db.InClause(2, len(ids))
	args := make([]any, 0, 1+len(ids))
	args = append(args, workerID)
	if db.Dialect == "sqlite" {
		for _, id := range ids {
			args = append(args, id)
		}
	} else {
		for _, id := range ids {
			args = append(args, id)
		}
	}
	q := fmt.Sprintf(`UPDATE %s SET locked_at=%s, locked_by=$1 WHERE id %s`, table, now, in)
	q = db.Rebind(q)
	_, err := db.ExecContext(ctx, q, args...)
	return err
}

// NullString converts *string to sql.NullString.
func NullString(s *string) sql.NullString {
	if s == nil || *s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

// NullTime converts *time.Time to sql.NullTime.
func NullTime(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

// PtrString converts sql.NullString to *string.
func PtrString(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	return &ns.String
}

// PtrTime converts sql.NullTime to *time.Time.
func PtrTime(nt sql.NullTime) *time.Time {
	if !nt.Valid {
		return nil
	}
	return &nt.Time
}
