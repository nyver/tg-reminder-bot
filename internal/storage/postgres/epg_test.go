package postgres

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nyver2k/remindertgbot/internal/provider/iptvx"
)

func newEPGTestRepo(t *testing.T) *EPGRepo {
	t.Helper()
	ctx := context.Background()
	db, err := New(ctx, "sqlite", filepath.Join(t.TempDir(), "epg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	for _, stmt := range []string{
		`CREATE TABLE epg_channels (id TEXT PRIMARY KEY, display_name TEXT NOT NULL)`,
		`CREATE TABLE epg_programmes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			channel_id TEXT NOT NULL,
			title TEXT NOT NULL,
			starts_at DATETIME NOT NULL,
			ends_at DATETIME,
			description TEXT
		)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	return NewEPGRepo(db)
}

// fillEPG streams nCh channels and nProg programmes (round-robin over channels,
// one per minute starting at base) into w.
func fillEPG(nCh, nProg int, base time.Time) func(iptvx.EPGWriter) error {
	return func(w iptvx.EPGWriter) error {
		for i := 0; i < nCh; i++ {
			if err := w.AddChannel(iptvx.EPGChannel{ID: fmt.Sprint(i), DisplayName: fmt.Sprintf("Channel %d", i)}); err != nil {
				return err
			}
		}
		for i := 0; i < nProg; i++ {
			p := iptvx.EPGProgramme{
				ChannelID: fmt.Sprint(i % nCh),
				Title:     fmt.Sprintf("Show %d", i),
				StartsAt:  base.Add(time.Duration(i) * time.Minute),
			}
			if i%2 == 0 {
				p.EndsAt = p.StartsAt.Add(time.Minute)
				p.Desc = "desc"
			}
			if err := w.AddProgramme(p); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestEPGRepoImportStreamsAcrossBatches(t *testing.T) {
	repo := newEPGTestRepo(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(time.Hour).Truncate(time.Second)

	// More rows than epgBatchSize in both tables to exercise intermediate
	// flushes and the final partial batch.
	nCh, nProg := epgBatchSize+7, 3*epgBatchSize+11
	if err := repo.ImportEPG(ctx, fillEPG(nCh, nProg, base)); err != nil {
		t.Fatal(err)
	}

	channels, err := repo.Channels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != nCh {
		t.Fatalf("channels = %d, want %d", len(channels), nCh)
	}

	progs, err := repo.Programmes(ctx, "0", base, base.Add(time.Duration(nProg)*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if want := (nProg + nCh - 1) / nCh; len(progs) != want {
		t.Fatalf("channel 0 programmes = %d, want %d", len(progs), want)
	}
	if progs[0].Desc != "desc" || progs[0].EndsAt.IsZero() {
		t.Fatalf("first programme lost optional fields: %+v", progs[0])
	}

	ok, err := repo.HasFutureSchedule(ctx)
	if err != nil || !ok {
		t.Fatalf("HasFutureSchedule = %v, %v", ok, err)
	}

	res, err := repo.SearchProgrammes(ctx, "show 1", "", base, base.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 || res[0].ChannelName == "" {
		t.Fatalf("search results = %+v", res)
	}
}

func TestEPGRepoImportReplacesAndRollsBackOnError(t *testing.T) {
	repo := newEPGTestRepo(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(time.Hour).Truncate(time.Second)

	if err := repo.ImportEPG(ctx, fillEPG(3, 10, base)); err != nil {
		t.Fatal(err)
	}

	// A fill that fails after several batches were flushed must leave the
	// previous import intact.
	boom := errors.New("boom")
	err := repo.ImportEPG(ctx, func(w iptvx.EPGWriter) error {
		if err := fillEPG(5, 2*epgBatchSize+1, base)(w); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	channels, err := repo.Channels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 3 {
		t.Fatalf("channels after rollback = %d, want 3", len(channels))
	}

	// A successful re-import fully replaces previous rows.
	if err := repo.ImportEPG(ctx, fillEPG(1, 2, base)); err != nil {
		t.Fatal(err)
	}
	progs, err := repo.Programmes(ctx, "0", base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(progs) != 2 {
		t.Fatalf("programmes after replace = %d, want 2", len(progs))
	}
}
