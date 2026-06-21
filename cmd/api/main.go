package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/nyver2k/remindertgbot/internal/config"
	"github.com/nyver2k/remindertgbot/internal/httpapi"
	"github.com/nyver2k/remindertgbot/internal/observability"
	"github.com/nyver2k/remindertgbot/internal/storage/postgres"
)

var version = "dev"

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load", "err", err)
		os.Exit(1)
	}

	log := observability.NewLogger(cfg.Server.LogLevel)
	log.Info("starting api", "version", version)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	db, err := postgres.New(ctx, cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		log.Error("db connect", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	server := httpapi.NewServer(
		postgres.NewReminderRepo(db),
		postgres.NewNotificationRepo(db),
		postgres.NewObservationRepo(db),
		log,
	)

	if err := server.Run(ctx, cfg.Server.APIPort); err != nil {
		log.Error("api exited", "err", err)
		os.Exit(1)
	}
}
