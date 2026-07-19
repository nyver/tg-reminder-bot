package exchangerate

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

func TestSampleFiatRateAndCrossRate(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><ValCurs Date="19.07.2026">
			<Valute><CharCode>EUR</CharCode><Nominal>1</Nominal><Name>Euro</Name><Value>101,2500</Value></Valute>
			<Valute><CharCode>CNY</CharCode><Nominal>10</Nominal><Name>Yuan</Name><Value>110,0000</Value></Valute>
		</ValCurs>`)
	}))
	defer server.Close()
	p := newTestProvider(t, server.URL, server.URL)

	euro, err := p.Sample(context.Background(), provider.Query{Params: map[string]string{
		"asset_type": "fiat", "base": "EUR", "quote": "RUB",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !euro.Available || euro.Value != 101_250_000 || euro.Currency != "RUB" || euro.Meta["source_date"] != "19.07.2026" {
		t.Fatalf("EUR/RUB measurement = %+v", euro)
	}
	cached, err := p.Sample(context.Background(), provider.Query{Title: "Euro alert", Params: map[string]string{
		"asset_type": "fiat", "base": "eur", "quote": "rub",
	}})
	if err != nil || cached.Title != "Euro alert" || calls.Load() != 1 {
		t.Fatalf("cached EUR/RUB measurement = %+v, err = %v, calls = %d", cached, err, calls.Load())
	}

	cross, err := p.Sample(context.Background(), provider.Query{Params: map[string]string{
		"asset_type": "fiat", "base": "EUR", "quote": "CNY",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cross.Value != 9_204_545 { // 101.25 RUB / 11 RUB, rounded to six decimals.
		t.Fatalf("EUR/CNY value = %d", cross.Value)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2 distinct pairs", calls.Load())
	}
}

func TestSampleCryptoRateAndDailyChange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-cg-demo-api-key"); got != "demo-secret" {
			t.Errorf("API key header = %q", got)
		}
		if r.URL.Query().Get("ids") != "bitcoin" || r.URL.Query().Get("vs_currencies") != "rub" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"bitcoin":{"rub":5123456.789,"rub_24h_change":-5.126,"last_updated_at":1784476800}}`)
	}))
	defer server.Close()
	p := newTestProvider(t, server.URL, server.URL)
	p.config.CoinGeckoAPIKey = "demo-secret"

	rate, err := p.Sample(context.Background(), provider.Query{Title: "Bitcoin/RUB", Params: map[string]string{
		"asset_type": "crypto", "base": "bitcoin", "quote": "RUB", "metric": "rate",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rate.Value != 5_123_456_789_000 || rate.Currency != "RUB" || rate.Title != "Bitcoin/RUB" {
		t.Fatalf("rate measurement = %+v", rate)
	}

	change, err := p.Sample(context.Background(), provider.Query{Params: map[string]string{
		"asset_type": "crypto", "base": "bitcoin", "quote": "rub", "metric": "change_24h",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if change.Value != -513 || change.Currency != "%" || change.Meta["metric"] != "change_24h" {
		t.Fatalf("change measurement = %+v", change)
	}
}

func TestProviderRejectsInvalidConfigParamsAndUpstreamResponses(t *testing.T) {
	if _, err := New(Config{Timeout: time.Second, CBRURL: "file:///tmp/rates", CoinGeckoURL: "https://example.com"}); err == nil {
		t.Fatal("expected invalid URL error")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	p := newTestProvider(t, server.URL, server.URL)

	_, err := p.Sample(context.Background(), provider.Query{Params: map[string]string{
		"asset_type": "crypto", "base": "bitcoin", "quote": "RUB",
	}})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("upstream error = %v", err)
	}
	_, err = p.Sample(context.Background(), provider.Query{Params: map[string]string{
		"asset_type": "fiat", "base": "EUR", "quote": "EUR",
	}})
	if err == nil {
		t.Fatal("expected invalid fiat pair error")
	}
}

func newTestProvider(t *testing.T, cbrURL, coinGeckoURL string) *Provider {
	t.Helper()
	p, err := New(Config{CBRURL: cbrURL, CoinGeckoURL: coinGeckoURL, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
