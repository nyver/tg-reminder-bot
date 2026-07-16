package postgres

import (
	"context"
	"path/filepath"
	"testing"
)

func newCacheTestDB(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()
	db, err := New(ctx, "sqlite", filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	schema := `CREATE TABLE provider_cache (
		key TEXT PRIMARY KEY,
		value TEXT,
		fetched_at TEXT NOT NULL DEFAULT (datetime('now')),
		ttl_seconds INTEGER NOT NULL DEFAULT 0
	)`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestCacheRepoGetMissReturnsNoError(t *testing.T) {
	db := newCacheTestDB(t)
	repo := NewCacheRepo(db)

	val, ok, err := repo.Get(context.Background(), "missing")
	if err != nil {
		t.Fatalf("expected nil error on genuine cache miss, got: %v", err)
	}
	if ok || val != nil {
		t.Fatalf("expected (nil, false), got (%v, %v)", val, ok)
	}
}

// TestCacheRepoGetStalePropagatesRealScanError guards against a regression
// where every Scan error — not just sql.ErrNoRows — was swallowed and
// reported identically to a cache miss, hiding real DB/decode problems from
// callers who would otherwise re-hit a possibly degraded upstream provider.
func TestCacheRepoGetStalePropagatesRealScanError(t *testing.T) {
	db := newCacheTestDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO provider_cache (key, value) VALUES ('k1', NULL)`); err != nil {
		t.Fatal(err)
	}
	repo := NewCacheRepo(db)

	val, ok, err := repo.GetStale(ctx, "k1")
	if err == nil {
		t.Fatalf("expected a scan error for a NULL value column, got (%v, %v, nil)", val, ok)
	}
	if ok || val != nil {
		t.Fatalf("expected (nil, false) alongside the error, got (%v, %v)", val, ok)
	}
}
