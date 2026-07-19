package config

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Database  DatabaseConfig  `yaml:"database"`
	Telegram  TelegramConfig  `yaml:"telegram"`
	NLU       NLUConfig       `yaml:"nlu"`
	Providers ProvidersConfig `yaml:"providers"`
	Scheduler SchedulerConfig `yaml:"scheduler"`
	Server    ServerConfig    `yaml:"server"`
}

type DatabaseConfig struct {
	Driver string `yaml:"driver"` // "sqlite" | "postgres"
	DSN    string `yaml:"dsn"`
}

type TelegramConfig struct {
	Token string `yaml:"token"`
}

type NLUConfig struct {
	Provider   string           `yaml:"provider"` // "claude" | "openrouter"
	APIKey     string           `yaml:"api_key"`
	Claude     ClaudeConfig     `yaml:"claude"`
	OpenRouter OpenRouterConfig `yaml:"openrouter"`
}

type ClaudeConfig struct {
	Model string `yaml:"model"`
}

type OpenRouterConfig struct {
	BaseURL        string        `yaml:"base_url"`
	Model          string        `yaml:"model"`
	FallbackModels []string      `yaml:"fallback_models"`
	Timeout        time.Duration `yaml:"timeout"`
	MaxTokens      int           `yaml:"max_tokens"`
}

type ProvidersConfig struct {
	TV      TVConfig      `yaml:"tv"`
	IPTVX   IPTVXConfig   `yaml:"iptvx"`
	Price   PriceConfig   `yaml:"price"`
	Travel  TravelConfig  `yaml:"travel"`
	RSS     RSSConfig     `yaml:"rss"`
	Weather WeatherConfig `yaml:"weather"`
}

type TVConfig struct {
	BaseURL string        `yaml:"base_url"`
	APIKey  string        `yaml:"api_key"`
	Timeout time.Duration `yaml:"timeout"`
}

type IPTVXConfig struct {
	URL            string        `yaml:"url"`
	FilePath       string        `yaml:"file_path"`
	UpdateInterval time.Duration `yaml:"update_interval"`
	Timeout        time.Duration `yaml:"timeout"`
}

type PriceConfig struct {
	UserAgent string        `yaml:"user_agent"`
	Timeout   time.Duration `yaml:"timeout"`
	Headless  bool          `yaml:"headless"`
	ProxyURL  string        `yaml:"proxy_url"`
	// PollCron is the default cron schedule for price-drop reminders when the
	// user does not specify an explicit interval. Standard 5-field cron syntax.
	PollCron string `yaml:"poll_cron"`
}

type TravelConfig struct {
	AirAPIKey      string        `yaml:"air_api_key"`
	RailAPIKey     string        `yaml:"rail_api_key"`
	Timeout        time.Duration `yaml:"timeout"`
	MaxHorizonDays int           `yaml:"max_horizon_days"`
}

type RSSConfig struct {
	// Timeout is the deadline for fetching and reading one RSS/Atom feed.
	Timeout time.Duration `yaml:"timeout"`
	// LLMDigest enables optional LLM-based re-ranking and re-summarization of
	// digest items (using the nlu provider/model configuration), replacing
	// the keyword+recency heuristic. Off by default: it adds one LLM call
	// per digest tick.
	LLMDigest bool `yaml:"llm_digest"`
	// ProxyURL routes RSS/Atom fetches through an HTTP(S) or SOCKS5 proxy.
	// Some feeds block requests from datacenter/VPS IP ranges outright; a
	// proxy is the only way to reach those. Empty means fetch directly.
	ProxyURL string `yaml:"proxy_url"`
}

type WeatherConfig struct {
	// ForecastURL and GeocodingURL are configurable to support self-hosted
	// Open-Meteo installations. Both must use HTTP(S).
	ForecastURL  string `yaml:"forecast_url"`
	GeocodingURL string `yaml:"geocoding_url"`
	// DefaultLocation is used when a reminder does not name a city.
	DefaultLocation string        `yaml:"default_location"`
	Timeout         time.Duration `yaml:"timeout"`
	// PollCron is the default schedule for forecast-based temperature alerts.
	PollCron string `yaml:"poll_cron"`
}

type SchedulerConfig struct {
	WatcherTick      time.Duration `yaml:"watcher_tick"`
	DeliveryTick     time.Duration `yaml:"delivery_tick"`
	HousekeepingTick time.Duration `yaml:"housekeeping_tick"`
}

type ServerConfig struct {
	WorkerID string `yaml:"worker_id"`
	LogLevel string `yaml:"log_level"`
}

// Load reads config.yaml (or the file at CONFIG_FILE env var) and applies
// environment variable overrides for secrets.
func Load() (*Config, error) {
	path := configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	// Expand ${VAR} and $VAR in the raw YAML before parsing.
	data = []byte(os.ExpandEnv(string(data)))

	cfg := defaults()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyEnvOverrides(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadOrDefaults returns defaults if config file is absent (useful for remindctl).
func LoadOrDefaults() (*Config, error) {
	cfg, err := Load()
	if os.IsNotExist(err) {
		cfg = defaults()
		applyEnvOverrides(cfg)
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
		return cfg, nil
	}
	return cfg, err
}

func configPath() string {
	if p := os.Getenv("CONFIG_FILE"); p != "" {
		return p
	}
	return "config.yaml"
}

func defaults() *Config {
	return &Config{
		Database: DatabaseConfig{
			Driver: "sqlite",
			DSN:    "./data/remind.db",
		},
		NLU: NLUConfig{
			Provider: "openrouter",
			Claude: ClaudeConfig{
				Model: "claude-haiku-4-5-20251001",
			},
			OpenRouter: OpenRouterConfig{
				BaseURL:   "https://openrouter.ai/api/v1",
				Model:     "anthropic/claude-haiku-4.5",
				Timeout:   30 * time.Second,
				MaxTokens: 1024,
				FallbackModels: []string{
					"mistralai/mistral-7b-instruct:free",
					"meta-llama/llama-3.2-3b-instruct:free",
				},
			},
		},
		Providers: ProvidersConfig{
			TV: TVConfig{
				BaseURL: "https://api.epgservice.ru",
				Timeout: 15 * time.Second,
			},
			IPTVX: IPTVXConfig{
				URL:            "https://iptvx.one/epg/epg.xml.gz",
				FilePath:       "./data/iptvx_epg.xml.gz",
				UpdateInterval: 7 * 24 * time.Hour,
				Timeout:        120 * time.Second,
			},
			Price: PriceConfig{
				UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
				Timeout:   15 * time.Second,
				PollCron:  "0 * * * *", // every hour
			},
			Travel: TravelConfig{
				Timeout:        10 * time.Second,
				MaxHorizonDays: 180,
			},
			RSS: RSSConfig{
				Timeout: 15 * time.Second,
			},
			Weather: WeatherConfig{
				ForecastURL:     "https://api.open-meteo.com/v1/forecast",
				GeocodingURL:    "https://geocoding-api.open-meteo.com/v1/search",
				DefaultLocation: "Moscow",
				Timeout:         10 * time.Second,
				PollCron:        "0 * * * *",
			},
		},
		Scheduler: SchedulerConfig{
			WatcherTick:      time.Minute,
			DeliveryTick:     15 * time.Second,
			HousekeepingTick: time.Hour,
		},
		Server: ServerConfig{
			LogLevel: "info",
		},
	}
}

// applyEnvOverrides lets operators override sensitive values via environment
// variables without touching the config file.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("TELEGRAM_TOKEN"); v != "" {
		cfg.Telegram.Token = v
	}
	if v := os.Getenv("LLM_API_KEY"); v != "" {
		cfg.NLU.APIKey = v
	}
	if v := os.Getenv("EPG_SERVICE_API_KEY"); v != "" {
		cfg.Providers.TV.APIKey = v
	}
	if v := os.Getenv("EPG_SERVICE_BASE_URL"); v != "" {
		cfg.Providers.TV.BaseURL = v
	}
	if v := os.Getenv("IPTVX_EPG_URL"); v != "" {
		cfg.Providers.IPTVX.URL = v
	}
	if v := os.Getenv("IPTVX_EPG_FILE"); v != "" {
		cfg.Providers.IPTVX.FilePath = v
	}
	if v := os.Getenv("DATABASE_DSN"); v != "" {
		cfg.Database.DSN = v
	}
	if v := os.Getenv("DATABASE_DRIVER"); v != "" {
		cfg.Database.Driver = v
	}
	// DATABASE_URL is the conventional PostgreSQL override and has the
	// highest precedence over the generic database variables.
	if v := os.Getenv("DATABASE_URL"); v != "" {
		cfg.Database.DSN = v
		cfg.Database.Driver = "postgres"
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.Server.LogLevel = v
	}
}

// Validate rejects configuration errors early, before a service starts.
func (cfg *Config) Validate() error {
	cfg.Database.Driver = strings.ToLower(strings.TrimSpace(cfg.Database.Driver))
	if cfg.Database.Driver != "sqlite" && cfg.Database.Driver != "postgres" {
		return fmt.Errorf("config: database.driver must be sqlite or postgres")
	}
	if strings.TrimSpace(cfg.Database.DSN) == "" {
		return fmt.Errorf("config: database.dsn is required")
	}
	cfg.NLU.Provider = strings.ToLower(strings.TrimSpace(cfg.NLU.Provider))
	if cfg.NLU.Provider != "claude" && cfg.NLU.Provider != "openrouter" {
		return fmt.Errorf("config: nlu.provider must be claude or openrouter")
	}
	cfg.Server.LogLevel = strings.ToLower(strings.TrimSpace(cfg.Server.LogLevel))
	switch cfg.Server.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: server.log_level must be one of: debug, info, warn, error")
	}
	if cfg.Scheduler.WatcherTick <= 0 {
		return fmt.Errorf("config: scheduler.watcher_tick must be positive")
	}
	if cfg.Scheduler.DeliveryTick <= 0 {
		return fmt.Errorf("config: scheduler.delivery_tick must be positive")
	}
	if cfg.Scheduler.HousekeepingTick <= 0 {
		return fmt.Errorf("config: scheduler.housekeeping_tick must be positive")
	}
	if cfg.NLU.OpenRouter.Timeout <= 0 {
		return fmt.Errorf("config: nlu.openrouter.timeout must be positive")
	}
	if cfg.NLU.OpenRouter.MaxTokens <= 0 {
		return fmt.Errorf("config: nlu.openrouter.max_tokens must be positive")
	}
	if cfg.Providers.TV.Timeout <= 0 {
		return fmt.Errorf("config: providers.tv.timeout must be positive")
	}
	if cfg.Providers.IPTVX.Timeout <= 0 {
		return fmt.Errorf("config: providers.iptvx.timeout must be positive")
	}
	if cfg.Providers.IPTVX.UpdateInterval <= 0 {
		return fmt.Errorf("config: providers.iptvx.update_interval must be positive")
	}
	if cfg.Providers.Price.Timeout <= 0 {
		return fmt.Errorf("config: providers.price.timeout must be positive")
	}
	if err := validateProxyURL(cfg.Providers.Price.ProxyURL); err != nil {
		return fmt.Errorf("config: providers.price.proxy_url: %w", err)
	}
	if strings.TrimSpace(cfg.Providers.Price.PollCron) == "" {
		return fmt.Errorf("config: providers.price.poll_cron is required")
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	if _, err := parser.Parse(cfg.Providers.Price.PollCron); err != nil {
		return fmt.Errorf("config: providers.price.poll_cron is invalid: %w", err)
	}
	if cfg.Providers.Travel.Timeout <= 0 {
		return fmt.Errorf("config: providers.travel.timeout must be positive")
	}
	if cfg.Providers.Travel.MaxHorizonDays <= 0 {
		return fmt.Errorf("config: providers.travel.max_horizon_days must be positive")
	}
	if cfg.Providers.RSS.Timeout <= 0 {
		return fmt.Errorf("config: providers.rss.timeout must be positive")
	}
	if err := validateProxyURL(cfg.Providers.RSS.ProxyURL); err != nil {
		return fmt.Errorf("config: providers.rss.proxy_url: %w", err)
	}
	if cfg.Providers.Weather.Timeout <= 0 {
		return fmt.Errorf("config: providers.weather.timeout must be positive")
	}
	if strings.TrimSpace(cfg.Providers.Weather.DefaultLocation) == "" {
		return fmt.Errorf("config: providers.weather.default_location is required")
	}
	for name, rawURL := range map[string]string{
		"forecast_url":  cfg.Providers.Weather.ForecastURL,
		"geocoding_url": cfg.Providers.Weather.GeocodingURL,
	} {
		parsed, err := url.ParseRequestURI(rawURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("config: providers.weather.%s must be an HTTP(S) URL", name)
		}
	}
	if strings.TrimSpace(cfg.Providers.Weather.PollCron) == "" {
		return fmt.Errorf("config: providers.weather.poll_cron is required")
	}
	if _, err := parser.Parse(cfg.Providers.Weather.PollCron); err != nil {
		return fmt.Errorf("config: providers.weather.poll_cron: %w", err)
	}
	return nil
}

func validateProxyURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("host is required")
	}
	return nil
}
