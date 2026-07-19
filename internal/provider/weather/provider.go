package weather

import (
	"context"
	"encoding/json"
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
)

const (
	providerType        = "weather"
	maxResponseBytes    = 2 << 20
	maxCachedLocations  = 256
	defaultForecastDays = 7
)

type Config struct {
	ForecastURL     string
	GeocodingURL    string
	DefaultLocation string
	Timeout         time.Duration
}

// Provider exposes Open-Meteo forecasts as both time-anchored events and a
// scalar temperature metric. Open-Meteo does not require an API key for its
// public non-commercial endpoint.
type Provider struct {
	config Config
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	locations map[string]location
}

type location struct {
	Name      string
	Country   string
	Latitude  float64
	Longitude float64
	Timezone  string
}

type geocodingResponse struct {
	Results []struct {
		Name      string  `json:"name"`
		Country   string  `json:"country"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Timezone  string  `json:"timezone"`
	} `json:"results"`
}

type forecastResponse struct {
	Timezone string `json:"timezone"`
	Daily    struct {
		Time                        []string  `json:"time"`
		WeatherCode                 []int     `json:"weather_code"`
		TemperatureMin              []float64 `json:"temperature_2m_min"`
		TemperatureMax              []float64 `json:"temperature_2m_max"`
		ApparentTemperatureMin      []float64 `json:"apparent_temperature_min"`
		ApparentTemperatureMax      []float64 `json:"apparent_temperature_max"`
		PrecipitationProbabilityMax []float64 `json:"precipitation_probability_max"`
		PrecipitationSum            []float64 `json:"precipitation_sum"`
		RainSum                     []float64 `json:"rain_sum"`
		WindSpeedMax                []float64 `json:"wind_speed_10m_max"`
	} `json:"daily"`
	Hourly struct {
		Time        []string  `json:"time"`
		Temperature []float64 `json:"temperature_2m"`
	} `json:"hourly"`
}

func New(cfg Config) (*Provider, error) {
	if cfg.Timeout <= 0 {
		return nil, fmt.Errorf("weather timeout must be positive")
	}
	if strings.TrimSpace(cfg.DefaultLocation) == "" {
		return nil, fmt.Errorf("weather default location is required")
	}
	for name, raw := range map[string]string{"forecast": cfg.ForecastURL, "geocoding": cfg.GeocodingURL} {
		u, err := url.ParseRequestURI(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("weather %s URL must be an HTTP(S) URL", name)
		}
	}
	return &Provider{
		config:    cfg,
		client:    &http.Client{Timeout: cfg.Timeout},
		now:       time.Now,
		locations: make(map[string]location),
	}, nil
}

func (p *Provider) Type() string { return providerType }

func (p *Provider) Lookup(ctx context.Context, q provider.Query, from, to time.Time) ([]provider.Event, error) {
	loc, err := p.resolveLocation(ctx, q.Params)
	if err != nil {
		return nil, err
	}
	forecast, err := p.fetchForecast(ctx, loc, q.Params)
	if err != nil {
		return nil, err
	}
	target, err := p.targetDate(loc, q.Params)
	if err != nil {
		return nil, err
	}
	i := indexOf(forecast.Daily.Time, target.Format("2006-01-02"))
	if i < 0 {
		return nil, fmt.Errorf("weather forecast for %s is unavailable", target.Format("2006-01-02"))
	}
	if !dailyIndexValid(forecast, i) {
		return nil, fmt.Errorf("weather forecast returned incomplete daily data")
	}
	if q.Params["condition"] == "rain" && forecast.Daily.RainSum[i] <= 0 && !isRainCode(forecast.Daily.WeatherCode[i]) {
		return nil, nil
	}

	anchor := from.Add(time.Second)
	if !to.IsZero() && !anchor.Before(to) {
		anchor = from
	}
	displayName := loc.Name
	if loc.Country != "" {
		displayName += ", " + loc.Country
	}
	meta := map[string]string{
		"location":                   displayName,
		"date":                       target.Format("2006-01-02"),
		"weather":                    weatherDescription(forecast.Daily.WeatherCode[i]),
		"weather_code":               strconv.Itoa(forecast.Daily.WeatherCode[i]),
		"temperature_min_c":          formatDecimal(forecast.Daily.TemperatureMin[i]),
		"temperature_max_c":          formatDecimal(forecast.Daily.TemperatureMax[i]),
		"apparent_temperature_min_c": formatDecimal(forecast.Daily.ApparentTemperatureMin[i]),
		"apparent_temperature_max_c": formatDecimal(forecast.Daily.ApparentTemperatureMax[i]),
		"precipitation_probability":  formatDecimal(forecast.Daily.PrecipitationProbabilityMax[i]),
		"precipitation_mm":           formatDecimal(forecast.Daily.PrecipitationSum[i]),
		"rain_mm":                    formatDecimal(forecast.Daily.RainSum[i]),
		"wind_speed_kmh":             formatDecimal(forecast.Daily.WindSpeedMax[i]),
	}
	return []provider.Event{{
		Identity: fmt.Sprintf("weather:%0.4f:%0.4f:%s:%s", loc.Latitude, loc.Longitude, target.Format("2006-01-02"), q.Params["condition"]),
		Title:    "Прогноз погоды: " + displayName,
		AnchorAt: anchor,
		Meta:     meta,
	}}, nil
}

func (p *Provider) Sample(ctx context.Context, q provider.Query) (provider.Measurement, error) {
	loc, err := p.resolveLocation(ctx, q.Params)
	if err != nil {
		return provider.Measurement{Available: false}, err
	}
	forecast, err := p.fetchForecast(ctx, loc, q.Params)
	if err != nil {
		return provider.Measurement{Available: false}, err
	}
	target, err := p.targetDate(loc, q.Params)
	if err != nil {
		return provider.Measurement{Available: false}, err
	}

	value, err := temperatureForPeriod(forecast, target, q.Params["period"], p.now().In(target.Location()))
	if err != nil {
		return provider.Measurement{Available: false}, err
	}
	displayName := loc.Name
	if loc.Country != "" {
		displayName += ", " + loc.Country
	}
	return provider.Measurement{
		Value:     int64(math.Round(value * 10)),
		Available: true,
		Title:     "Температура: " + displayName,
		Meta: map[string]string{
			"unit":          "°C",
			"temperature_c": formatDecimal(value),
			"date":          target.Format("2006-01-02"),
			"period":        q.Params["period"],
			"location":      displayName,
		},
	}, nil
}

func (p *Provider) resolveLocation(ctx context.Context, params map[string]string) (location, error) {
	if lat, latErr := strconv.ParseFloat(params["latitude"], 64); latErr == nil {
		if lon, lonErr := strconv.ParseFloat(params["longitude"], 64); lonErr == nil {
			if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
				return location{}, fmt.Errorf("weather coordinates are out of range")
			}
			return location{Name: firstNonEmpty(params["location"], p.config.DefaultLocation), Latitude: lat, Longitude: lon, Timezone: params["timezone"]}, nil
		}
	}
	name := strings.TrimSpace(firstNonEmpty(params["location"], p.config.DefaultLocation))
	cacheKey := strings.ToLower(name)
	p.mu.Lock()
	loc, ok := p.locations[cacheKey]
	p.mu.Unlock()
	if ok {
		return loc, nil
	}

	u, _ := url.Parse(p.config.GeocodingURL)
	values := u.Query()
	values.Set("name", name)
	values.Set("count", "1")
	values.Set("language", "ru")
	values.Set("format", "json")
	u.RawQuery = values.Encode()
	var response geocodingResponse
	if err := p.getJSON(ctx, u.String(), &response); err != nil {
		return location{}, fmt.Errorf("weather geocoding %q: %w", name, err)
	}
	if len(response.Results) == 0 {
		return location{}, fmt.Errorf("weather location %q was not found", name)
	}
	result := response.Results[0]
	if result.Latitude < -90 || result.Latitude > 90 || result.Longitude < -180 || result.Longitude > 180 {
		return location{}, fmt.Errorf("weather geocoding returned invalid coordinates")
	}
	loc = location{Name: result.Name, Country: result.Country, Latitude: result.Latitude, Longitude: result.Longitude, Timezone: result.Timezone}
	p.mu.Lock()
	if len(p.locations) >= maxCachedLocations {
		p.locations = make(map[string]location)
	}
	p.locations[cacheKey] = loc
	p.mu.Unlock()
	return loc, nil
}

func (p *Provider) fetchForecast(ctx context.Context, loc location, params map[string]string) (forecastResponse, error) {
	u, _ := url.Parse(p.config.ForecastURL)
	values := u.Query()
	values.Set("latitude", strconv.FormatFloat(loc.Latitude, 'f', 6, 64))
	values.Set("longitude", strconv.FormatFloat(loc.Longitude, 'f', 6, 64))
	values.Set("timezone", firstNonEmpty(params["timezone"], loc.Timezone, "auto"))
	values.Set("forecast_days", strconv.Itoa(p.forecastDays(loc, params)))
	values.Set("daily", strings.Join([]string{
		"weather_code", "temperature_2m_min", "temperature_2m_max",
		"apparent_temperature_min", "apparent_temperature_max",
		"precipitation_probability_max", "precipitation_sum", "rain_sum", "wind_speed_10m_max",
	}, ","))
	if params["period"] == "night" {
		values.Set("hourly", "temperature_2m")
	}
	u.RawQuery = values.Encode()
	var response forecastResponse
	if err := p.getJSON(ctx, u.String(), &response); err != nil {
		return forecastResponse{}, fmt.Errorf("weather forecast: %w", err)
	}
	return response, nil
}

func (p *Provider) getJSON(ctx context.Context, rawURL string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "remindertgbot/1.0")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	limited := io.LimitReader(resp.Body, maxResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if len(data) > maxResponseBytes {
		return fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}

func (p *Provider) targetDate(loc location, params map[string]string) (time.Time, error) {
	timezone := firstNonEmpty(params["timezone"], loc.Timezone, "UTC")
	tz, err := time.LoadLocation(timezone)
	if err != nil {
		tz = time.UTC
	}
	now := p.now().In(tz)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tz)
	switch day := strings.ToLower(strings.TrimSpace(params["day"])); day {
	case "", "today":
		return today, nil
	case "tomorrow":
		return today.AddDate(0, 0, 1), nil
	case "next_night":
		if now.Hour() >= 6 {
			return today.AddDate(0, 0, 1), nil
		}
		return today, nil
	default:
		parsed, parseErr := time.ParseInLocation("2006-01-02", day, tz)
		if parseErr != nil {
			return time.Time{}, fmt.Errorf("invalid weather day %q", day)
		}
		return parsed, nil
	}
}

func (p *Provider) forecastDays(loc location, params map[string]string) int {
	target, err := p.targetDate(loc, params)
	if err != nil {
		return defaultForecastDays
	}
	timezone := firstNonEmpty(params["timezone"], loc.Timezone, "UTC")
	tz, err := time.LoadLocation(timezone)
	if err != nil {
		tz = time.UTC
	}
	now := p.now().In(tz)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tz)
	days := int(target.Sub(today).Hours()/24) + 1
	if days < 1 {
		return 1
	}
	if days > 16 {
		return 16
	}
	return days
}

func temperatureForPeriod(f forecastResponse, target time.Time, period string, now time.Time) (float64, error) {
	date := target.Format("2006-01-02")
	if period != "night" {
		i := indexOf(f.Daily.Time, date)
		if i < 0 || i >= len(f.Daily.TemperatureMin) {
			return 0, fmt.Errorf("temperature forecast for %s is unavailable", date)
		}
		return f.Daily.TemperatureMin[i], nil
	}
	minimum := math.Inf(1)
	for i, rawTime := range f.Hourly.Time {
		if i >= len(f.Hourly.Temperature) || !strings.HasPrefix(rawTime, date+"T") {
			continue
		}
		parsed, err := time.ParseInLocation("2006-01-02T15:04", rawTime, target.Location())
		if err == nil && parsed.Hour() < 6 && !parsed.Before(now) && f.Hourly.Temperature[i] < minimum {
			minimum = f.Hourly.Temperature[i]
		}
	}
	if math.IsInf(minimum, 1) {
		return 0, fmt.Errorf("night temperature forecast for %s is unavailable", date)
	}
	return minimum, nil
}

func dailyIndexValid(f forecastResponse, i int) bool {
	return i < len(f.Daily.WeatherCode) &&
		i < len(f.Daily.TemperatureMin) && i < len(f.Daily.TemperatureMax) &&
		i < len(f.Daily.ApparentTemperatureMin) && i < len(f.Daily.ApparentTemperatureMax) &&
		i < len(f.Daily.PrecipitationProbabilityMax) && i < len(f.Daily.PrecipitationSum) &&
		i < len(f.Daily.RainSum) &&
		i < len(f.Daily.WindSpeedMax)
}

func indexOf(values []string, want string) int {
	for i, value := range values {
		if value == want {
			return i
		}
	}
	return -1
}

func isRainCode(code int) bool {
	switch code {
	case 51, 53, 55, 56, 57, 61, 63, 65, 66, 67, 80, 81, 82, 95, 96, 99:
		return true
	default:
		return false
	}
}

func weatherDescription(code int) string {
	switch code {
	case 0:
		return "ясно"
	case 1, 2:
		return "переменная облачность"
	case 3:
		return "пасмурно"
	case 45, 48:
		return "туман"
	case 51, 53, 55, 56, 57:
		return "морось"
	case 61, 63, 65, 66, 67:
		return "дождь"
	case 71, 73, 75, 77:
		return "снег"
	case 80, 81, 82:
		return "ливень"
	case 85, 86:
		return "снегопад"
	case 95, 96, 99:
		return "гроза"
	default:
		return "погодные условия без уточнения"
	}
}

func formatDecimal(value float64) string {
	return strconv.FormatFloat(value, 'f', 1, 64)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

var (
	_ provider.EventProvider  = (*Provider)(nil)
	_ provider.MetricProvider = (*Provider)(nil)
)
