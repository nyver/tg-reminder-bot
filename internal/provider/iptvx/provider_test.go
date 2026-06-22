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
