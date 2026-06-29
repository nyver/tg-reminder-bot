package iptvx

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

// memStore is an in-memory EPGStore for testing.
type memStore struct {
	mu       sync.RWMutex
	channels []EPGChannel
	progs    []EPGProgramme
	imports  int
}

func (m *memStore) ImportEPG(_ context.Context, channels []EPGChannel, progs []EPGProgramme) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.channels = channels
	m.progs = progs
	m.imports++
	return nil
}

func (m *memStore) Channels(_ context.Context) ([]EPGChannel, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.channels, nil
}

func (m *memStore) HasFutureSchedule(_ context.Context) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	now := time.Now()
	for _, p := range m.progs {
		if p.StartsAt.After(now) {
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) SearchProgrammes(_ context.Context, titleLike, channelID string, from, to time.Time) ([]EPGSearchResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	pattern := strings.ToLower(titleLike)
	var out []EPGSearchResult
	for _, p := range m.progs {
		if channelID != "" && p.ChannelID != channelID {
			continue
		}
		if p.StartsAt.Before(from) || !p.StartsAt.Before(to) {
			continue
		}
		if pattern != "" && !strings.Contains(strings.ToLower(p.Title), pattern) {
			continue
		}
		chName := p.ChannelID
		for _, ch := range m.channels {
			if ch.ID == p.ChannelID {
				chName = ch.DisplayName
				break
			}
		}
		out = append(out, EPGSearchResult{
			ChannelID:   p.ChannelID,
			ChannelName: chName,
			Title:       p.Title,
			StartsAt:    p.StartsAt,
			EndsAt:      p.EndsAt,
		})
	}
	return out, nil
}

func (m *memStore) Programmes(_ context.Context, channelID string, from, to time.Time) ([]EPGProgramme, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []EPGProgramme
	for _, p := range m.progs {
		if p.ChannelID != channelID {
			continue
		}
		if p.StartsAt.Before(from) || !p.StartsAt.Before(to) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// --- test helpers ---

func makeXMLTV(start, stop time.Time) string {
	startStr := start.Format("20060102150405 -0700")
	stopStr := stop.Format("20060102150405 -0700")
	return fmt.Sprintf(
		`<?xml version="1.0" encoding="UTF-8"?>`+"\n"+
			`<tv>`+
			`<channel id="1"><display-name lang="ru">Первый канал</display-name></channel>`+
			`<channel id="2"><display-name lang="ru">Россия 1</display-name></channel>`+
			`<programme start="%s" stop="%s" channel="1"><title lang="ru">КВН. Высшая лига</title><desc lang="ru">Финал сезона</desc></programme>`+
			`<programme start="%s" channel="1"><title lang="ru">Новости</title></programme>`+
			`</tv>`,
		startStr, stopStr, stop.Format("20060102150405 -0700"),
	)
}

func gzipBytes(data []byte) []byte {
	pr, pw := io.Pipe()
	go func() {
		gz := gzip.NewWriter(pw)
		_, _ = gz.Write(data)
		_ = gz.Close()
		_ = pw.Close()
	}()
	b, _ := io.ReadAll(pr)
	return b
}

func newTestProvider(t *testing.T, url, filePath string, store EPGStore) *Provider {
	t.Helper()
	return New(Config{
		URL:            url,
		FilePath:       filePath,
		UpdateInterval: 24 * time.Hour,
		Timeout:        5 * time.Second,
	}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// --- tests ---

func TestLookup_plainXML(t *testing.T) {
	t.Parallel()
	loc, _ := time.LoadLocation("Europe/Moscow")
	start := time.Date(2026, 6, 22, 21, 0, 0, 0, loc)
	stop := start.Add(2 * time.Hour)
	xmlData := []byte(makeXMLTV(start, stop))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write(xmlData)
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := &memStore{}
	p := newTestProvider(t, srv.URL, filepath.Join(dir, "epg.xml"), store)

	if err := p.ensureImported(context.Background()); err != nil {
		t.Fatalf("ensureImported: %v", err)
	}
	if store.imports != 1 {
		t.Fatalf("imports = %d, want 1", store.imports)
	}

	from := time.Date(2026, 6, 22, 0, 0, 0, 0, loc)
	to := from.Add(24 * time.Hour)

	events, err := p.Lookup(context.Background(), provider.Query{
		Title:  "КВН",
		Params: map[string]string{"channel": "Первый"},
	}, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Title != "КВН. Высшая лига" {
		t.Errorf("Title = %q", ev.Title)
	}
	if !ev.AnchorAt.Equal(start) {
		t.Errorf("AnchorAt = %v, want %v", ev.AnchorAt, start)
	}
	if ev.Meta["channel"] != "Первый канал" {
		t.Errorf("channel meta = %q", ev.Meta["channel"])
	}
	if ev.Meta["channel_id"] != "1" {
		t.Errorf("channel_id meta = %q", ev.Meta["channel_id"])
	}
	if ev.Meta["description"] != "Финал сезона" {
		t.Errorf("description meta = %q", ev.Meta["description"])
	}
}

func TestLookup_gzippedFile(t *testing.T) {
	t.Parallel()
	loc, _ := time.LoadLocation("Europe/Moscow")
	start := time.Date(2026, 6, 22, 20, 0, 0, 0, loc)
	stop := start.Add(time.Hour)
	compressed := gzipBytes([]byte(makeXMLTV(start, stop)))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(compressed)
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := &memStore{}
	p := newTestProvider(t, srv.URL, filepath.Join(dir, "epg.xml.gz"), store)

	if err := p.ensureImported(context.Background()); err != nil {
		t.Fatalf("ensureImported: %v", err)
	}

	from := time.Date(2026, 6, 22, 0, 0, 0, 0, loc)
	to := from.Add(24 * time.Hour)

	events, err := p.Lookup(context.Background(), provider.Query{
		Title:  "КВН",
		Params: map[string]string{"channel": "Первый"},
	}, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
}

func TestLookup_byChannelID(t *testing.T) {
	t.Parallel()
	loc, _ := time.LoadLocation("Europe/Moscow")
	start := time.Date(2026, 6, 22, 19, 0, 0, 0, loc)
	stop := start.Add(time.Hour)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "epg.xml")
	if err := os.WriteFile(filePath, []byte(makeXMLTV(start, stop)), 0o644); err != nil {
		t.Fatal(err)
	}

	store := &memStore{}
	p := newTestProvider(t, "http://unused", filePath, store)
	if err := p.importFile(context.Background()); err != nil {
		t.Fatal(err)
	}

	from := time.Date(2026, 6, 22, 0, 0, 0, 0, loc)
	to := from.Add(24 * time.Hour)

	events, err := p.Lookup(context.Background(), provider.Query{
		Title:  "КВН",
		Params: map[string]string{"channel_id": "1"},
	}, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
}

func TestLookup_emptyCacheReturnsEmpty(t *testing.T) {
	t.Parallel()
	store := &memStore{}
	p := New(Config{URL: "https://example.test", FilePath: "/nonexistent"}, store, nil)
	events, err := p.Lookup(context.Background(), provider.Query{
		Title:  "КВН",
		Params: map[string]string{"channel": "Первый"},
	}, time.Now(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("want 0 events, got %d", len(events))
	}
}

func TestImport_replacesPreviousData(t *testing.T) {
	t.Parallel()
	loc, _ := time.LoadLocation("Europe/Moscow")
	start := time.Date(2026, 6, 22, 20, 0, 0, 0, loc)
	stop := start.Add(time.Hour)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "epg.xml")
	if err := os.WriteFile(filePath, []byte(makeXMLTV(start, stop)), 0o644); err != nil {
		t.Fatal(err)
	}

	store := &memStore{}
	p := newTestProvider(t, "http://unused", filePath, store)

	// Import twice — store must reflect second import.
	for i := 0; i < 2; i++ {
		if err := p.importFile(context.Background()); err != nil {
			t.Fatalf("import %d: %v", i, err)
		}
	}
	if store.imports != 2 {
		t.Fatalf("imports = %d, want 2", store.imports)
	}
}

func TestLookup_staleFileUsedOnDownloadError(t *testing.T) {
	t.Parallel()
	loc, _ := time.LoadLocation("Europe/Moscow")
	start := time.Date(2026, 6, 15, 21, 0, 0, 0, loc)
	stop := start.Add(time.Hour)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "epg.xml")
	if err := os.WriteFile(filePath, []byte(makeXMLTV(start, stop)), 0o644); err != nil {
		t.Fatal(err)
	}
	// Mark file as old so ensureImported tries to re-download.
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(filePath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	defer srv.Close()

	store := &memStore{}
	p := newTestProvider(t, srv.URL, filePath, store)

	if err := p.ensureImported(context.Background()); err != nil {
		t.Fatalf("ensureImported should fall back to stale cache: %v", err)
	}
	if store.imports != 1 {
		t.Fatalf("want 1 import from stale cache, got %d", store.imports)
	}
}

func TestType(t *testing.T) {
	p := New(Config{}, &memStore{}, nil)
	if got := p.Type(); got != "tv_program" {
		t.Fatalf("Type() = %q, want tv_program", got)
	}
}

// TestEnsureImported_skipsWhenFreshFileAndWarmDB verifies that ensureImported
// does not re-download or re-import when the cached file is younger than
// UpdateInterval AND the DB already contains future programmes.
func TestEnsureImported_skipsWhenFreshFileAndWarmDB(t *testing.T) {
	t.Parallel()

	loc, _ := time.LoadLocation("Europe/Moscow")
	futureStart := time.Now().Add(2 * time.Hour).In(loc)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "epg.xml")
	// Write a fresh file (mod-time = now → younger than 24 h UpdateInterval).
	if err := os.WriteFile(filePath, []byte(makeXMLTV(futureStart, futureStart.Add(time.Hour))), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pre-populate store so HasFutureSchedule returns true.
	store := &memStore{
		channels: []EPGChannel{{ID: "1", DisplayName: "Первый канал"}},
		progs:    []EPGProgramme{{ChannelID: "1", Title: "Тест", StartsAt: futureStart}},
	}
	store.imports = 1

	p := newTestProvider(t, "http://should-not-be-called", filePath, store)
	if err := p.ensureImported(context.Background()); err != nil {
		t.Fatalf("ensureImported: %v", err)
	}
	if store.imports != 1 {
		t.Fatalf("imports = %d, want 1 (expected skip)", store.imports)
	}
}

// TestEnsureImported_reimportsWhenFileIsStale verifies that ensureImported
// re-downloads and re-imports even when the DB has future programmes, as long as
// the cached file is older than UpdateInterval. This is the fix for the bug where
// update_interval was effectively ignored once data was seeded into the DB.
func TestEnsureImported_reimportsWhenFileIsStale(t *testing.T) {
	t.Parallel()

	loc, _ := time.LoadLocation("Europe/Moscow")
	futureStart := time.Now().Add(2 * time.Hour).In(loc)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "epg.xml")
	// Write a stale file — mod-time is 30 days in the past.
	if err := os.WriteFile(filePath, []byte(makeXMLTV(futureStart, futureStart.Add(time.Hour))), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(filePath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// Serve fresh data from the mock HTTP server.
	freshStart := futureStart.Add(48 * time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(makeXMLTV(freshStart, freshStart.Add(time.Hour))))
	}))
	defer srv.Close()

	// Pre-populate store so HasFutureSchedule returns true (simulates "warm DB").
	store := &memStore{
		channels: []EPGChannel{{ID: "1", DisplayName: "Первый канал"}},
		progs:    []EPGProgramme{{ChannelID: "1", Title: "Старая передача", StartsAt: futureStart}},
	}
	store.imports = 1

	p := newTestProvider(t, srv.URL, filePath, store)
	if err := p.ensureImported(context.Background()); err != nil {
		t.Fatalf("ensureImported: %v", err)
	}
	// Must have incremented: stale file forced a re-download + re-import.
	if store.imports != 2 {
		t.Fatalf("imports = %d, want 2 (expected re-import due to stale file)", store.imports)
	}
}
