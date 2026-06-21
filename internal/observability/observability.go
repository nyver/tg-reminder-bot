package observability

import (
	"log/slog"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	RemindersActive = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "reminders_active",
		Help: "Number of active reminders by trigger type",
	}, []string{"trigger"})

	NotificationsPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "notifications_pending",
		Help: "Number of pending notifications",
	})

	NotificationsSentTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "notifications_sent_total",
		Help: "Total sent notifications",
	})

	NotificationsFailedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "notifications_failed_total",
		Help: "Total failed notifications",
	})

	ProviderFetchErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "provider_fetch_errors_total",
		Help: "Provider fetch errors by provider name",
	}, []string{"provider"})

	TravelSearchTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "travel_search_total",
		Help: "Travel search attempts by source and result",
	}, []string{"source", "result"})

	DigestOffersCount = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "digest_offers_count",
		Help:    "Number of offers returned in a digest",
		Buckets: []float64{0, 1, 2, 3, 5, 10, 20, 50},
	})

	TravelHorizonDays = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "travel_horizon_days",
		Help:    "Requested travel horizon in days",
		Buckets: []float64{7, 14, 30, 60, 90, 120, 180},
	})
)

func NewLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
