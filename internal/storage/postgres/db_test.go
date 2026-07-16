package postgres

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestInClauseZeroDoesNotPanic(t *testing.T) {
	pg := &DB{Dialect: "postgres"}
	if got := pg.InClause(2, 0); got != "IN (NULL)" {
		t.Fatalf("postgres InClause(2,0) = %q, want %q", got, "IN (NULL)")
	}
	sqlite := &DB{Dialect: "sqlite"}
	if got := sqlite.InClause(2, 0); got != "IN (NULL)" {
		t.Fatalf("sqlite InClause(2,0) = %q, want %q", got, "IN (NULL)")
	}
}

func TestInClauseNonZero(t *testing.T) {
	pg := &DB{Dialect: "postgres"}
	if got := pg.InClause(2, 3); got != "IN ($2,$3,$4)" {
		t.Fatalf("postgres InClause(2,3) = %q", got)
	}
	sqlite := &DB{Dialect: "sqlite"}
	if got := sqlite.InClause(1, 3); got != "IN (?,?,?)" {
		t.Fatalf("sqlite InClause(1,3) = %q", got)
	}
}

func TestParseUUIDReturnsErrorOnCorruptValue(t *testing.T) {
	if _, err := parseUUID("not-a-uuid"); err == nil {
		t.Fatal("expected an error for a corrupt UUID column value, got nil")
	}
	id, err := parseUUID("123e4567-e89b-12d3-a456-426614174000")
	if err != nil {
		t.Fatalf("unexpected error for a valid UUID: %v", err)
	}
	if id.String() != "123e4567-e89b-12d3-a456-426614174000" {
		t.Fatalf("parseUUID roundtrip = %v", id)
	}
}

func TestNewSQLiteWaitsForStartupLock(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "locked.db")
	locker, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	locker.SetMaxOpenConns(1)

	if _, err := locker.Exec(`CREATE TABLE lock_test (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := locker.Exec(`BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}

	type result struct {
		db  *DB
		err error
	}
	done := make(chan result, 1)
	go func() {
		db, err := New(context.Background(), "sqlite", dsn)
		done <- result{db: db, err: err}
	}()

	select {
	case got := <-done:
		if got.db != nil {
			got.db.Close()
		}
		t.Fatalf("New returned while database was locked: %v", got.err)
	case <-time.After(100 * time.Millisecond):
	}

	if _, err := locker.Exec(`COMMIT`); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("New failed after lock release: %v", got.err)
		}
		got.db.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("New did not finish after lock release")
	}
}
