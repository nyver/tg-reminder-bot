package exchangerate

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nyver2k/remindertgbot/internal/provider"
	"golang.org/x/text/encoding/charmap"
)

const (
	providerType     = "exchange_rate"
	maxResponseBytes = 2 << 20

	// RateScale stores quote-currency units with six decimal places.
	RateScale int64 = 1_000_000
	// PercentScale stores percentage values with two decimal places.
	PercentScale int64 = 100

	fiatCacheTTL   = time.Hour
	cryptoCacheTTL = 20 * time.Second
)

type Config struct {
	CBRURL          string
	CoinGeckoURL    string
	CoinGeckoAPIKey string
	Timeout         time.Duration
}

// Provider exposes fiat and cryptocurrency rates as scalar metrics. Fiat
// cross-rates are derived from the Bank of Russia's RUB reference rates;
// cryptocurrency prices and rolling 24-hour changes come from CoinGecko.
type Provider struct {
	config Config
	client *http.Client

	mu    sync.Mutex
	cache map[string]cachedMeasurement
}

type cachedMeasurement struct {
	measurement provider.Measurement
	expiresAt   time.Time
}

type cbrResponse struct {
	Date    string        `xml:"Date,attr"`
	Valutes []cbrCurrency `xml:"Valute"`
}

type cbrCurrency struct {
	CharCode string `xml:"CharCode"`
	Nominal  int64  `xml:"Nominal"`
	Name     string `xml:"Name"`
	Value    string `xml:"Value"`
}

func New(cfg Config) (*Provider, error) {
	if cfg.Timeout <= 0 {
		return nil, fmt.Errorf("exchange rate timeout must be positive")
	}
	for name, raw := range map[string]string{"CBR": cfg.CBRURL, "CoinGecko": cfg.CoinGeckoURL} {
		u, err := url.ParseRequestURI(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("exchange rate %s URL must be an HTTP(S) URL", name)
		}
	}
	return &Provider{config: cfg, client: &http.Client{Timeout: cfg.Timeout}, cache: make(map[string]cachedMeasurement)}, nil
}

func (p *Provider) Type() string { return providerType }

func (p *Provider) Sample(ctx context.Context, q provider.Query) (provider.Measurement, error) {
	params := q.Params
	assetType := strings.ToLower(strings.TrimSpace(params["asset_type"]))
	metric := strings.ToLower(strings.TrimSpace(params["metric"]))
	if metric == "" {
		metric = "rate"
	}
	cacheKey := assetType + "|" + strings.ToLower(params["base"]) + "|" + strings.ToLower(params["quote"]) + "|" + metric
	if cached, ok := p.cached(cacheKey, q.Title, params); ok {
		return cached, nil
	}

	var (
		measurement provider.Measurement
		err         error
		ttl         time.Duration
	)
	switch assetType {
	case "fiat":
		if metric != "rate" {
			return provider.Measurement{Available: false}, fmt.Errorf("fiat exchange rates support only the rate metric")
		}
		measurement, err = p.sampleFiat(ctx, q.Title, params)
		ttl = fiatCacheTTL
	case "crypto":
		if metric != "rate" && metric != "change_24h" {
			return provider.Measurement{Available: false}, fmt.Errorf("unsupported cryptocurrency metric %q", metric)
		}
		measurement, err = p.sampleCrypto(ctx, q.Title, params, metric)
		ttl = cryptoCacheTTL
	default:
		return provider.Measurement{Available: false}, fmt.Errorf("unsupported exchange rate asset_type %q", assetType)
	}
	if err == nil && measurement.Available {
		p.storeCached(cacheKey, measurement, ttl)
	}
	return measurement, err
}

func (p *Provider) cached(key, title string, params map[string]string) (provider.Measurement, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.cache[key]
	if !ok || !time.Now().Before(entry.expiresAt) {
		if ok {
			delete(p.cache, key)
		}
		return provider.Measurement{}, false
	}
	measurement := entry.measurement
	measurement.Title = titleOrPair(title, strings.ToUpper(params["base"]), strings.ToUpper(params["quote"]))
	measurement.Meta = cloneMap(measurement.Meta)
	return measurement, true
}

func (p *Provider) storeCached(key string, measurement provider.Measurement, ttl time.Duration) {
	measurement.Title = ""
	measurement.Meta = cloneMap(measurement.Meta)
	p.mu.Lock()
	p.cache[key] = cachedMeasurement{measurement: measurement, expiresAt: time.Now().Add(ttl)}
	p.mu.Unlock()
}

func cloneMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (p *Provider) sampleFiat(ctx context.Context, title string, params map[string]string) (provider.Measurement, error) {
	base := strings.ToUpper(strings.TrimSpace(params["base"]))
	quote := strings.ToUpper(strings.TrimSpace(params["quote"]))
	if !isCurrencyCode(base) || !isCurrencyCode(quote) || base == quote {
		return provider.Measurement{Available: false}, fmt.Errorf("fiat exchange rate requires distinct three-letter base and quote codes")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.config.CBRURL, nil)
	if err != nil {
		return provider.Measurement{Available: false}, fmt.Errorf("build CBR request: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return provider.Measurement{Available: false}, fmt.Errorf("fetch CBR exchange rates: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return provider.Measurement{Available: false, HTTPStatus: resp.StatusCode}, fmt.Errorf("CBR exchange rates returned HTTP %d", resp.StatusCode)
	}

	var data cbrResponse
	decoder := xml.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes))
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		if strings.EqualFold(charset, "windows-1251") {
			return charmap.Windows1251.NewDecoder().Reader(input), nil
		}
		return nil, fmt.Errorf("unsupported CBR response charset %q", charset)
	}
	if err := decoder.Decode(&data); err != nil {
		return provider.Measurement{Available: false}, fmt.Errorf("decode CBR exchange rates: %w", err)
	}

	rates := map[string]float64{"RUB": 1}
	for _, currency := range data.Valutes {
		if currency.Nominal <= 0 {
			continue
		}
		value, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(currency.Value), ",", "."), 64)
		if err == nil && value > 0 && !math.IsInf(value, 0) && !math.IsNaN(value) {
			rates[strings.ToUpper(currency.CharCode)] = value / float64(currency.Nominal)
		}
	}
	baseRUB, baseOK := rates[base]
	quoteRUB, quoteOK := rates[quote]
	if !baseOK || !quoteOK {
		return provider.Measurement{Available: false}, fmt.Errorf("CBR exchange rate pair %s/%s is unavailable", base, quote)
	}
	rate := baseRUB / quoteRUB
	value, err := scaledValue(rate, RateScale)
	if err != nil {
		return provider.Measurement{Available: false}, err
	}
	return provider.Measurement{
		Value: value, Currency: quote, Available: true,
		Title: titleOrPair(title, base, quote),
		Meta:  map[string]string{"asset_type": "fiat", "metric": "rate", "base": base, "quote": quote, "source_date": data.Date},
	}, nil
}

func (p *Provider) sampleCrypto(ctx context.Context, title string, params map[string]string, metric string) (provider.Measurement, error) {
	coinID := strings.ToLower(strings.TrimSpace(params["base"]))
	quote := strings.ToLower(strings.TrimSpace(params["quote"]))
	if !isCoinID(coinID) || !isCurrencyCode(strings.ToUpper(quote)) {
		return provider.Measurement{Available: false}, fmt.Errorf("cryptocurrency exchange rate requires a CoinGecko coin ID and a three-letter quote code")
	}

	u, _ := url.Parse(p.config.CoinGeckoURL)
	query := u.Query()
	query.Set("ids", coinID)
	query.Set("vs_currencies", quote)
	query.Set("include_last_updated_at", "true")
	if metric == "change_24h" {
		query.Set("include_24hr_change", "true")
	}
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return provider.Measurement{Available: false}, fmt.Errorf("build CoinGecko request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if p.config.CoinGeckoAPIKey != "" {
		req.Header.Set("x-cg-demo-api-key", p.config.CoinGeckoAPIKey)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return provider.Measurement{Available: false}, fmt.Errorf("fetch CoinGecko price: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return provider.Measurement{Available: false, HTTPStatus: resp.StatusCode}, fmt.Errorf("CoinGecko price returned HTTP %d", resp.StatusCode)
	}
	var data map[string]map[string]*float64
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&data); err != nil {
		return provider.Measurement{Available: false}, fmt.Errorf("decode CoinGecko price: %w", err)
	}
	coin, ok := data[coinID]
	if !ok {
		return provider.Measurement{Available: false}, fmt.Errorf("CoinGecko coin %q was not found", coinID)
	}
	key := quote
	scale := RateScale
	currency := strings.ToUpper(quote)
	if metric == "change_24h" {
		key = quote + "_24h_change"
		scale = PercentScale
		currency = "%"
	}
	raw, ok := coin[key]
	if !ok || raw == nil {
		return provider.Measurement{Available: false}, fmt.Errorf("CoinGecko metric %q is unavailable for %s", metric, coinID)
	}
	if metric == "rate" && *raw <= 0 {
		return provider.Measurement{Available: false}, fmt.Errorf("CoinGecko returned a non-positive price for %s", coinID)
	}
	value, err := scaledValue(*raw, scale)
	if err != nil {
		return provider.Measurement{Available: false}, err
	}
	meta := map[string]string{"asset_type": "crypto", "metric": metric, "base": coinID, "quote": strings.ToUpper(quote)}
	if updated := coin["last_updated_at"]; updated != nil {
		meta["last_updated_at"] = strconv.FormatInt(int64(*updated), 10)
	}
	return provider.Measurement{
		Value: value, Currency: currency, Available: true,
		Title: titleOrPair(title, strings.ToUpper(coinID), strings.ToUpper(quote)), Meta: meta,
	}, nil
}

func scaledValue(value float64, scale int64) (int64, error) {
	scaled := value * float64(scale)
	if math.IsNaN(scaled) || math.IsInf(scaled, 0) || scaled > math.MaxInt64 || scaled < math.MinInt64 {
		return 0, fmt.Errorf("exchange rate value is out of range")
	}
	return int64(math.Round(scaled)), nil
}

func isCurrencyCode(value string) bool {
	if len(value) != 3 {
		return false
	}
	for _, r := range value {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func isCoinID(value string) bool {
	if value == "" || len(value) > 100 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func titleOrPair(title, base, quote string) string {
	if strings.TrimSpace(title) != "" {
		return strings.TrimSpace(title)
	}
	return base + "/" + quote
}

var _ provider.MetricProvider = (*Provider)(nil)
