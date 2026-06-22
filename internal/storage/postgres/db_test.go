package postgres

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

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
