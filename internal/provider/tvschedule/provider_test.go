package tvschedule

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

func TestLookupEPGService(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 6, 15, 10, 0, 0, 0, loc)
	to := from.Add(24 * time.Hour)
	start := time.Date(2026, 6, 15, 21, 0, 0, 0, loc)
	stop := start.Add(2 * time.Hour)
	var indexRequests atomic.Int32

	startStr := start.Format("20060102150405 -0700")
	stopStr := stop.Format("20060102150405 -0700")
	newsStr := start.Add(time.Hour).Format("20060102150405 -0700")

	indexXML := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<tv><channel id="2"><display-name>Первый канал</display-name></channel></tv>`

	scheduleXML := fmt.Sprintf(
		`<?xml version="1.0" encoding="UTF-8"?>`+"\n"+
			`<tv>`+
			`<channel id="2"><display-name>Первый канал</display-name></channel>`+
			`<programme start="%s" stop="%s" channel="2"><title>КВН. Высшая лига</title><desc>Финал</desc></programme>`+
			`<programme start="%s" channel="2"><title>Новости</title></programme>`+
			`</tv>`,
		startStr, stopStr, newsStr,
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/xml" {
			t.Errorf("Accept = %q", got)
		}
		w.Header().Set("Content-Type", "application/xml")
		switch r.URL.Path {
		case "/v1/index":
			indexRequests.Add(1)
			_, _ = io.WriteString(w, indexXML)
		case "/v1/schedule/2":
			if got := r.URL.Query().Get("week"); got != "20260615" {
				t.Errorf("week = %q", got)
			}
			_, _ = io.WriteString(w, scheduleXML)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p := New(Config{BaseURL: server.URL, APIKey: "test-token", Timeout: time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	events, err := p.Lookup(context.Background(), provider.Query{
		Title:  "КВН",
		Params: map[string]string{"channel": "Первый"},
	}, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	event := events[0]
	wantIdentity := fmt.Sprintf("2:%d:%s", start.Unix(), normalize("КВН. Высшая лига"))
	if event.Identity != wantIdentity {
		t.Fatalf("Identity = %q, want %q", event.Identity, wantIdentity)
	}
	if event.Title != "КВН. Высшая лига" {
		t.Fatalf("Title = %q", event.Title)
	}
	if !event.AnchorAt.Equal(start) {
		t.Fatalf("AnchorAt = %v, want %v", event.AnchorAt, start)
	}
	if event.Meta["channel"] != "Первый канал" || event.Meta["channel_id"] != "2" {
		t.Fatalf("unexpected meta: %+v", event.Meta)
	}

	// Second lookup should reuse cached channel index.
	if _, err := p.Lookup(context.Background(), provider.Query{
		Title: "КВН", Params: map[string]string{"channel": "Первый"},
	}, from, to); err != nil {
		t.Fatal(err)
	}
	if got := indexRequests.Load(); got != 1 {
		t.Fatalf("index requests = %d, want 1 (cache not used)", got)
	}
}

// TestLookupSkipsWeekOn404 covers the bug where a 7-day lookahead window spans
// two calendar weeks and the API returns 404 for the future week (data not yet
// available). The provider must skip that week and still return events from the
// current week instead of propagating the 404 as a transient error.
func TestLookupSkipsWeekOn404(t *testing.T) {
	t.Parallel()
	loc, _ := time.LoadLocation("Europe/Moscow")

	// Monday 2026-06-22 at 10:00 MSK. A 7-day window ends on 2026-06-29 10:00 MSK
	// which is past midnight of the next Monday, so weeksInRange returns both
	// "20260622" and "20260629".
	from := time.Date(2026, 6, 22, 10, 0, 0, 0, loc)
	to := from.AddDate(0, 0, 7)
	airTime := time.Date(2026, 6, 22, 18, 0, 0, 0, loc)

	scheduleXML := fmt.Sprintf(
		`<?xml version="1.0" encoding="UTF-8"?>`+"\n"+
			`<tv>`+
			`<channel id="1"><display-name>Test</display-name></channel>`+
			`<programme start="%s" channel="1"><title>Новости</title></programme>`+
			`</tv>`,
		airTime.Format("20060102150405 -0700"),
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/index":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+
				`<tv><channel id="1"><display-name>Test</display-name></channel></tv>`)
		case "/v1/schedule/1":
			week := r.URL.Query().Get("week")
			if week == "20260629" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, scheduleXML)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p := New(Config{BaseURL: server.URL, APIKey: "token", Timeout: time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	events, err := p.Lookup(context.Background(), provider.Query{
		Title:  "Новости",
		Params: map[string]string{"channel": "Test"},
	}, from, to)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Title != "Новости" {
		t.Fatalf("unexpected title %q", events[0].Title)
	}
}

func TestLookupRequiresChannel(t *testing.T) {
	t.Parallel()
	p := New(Config{BaseURL: "https://example.test", APIKey: "token"}, nil)
	_, err := p.Lookup(context.Background(), provider.Query{Title: "КВН"}, time.Now(), time.Now().Add(time.Hour))
	if err == nil {
		t.Fatal("expected channel validation error")
	}
}
