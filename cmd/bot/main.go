package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/nyver2k/remindertgbot/internal/clock"
	"github.com/nyver2k/remindertgbot/internal/config"
	"github.com/nyver2k/remindertgbot/internal/nlu"
	"github.com/nyver2k/remindertgbot/internal/observability"
	"github.com/nyver2k/remindertgbot/internal/provider"
	"github.com/nyver2k/remindertgbot/internal/provider/iptvx"
	"github.com/nyver2k/remindertgbot/internal/provider/price"
	"github.com/nyver2k/remindertgbot/internal/provider/rss"
	"github.com/nyver2k/remindertgbot/internal/provider/travel"
	"github.com/nyver2k/remindertgbot/internal/provider/tvschedule"
	"github.com/nyver2k/remindertgbot/internal/provider/weather"
	"github.com/nyver2k/remindertgbot/internal/scheduler"
	"github.com/nyver2k/remindertgbot/internal/storage/postgres"
	"github.com/nyver2k/remindertgbot/internal/telegram"
)

var version = "dev"

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load", "err", err)
		os.Exit(1)
	}

	log := observability.NewLogger(cfg.Server.LogLevel)
	log.Info("starting bot", "version", version)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	db, err := postgres.New(ctx, cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		log.Error("db connect", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	userRepo := postgres.NewUserRepo(db)
	reminderRepo := postgres.NewReminderRepo(db)
	observationRepo := postgres.NewObservationRepo(db)
	dialogRepo := postgres.NewDialogRepo(db)

	fastPath := nlu.NewFastPath()
	model := cfg.NLU.OpenRouter.Model
	baseURL := cfg.NLU.OpenRouter.BaseURL
	if cfg.NLU.Provider == "claude" {
		model = cfg.NLU.Claude.Model
	}
	llmParser, err := nlu.NewConfiguredLLMParser(cfg.NLU.Provider, cfg.NLU.APIKey, model, baseURL, cfg.NLU.OpenRouter.FallbackModels, cfg.NLU.OpenRouter.Timeout, cfg.NLU.OpenRouter.MaxTokens, log)
	if err != nil {
		log.Error("nlu init", "err", err)
		os.Exit(1)
	}
	parser := nlu.NewChain(0.85, fastPath, llmParser)

	priceProber := price.New(cfg.Providers.Price.UserAgent, cfg.Providers.Price.Timeout, cfg.Providers.Price.Headless, cfg.Providers.Price.ProxyURL, log)
	defer priceProber.Close()
	tvScheduler := iptvx.NewScheduler(postgres.NewEPGRepo(db))

	// Read-only provider registry for /run: lets the bot evaluate a reminder
	// on demand via the same scheduler.Evaluator the worker uses, without
	// running any background jobs (e.g. iptvx.Provider.Lookup queries the DB
	// the worker already keeps fresh; its Run() import loop is not started here).
	registry := provider.NewRegistry()
	if cfg.Providers.IPTVX.URL != "" {
		registry.RegisterEvent(iptvx.New(iptvx.Config{
			URL:            cfg.Providers.IPTVX.URL,
			FilePath:       cfg.Providers.IPTVX.FilePath,
			UpdateInterval: cfg.Providers.IPTVX.UpdateInterval,
			Timeout:        cfg.Providers.IPTVX.Timeout,
		}, postgres.NewEPGRepo(db), log))
	} else {
		registry.RegisterEvent(tvschedule.New(tvschedule.Config{
			BaseURL: cfg.Providers.TV.BaseURL,
			APIKey:  cfg.Providers.TV.APIKey,
			Timeout: cfg.Providers.TV.Timeout,
		}, log))
	}
	registry.RegisterMetric(priceProber)
	airP := travel.NewAirProvider(cfg.Providers.Travel.AirAPIKey, log)
	railP := travel.NewRailProvider(cfg.Providers.Travel.RailAPIKey, log)
	registry.RegisterSearch(travel.NewAggregator(log, airP, railP))
	rssProvider, err := rss.New(cfg.Providers.RSS.Timeout, cfg.Providers.RSS.ProxyURL, log)
	if err != nil {
		log.Error("rss provider init", "err", err)
		os.Exit(1)
	}
	registry.RegisterNews(rssProvider)
	weatherProvider, err := weather.New(weather.Config{
		ForecastURL:     cfg.Providers.Weather.ForecastURL,
		GeocodingURL:    cfg.Providers.Weather.GeocodingURL,
		DefaultLocation: cfg.Providers.Weather.DefaultLocation,
		Timeout:         cfg.Providers.Weather.Timeout,
	})
	if err != nil {
		log.Error("weather provider init", "err", err)
		os.Exit(1)
	}
	registry.RegisterEvent(weatherProvider)
	registry.RegisterMetric(weatherProvider)

	evaluator := scheduler.NewEvaluator(registry, observationRepo, clock.Real(), cfg.Providers.Travel.MaxHorizonDays, log)
	if cfg.Providers.RSS.LLMDigest {
		ranker, err := nlu.NewConfiguredNewsRanker(cfg.NLU.Provider, cfg.NLU.APIKey, model, baseURL, cfg.NLU.OpenRouter.FallbackModels, cfg.NLU.OpenRouter.Timeout, cfg.NLU.OpenRouter.MaxTokens, log)
		if err != nil {
			log.Error("rss llm_digest init", "err", err)
			os.Exit(1)
		}
		evaluator.SetNewsRanker(ranker)
	}

	handler := telegram.NewHandler(
		telegram.NewReminderService(reminderRepo),
		telegram.NewUserService(userRepo),
		dialogRepo,
		parser,
		priceProber,
		observationRepo,
		tvScheduler,
		evaluator,
		cfg.Providers.Price.PollCron,
		cfg.Providers.Weather.PollCron,
		cfg.Providers.Weather.DefaultLocation,
		log,
	)

	bot, err := telegram.NewBot(cfg.Telegram.Token, handler)
	if err != nil {
		log.Error("bot init", "err", err)
		os.Exit(1)
	}

	go func() {
		<-ctx.Done()
		log.Info("bot stopping")
		bot.Stop()
	}()

	log.Info("bot started, polling Telegram")
	bot.Start()
}

// dialogs adapter: DialogRepo implements telegram.DialogStore directly via pointer receiver.
var _ telegram.DialogStore = (*postgres.DialogRepo)(nil)
