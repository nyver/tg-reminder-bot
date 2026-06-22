package scheduler

import (
	"context"
	"log/slog"
	"time"
)

// HousekeepingRepo is satisfied by postgres.HousekeepingRepo.
type HousekeepingRepo interface {
	PruneNotifications(ctx context.Context, retentionDays int) (int64, error)
	PruneObservations(ctx context.Context, keep int) (int64, error)
	PruneExpiredCache(ctx context.Context) (int64, error)
	PruneDoneReminders(ctx context.Context, retentionDays int) (int64, error)
	// OptimizeDB performs engine-specific maintenance (VACUUM/WAL checkpoint for
	// SQLite; no-op for PostgreSQL which relies on autovacuum).
	OptimizeDB(ctx context.Context) error
}

const (
	notificationRetentionDays = 7
	reminderRetentionDays     = 30
	observationsKeep          = 10
)

// Janitor runs periodic database housekeeping to remove stale data.
type Janitor struct {
	repo HousekeepingRepo
	tick time.Duration
	log  *slog.Logger
}

func NewJanitor(repo HousekeepingRepo, tick time.Duration, log *slog.Logger) *Janitor {
	if log == nil {
		log = slog.Default()
	}
	return &Janitor{repo: repo, tick: tick, log: log}
}

func (j *Janitor) Run(ctx context.Context) error {
	j.sweep(ctx)
	ticker := time.NewTicker(j.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			j.sweep(ctx)
		}
	}
}

func (j *Janitor) sweep(ctx context.Context) {
	type task struct {
		name string
		fn   func() (int64, error)
	}
	tasks := []task{
		{"notifications", func() (int64, error) {
			return j.repo.PruneNotifications(ctx, notificationRetentionDays)
		}},
		{"observations", func() (int64, error) {
			return j.repo.PruneObservations(ctx, observationsKeep)
		}},
		{"cache", func() (int64, error) {
			return j.repo.PruneExpiredCache(ctx)
		}},
		{"reminders", func() (int64, error) {
			return j.repo.PruneDoneReminders(ctx, reminderRetentionDays)
		}},
	}
	for _, t := range tasks {
		n, err := t.fn()
		if err != nil {
			j.log.Error("housekeeping sweep failed", "table", t.name, "err", err)
			continue
		}
		if n > 0 {
			j.log.Info("housekeeping swept", "table", t.name, "deleted", n)
		}
	}

	if err := j.repo.OptimizeDB(ctx); err != nil {
		j.log.Error("housekeeping optimize failed", "err", err)
	}
}
