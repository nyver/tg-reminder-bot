package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/nyver2k/remindertgbot/internal/clock"
	"github.com/nyver2k/remindertgbot/internal/config"
	"github.com/nyver2k/remindertgbot/internal/delivery"
	"github.com/nyver2k/remindertgbot/internal/observability"
	"github.com/nyver2k/remindertgbot/internal/provider"
	"github.com/nyver2k/remindertgbot/internal/provider/price"
	"github.com/nyver2k/remindertgbot/internal/provider/travel"
	"github.com/nyver2k/remindertgbot/internal/provider/tvschedule"
	"github.com/nyver2k/remindertgbot/internal/scheduler"
	"github.com/nyver2k/remindertgbot/internal/storage/postgres"
	"github.com/nyver2k/remindertgbot/internal/telegram"
	"golang.org/x/sync/errgroup"
	tele "gopkg.in/telebot.v3"
)

var version = "dev"

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load", "err", err)
		os.Exit(1)
	}

	log := observability.NewLogger(cfg.Server.LogLevel)
	log.Info("starting worker", "version", version)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	db, err := postgres.New(ctx, cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		log.Error("db connect", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	reminderRepo := postgres.NewReminderRepo(db)
	notifRepo := postgres.NewNotificationRepo(db)
	obsRepo := postgres.NewObservationRepo(db)

	// Wire providers.
	registry := provider.NewRegistry()
	registry.RegisterEvent(tvschedule.New(cfg.Providers.TVAPIBaseURL, log))
	registry.RegisterMetric(price.New(cfg.Providers.Price.UserAgent, cfg.Providers.Travel.Timeout, log))

	airP := travel.NewAirProvider(cfg.Providers.Travel.AirAPIKey, log)
	railP := travel.NewRailProvider(cfg.Providers.Travel.RailAPIKey, log)
	agg := travel.NewAggregator(log, airP, railP)
	registry.RegisterSearch(agg)

	// Evaluator.
	evaluator := scheduler.NewEvaluator(registry, obsRepo, clock.Real(), cfg.Providers.Travel.MaxHorizonDays)

	workerID := workerID(cfg)

	// Watcher (evaluates reminders → enqueues notifications).
	watcher := scheduler.NewWatcher(reminderRepo, notifRepo, evaluator, workerID, cfg.Scheduler.WatcherTick, log)

	// Telegram sender for delivery.
	bot, err := makeSender(cfg.Telegram.Token)
	if err != nil {
		log.Error("telegram sender init", "err", err)
		os.Exit(1)
	}
	sender := telegram.NewTelebotSender(bot)

	// Delivery worker.
	deliveryWorker := delivery.NewWorker(notifRepo, reminderRepo, sender, workerID, cfg.Scheduler.DeliveryTick, log)

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return watcher.Run(ctx) })
	g.Go(func() error { return deliveryWorker.Run(ctx) })

	log.Info("worker running", "worker_id", workerID)
	if err := g.Wait(); err != nil && err != context.Canceled {
		log.Error("worker exited", "err", err)
		os.Exit(1)
	}
}

func makeSender(token string) (*tele.Bot, error) {
	return tele.NewBot(tele.Settings{
		Token:       token,
		Synchronous: true,
	})
}

func workerID(cfg *config.Config) string {
	if cfg.Server.WorkerID != "" {
		return cfg.Server.WorkerID
	}
	host, _ := os.Hostname()
	return fmt.Sprintf("worker-%s", host)
}

// Compile-time interface check: ObservationRepo → scheduler.HistoryRepo.
var _ scheduler.HistoryRepo = (*postgres.ObservationRepo)(nil)
