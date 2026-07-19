package weather

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

func TestLookupReturnsDailyForecastAndCachesLocation(t *testing.T) {
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	var geocodingCalls atomic.Int32
	server := newWeatherServer(t, &geocodingCalls, http.StatusOK, 61)
	p := newTestProvider(t, server.URL, now)
	query := provider.Query{Params: map[string]string{"location": "Казань", "timezone": "UTC", "day": "tomorrow"}}

	for range 2 {
		events, err := p.Lookup(context.Background(), query, now, now.Add(48*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 {
			t.Fatalf("events = %+v", events)
		}
		event := events[0]
		if event.Meta["weather"] != "дождь" || event.Meta["temperature_min_c"] != "8.2" || event.Meta["location"] != "Казань, Россия" {
			t.Fatalf("unexpected event metadata: %+v", event.Meta)
		}
		if !event.AnchorAt.Equal(now.Add(time.Second)) {
			t.Fatalf("anchor = %v", event.AnchorAt)
		}
	}
	if got := geocodingCalls.Load(); got != 1 {
		t.Fatalf("geocoding calls = %d, want 1", got)
	}
}

func TestLookupSuppressesRainAlertWhenForecastIsDry(t *testing.T) {
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	server := newWeatherServer(t, nil, http.StatusOK, 2)
	p := newTestProvider(t, server.URL, now)
	events, err := p.Lookup(context.Background(), provider.Query{Params: map[string]string{
		"condition": "rain", "day": "today", "timezone": "UTC",
	}}, now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("dry forecast produced events: %+v", events)
	}
}

func TestSampleUsesNightMinimumInTenthsOfDegree(t *testing.T) {
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	server := newWeatherServer(t, nil, http.StatusOK, 0)
	p := newTestProvider(t, server.URL, now)
	measurement, err := p.Sample(context.Background(), provider.Query{Params: map[string]string{
		"day": "next_night", "period": "night", "timezone": "UTC",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !measurement.Available || measurement.Value != -124 {
		t.Fatalf("measurement = %+v", measurement)
	}
	if measurement.Meta["temperature_c"] != "-12.4" || measurement.Meta["date"] != "2026-07-20" {
		t.Fatalf("metadata = %+v", measurement.Meta)
	}
}

func TestProviderReportsUpstreamAndValidationErrors(t *testing.T) {
	if _, err := New(Config{Timeout: time.Second, DefaultLocation: "Moscow", ForecastURL: "file:///tmp/a", GeocodingURL: "https://example.com"}); err == nil {
		t.Fatal("expected invalid URL error")
	}
	server := newWeatherServer(t, nil, http.StatusTooManyRequests, 0)
	p := newTestProvider(t, server.URL, time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC))
	_, err := p.Sample(context.Background(), provider.Query{Params: map[string]string{"timezone": "UTC"}})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveLocationRetriesHyphenatedNameWithSpaces(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/geocode" {
			http.NotFound(w, r)
			return
		}
		queries = append(queries, r.URL.Query().Get("name"))
		w.Header().Set("Content-Type", "application/json")
		if len(queries) == 1 {
			fmt.Fprint(w, `{"generationtime_ms":0.1}`)
			return
		}
		fmt.Fprint(w, `{"results":[{"name":"Saint Petersburg","country":"Russia","latitude":59.93863,"longitude":30.31413,"timezone":"Europe/Moscow"}]}`)
	}))
	t.Cleanup(server.Close)

	p := newTestProvider(t, server.URL, time.Now())
	params := map[string]string{"location": "Saint-Petersburg"}
	for range 2 {
		loc, err := p.resolveLocation(context.Background(), params)
		if err != nil {
			t.Fatal(err)
		}
		if loc.Name != "Saint Petersburg" || loc.Timezone != "Europe/Moscow" {
			t.Fatalf("location = %+v", loc)
		}
	}
	if got, want := strings.Join(queries, "|"), "Saint-Petersburg|Saint Petersburg"; got != want {
		t.Fatalf("geocoding queries = %q, want %q", got, want)
	}
}

func TestLocationNameWithoutHyphens(t *testing.T) {
	tests := map[string]string{
		"Saint-Petersburg":        "Saint Petersburg",
		"Rostov\u2011on\u2011Don": "Rostov on Don",
		"New  York":               "",
	}
	for input, want := range tests {
		if got := locationNameWithoutHyphens(input); got != want {
			t.Errorf("locationNameWithoutHyphens(%q) = %q, want %q", input, got, want)
		}
	}
}

func newTestProvider(t *testing.T, baseURL string, now time.Time) *Provider {
	t.Helper()
	p, err := New(Config{
		ForecastURL:     baseURL + "/forecast",
		GeocodingURL:    baseURL + "/geocode",
		DefaultLocation: "Moscow",
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return now }
	return p
}

func newWeatherServer(t *testing.T, geocodingCalls *atomic.Int32, forecastStatus, weatherCode int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/geocode":
			if geocodingCalls != nil {
				geocodingCalls.Add(1)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"results":[{"name":"Казань","country":"Россия","latitude":55.79,"longitude":49.12,"timezone":"UTC"}]}`)
		case "/forecast":
			if forecastStatus != http.StatusOK {
				w.WriteHeader(forecastStatus)
				fmt.Fprint(w, `{"error":true}`)
				return
			}
			if got := r.URL.Query().Get("daily"); !strings.Contains(got, "weather_code") {
				t.Errorf("daily query = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
  "timezone":"UTC",
  "daily":{
    "time":["2026-07-19","2026-07-20"],
    "weather_code":[%d,%d],
    "temperature_2m_min":[10.0,8.2],
    "temperature_2m_max":[20.1,17.8],
    "apparent_temperature_min":[9.1,7.4],
    "apparent_temperature_max":[19.0,16.9],
    "precipitation_probability_max":[20,80],
    "precipitation_sum":[0,4.5],
    "rain_sum":[%s,%s],
    "wind_speed_10m_max":[12,18]
  },
  "hourly":{
    "time":["2026-07-20T00:00","2026-07-20T01:00","2026-07-20T05:00","2026-07-20T06:00"],
    "temperature_2m":[-10.1,-12.4,-11.8,-9.0]
  }
}`, weatherCode, weatherCode, rainAmount(weatherCode), rainAmount(weatherCode))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func rainAmount(code int) string {
	if isRainCode(code) {
		return "0.5"
	}
	return "0"
}
