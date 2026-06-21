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
	var indexRequests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/index":
			indexRequests.Add(1)
			_, _ = io.WriteString(w, `{"info":{},"channel":[{"id":"2","display_name":"Первый канал","week":"2026-06-15","update":"","updte_ut":"","href":""}]}`)
		case "/v1/schedule/2":
			if got := r.URL.Query().Get("week"); got != "20260615" {
				t.Errorf("week = %q", got)
			}
			_, _ = fmt.Fprintf(w, `{"channel":[{"id":"2","display_name":"Первый канал"}],"programms":[`+
				`{"start":"","start_ut":%d,"stop_ut":%d,"event_id":"event-1","title":"КВН. Высшая лига","desc_short":"Финал"},`+
				`{"start":"","start_ut":%d,"event_id":"event-2","title":"Новости"}]}`,
				start.Unix(), start.Add(2*time.Hour).Unix(), start.Add(time.Hour).Unix())
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
	if event.Identity != "event-1" || event.Title != "КВН. Высшая лига" || !event.AnchorAt.Equal(start) {
		t.Fatalf("unexpected event: %+v", event)
	}
	if event.Meta["channel"] != "Первый канал" || event.Meta["channel_id"] != "2" {
		t.Fatalf("unexpected meta: %+v", event.Meta)
	}
	if _, err := p.Lookup(context.Background(), provider.Query{
		Title: "КВН", Params: map[string]string{"channel": "Первый"},
	}, from, to); err != nil {
		t.Fatal(err)
	}
	if got := indexRequests.Load(); got != 1 {
		t.Fatalf("index requests = %d, want 1", got)
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
