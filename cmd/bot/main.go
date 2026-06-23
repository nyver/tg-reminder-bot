package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nyver2k/remindertgbot/internal/config"
	"github.com/nyver2k/remindertgbot/internal/nlu"
	"github.com/nyver2k/remindertgbot/internal/observability"
	"github.com/nyver2k/remindertgbot/internal/provider/iptvx"
	"github.com/nyver2k/remindertgbot/internal/provider/price"
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

	loc, _ := time.LoadLocation("Europe/Moscow")
	fastPath := nlu.NewFastPath(loc)
	model := cfg.NLU.OpenRouter.Model
	baseURL := cfg.NLU.OpenRouter.BaseURL
	if cfg.NLU.Provider == "claude" {
		model = cfg.NLU.Claude.Model
	}
	llmParser, err := nlu.NewConfiguredLLMParser(cfg.NLU.Provider, cfg.NLU.APIKey, model, baseURL, cfg.NLU.OpenRouter.FallbackModels, cfg.NLU.OpenRouter.Timeout, cfg.NLU.OpenRouter.MaxTokens, loc)
	if err != nil {
		log.Error("nlu init", "err", err)
		os.Exit(1)
	}
	parser := nlu.NewChain(0.85, fastPath, llmParser)

	priceProber := price.New(cfg.Providers.Price.UserAgent, cfg.Providers.Price.Timeout, cfg.Providers.Price.Headless, cfg.Providers.Price.ProxyURL, log)
	defer priceProber.Close()
	tvScheduler := iptvx.NewScheduler(postgres.NewEPGRepo(db))

	handler := telegram.NewHandler(
		telegram.NewReminderService(reminderRepo),
		telegram.NewUserService(userRepo),
		dialogRepo,
		parser,
		priceProber,
		observationRepo,
		tvScheduler,
		cfg.Providers.Price.PollCron,
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
