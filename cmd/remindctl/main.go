package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nyver2k/remindertgbot/internal/config"
	internalmigrations "github.com/nyver2k/remindertgbot/internal/migrations"
	sqlitemigrations "github.com/nyver2k/remindertgbot/internal/migrations/sqlite"
	"github.com/nyver2k/remindertgbot/internal/provider"
	"github.com/nyver2k/remindertgbot/internal/provider/travel"
	"github.com/nyver2k/remindertgbot/internal/scheduler"
	"github.com/nyver2k/remindertgbot/internal/storage/postgres"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "migrate":
		runMigrate(os.Args[2:])
	case "reminders":
		runReminders(os.Args[2:])
	case "provider":
		runProvider(os.Args[2:])
	case "notifications":
		runNotifications(os.Args[2:])
	case "watcher":
		fmt.Println("watcher run-once: not yet implemented in CLI mode (use worker service)")
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`remindctl — CLI для управления ботом напоминаний

Команды:
  migrate up|down|status         — управление миграциями
  reminders list --user <id>     — список напоминаний пользователя
  provider travel                — проверка конфигурации (live-поиск пока недоступен)
    --from <город>
    --to <город>
    --modes air,rail
    --horizon-days 30
    --top 5
  notifications retry <id>       — повторить отправку уведомления
  version                        — версия`)
}

func runMigrate(args []string) {
	cfg := mustConfig()
	dialect, driver := "postgres", "pgx"
	goose.SetBaseFS(internalmigrations.FS)
	if cfg.Database.Driver == "sqlite" {
		dialect, driver = "sqlite3", "sqlite"
		goose.SetBaseFS(sqlitemigrations.FS)
	}
	if err := goose.SetDialect(dialect); err != nil {
		exitf("goose dialect: %v", err)
	}
	db, err := sql.Open(driver, cfg.Database.DSN)
	if err != nil {
		exitf("db open: %v", err)
	}
	defer db.Close()

	cmd := "up"
	if len(args) > 0 {
		cmd = args[0]
	}
	if err := goose.RunContext(context.Background(), cmd, db, "."); err != nil {
		exitf("migrate %s: %v", cmd, err)
	}
	fmt.Printf("Migration '%s' done.\n", cmd)
}

func runReminders(args []string) {
	if len(args) < 1 {
		exitf("usage: reminders list --user <id>")
	}
	cfg := mustConfig()
	ctx := context.Background()
	db, err := postgres.New(ctx, cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		exitf("db: %v", err)
	}
	repo := postgres.NewReminderRepo(db)

	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("reminders list", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		userID := fs.Int64("user", 0, "Telegram user ID")
		if err := fs.Parse(args[1:]); err != nil {
			exitf("usage: reminders list --user <id>")
		}
		if *userID <= 0 {
			exitf("usage: reminders list --user <id>")
		}
		rems, err := repo.ListByUser(ctx, *userID)
		if err != nil {
			exitf("list: %v", err)
		}
		if len(rems) == 0 {
			fmt.Println("Нет активных напоминаний.")
			return
		}
		for _, r := range rems {
			fmt.Printf("[%s] %s [%s] %s\n", r.ID, r.Status, r.Kind, r.RawText)
		}
	default:
		exitf("unknown subcommand: %s", args[0])
	}
}

func runProvider(args []string) {
	if len(args) < 1 || args[0] != "travel" {
		exitf("usage: provider travel [flags]")
	}
	cfg := mustConfig()
	fs := flag.NewFlagSet("provider travel", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	origin := fs.String("from", "", "origin city")
	dest := fs.String("to", "", "destination city")
	modes := fs.String("modes", "", "comma-separated modes")
	horizonDays := fs.Int("horizon-days", 30, "search horizon in days")
	top := fs.Int("top", 5, "number of offers to print")
	if err := fs.Parse(args[1:]); err != nil {
		exitf("usage: provider travel [flags]")
	}
	if *origin == "" || *dest == "" {
		exitf("--from and --to are required")
	}
	if *horizonDays <= 0 {
		exitf("--horizon-days must be positive")
	}
	if *top <= 0 {
		exitf("--top must be positive")
	}

	log := slog.Default()
	agg := travel.NewAggregator(log,
		travel.NewAirProvider(cfg.Providers.Travel.AirAPIKey, log),
		travel.NewRailProvider(cfg.Providers.Travel.RailAPIKey, log),
	)

	from := startOfDay(time.Now())
	to := from.AddDate(0, 0, *horizonDays)
	q := provider.SearchQuery{
		Origin:      *origin,
		Destination: *dest,
		DateFrom:    from,
		DateTo:      to,
		Modes:       splitModes(*modes),
		Limit:       50,
	}

	fmt.Printf("Поиск %s → %s, горизонт %d дней (%s – %s)\n",
		*origin, *dest, *horizonDays, from.Format("02.01"), to.Format("02.01.06"))

	offers, err := agg.Search(context.Background(), q)
	if err != nil {
		exitf("search: %v", err)
	}

	topOffers := scheduler.PickTopN(offers, *top)
	if len(topOffers) == 0 {
		fmt.Println("Travel providers are not implemented; no live offers are available.")
		return
	}
	fmt.Printf("\nТоп-%d предложений:\n", *top)
	for i, o := range topOffers {
		fmt.Printf("%d. [%s] %s · %s · %.0f руб.\n",
			i+1, o.Mode, o.Title, o.DepartAt.Format("02 Jan 15:04"), float64(o.Price)/100)
	}
}

func runNotifications(args []string) {
	if len(args) < 2 || args[0] != "retry" {
		exitf("usage: notifications retry <id>")
	}
	cfg := mustConfig()
	ctx := context.Background()
	db, err := postgres.New(ctx, cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		exitf("db: %v", err)
	}
	id, err := uuid.Parse(args[1])
	if err != nil {
		exitf("invalid id: %v", err)
	}
	if err := postgres.NewNotificationRepo(db).Retry(ctx, id); err != nil {
		exitf("retry: %v", err)
	}
	fmt.Println("Queued for retry.")
}

func mustConfig() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		exitf("config: %v", err)
	}
	return cfg
}

func exitf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func splitModes(s string) []string {
	if s == "" {
		return []string{"air", "rail"}
	}
	parts := strings.Split(s, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}
