package postgres

import "context"

type HousekeepingRepo struct{ db *DB }

func NewHousekeepingRepo(db *DB) *HousekeepingRepo { return &HousekeepingRepo{db: db} }

// OptimizeDB runs database-engine-specific maintenance after pruning:
//   - SQLite: PRAGMA optimize (runs ANALYZE where the planner needs fresher stats),
//     PRAGMA wal_checkpoint(PASSIVE) (flushes WAL pages to the main file without
//     blocking writers), and VACUUM (rewrites the file reclaiming freed pages).
//   - PostgreSQL: no-op — autovacuum handles this automatically.
func (r *HousekeepingRepo) OptimizeDB(ctx context.Context) error {
	if r.db.Dialect != "sqlite" {
		return nil
	}
	for _, pragma := range []string{
		`PRAGMA optimize`,
		`PRAGMA wal_checkpoint(PASSIVE)`,
		`VACUUM`,
	} {
		if _, err := r.db.ExecContext(ctx, pragma); err != nil {
			return err
		}
	}
	return nil
}

// PruneNotifications deletes sent/failed notifications whose fire_at is older
// than retentionDays. Returns the number of deleted rows.
func (r *HousekeepingRepo) PruneNotifications(ctx context.Context, retentionDays int) (int64, error) {
	q := `DELETE FROM scheduled_notifications
	      WHERE status IN ('sent', 'failed')
	        AND fire_at < ` + r.db.DaysAgo(retentionDays)
	res, err := r.db.ExecContext(ctx, q)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneObservations keeps the most recent `keep` observations per reminder and
// deletes the rest, provided they are older than 1 day (safety window so that
// the current day's data is never truncated mid-run). Returns deleted row count.
func (r *HousekeepingRepo) PruneObservations(ctx context.Context, keep int) (int64, error) {
	q := r.db.Rebind(`
		DELETE FROM observations
		WHERE observed_at < ` + r.db.DaysAgo(1) + `
		  AND id NOT IN (
		    SELECT id FROM observations o2
		    WHERE o2.reminder_id = observations.reminder_id
		    ORDER BY o2.observed_at DESC
		    LIMIT $1
		  )`)
	res, err := r.db.ExecContext(ctx, q, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneExpiredCache removes provider_cache entries whose TTL has elapsed.
func (r *HousekeepingRepo) PruneExpiredCache(ctx context.Context) (int64, error) {
	q := `DELETE FROM provider_cache WHERE ` + ttlExpiry(r.db.Dialect) + ` < ` + r.db.Now()
	res, err := r.db.ExecContext(ctx, q)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneDoneReminders deletes reminders with status='done' whose updated_at is
// older than retentionDays. Cascades to scheduled_notifications and observations.
func (r *HousekeepingRepo) PruneDoneReminders(ctx context.Context, retentionDays int) (int64, error) {
	q := `DELETE FROM reminders
	      WHERE status = 'done'
	        AND updated_at < ` + r.db.DaysAgo(retentionDays)
	res, err := r.db.ExecContext(ctx, q)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
