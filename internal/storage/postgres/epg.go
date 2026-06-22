package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/nyver2k/remindertgbot/internal/provider/iptvx"
)

const epgBatchSize = 100 // rows per multi-value INSERT

type EPGRepo struct{ db *DB }

func NewEPGRepo(db *DB) *EPGRepo { return &EPGRepo{db: db} }

// ImportEPG atomically replaces all EPG data: truncates both tables then
// inserts the new channels and programmes in a single transaction.
// Using multi-row INSERT batches for efficiency (avoids N round-trips).
func (r *EPGRepo) ImportEPG(ctx context.Context, channels []iptvx.EPGChannel, progs []iptvx.EPGProgramme) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM epg_programmes`); err != nil {
		return fmt.Errorf("epg: clear programmes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM epg_channels`); err != nil {
		return fmt.Errorf("epg: clear channels: %w", err)
	}

	if err := insertChannels(ctx, tx, r.db, channels); err != nil {
		return fmt.Errorf("epg: insert channels: %w", err)
	}
	if err := insertProgrammes(ctx, tx, r.db, progs); err != nil {
		return fmt.Errorf("epg: insert programmes: %w", err)
	}

	return tx.Commit()
}

// Channels returns all EPG channels (used for fuzzy name matching in the provider).
func (r *EPGRepo) Channels(ctx context.Context) ([]iptvx.EPGChannel, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, display_name FROM epg_channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []iptvx.EPGChannel
	for rows.Next() {
		var ch iptvx.EPGChannel
		if err := rows.Scan(&ch.ID, &ch.DisplayName); err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}

// Programmes returns programmes for channelID where starts_at ∈ [from, to).
func (r *EPGRepo) Programmes(ctx context.Context, channelID string, from, to time.Time) ([]iptvx.EPGProgramme, error) {
	q := r.db.Rebind(`
		SELECT title, starts_at, ends_at, description
		FROM   epg_programmes
		WHERE  channel_id = $1
		  AND  starts_at >= $2
		  AND  starts_at <  $3
		ORDER  BY starts_at`)

	rows, err := r.db.QueryContext(ctx, q, channelID, from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []iptvx.EPGProgramme
	for rows.Next() {
		var p iptvx.EPGProgramme
		var endsAt sql.NullTime
		var desc sql.NullString
		if err := rows.Scan(&p.Title, &p.StartsAt, &endsAt, &desc); err != nil {
			return nil, err
		}
		if endsAt.Valid {
			p.EndsAt = endsAt.Time
		}
		p.Desc = desc.String
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- batch insert helpers ---

func insertChannels(ctx context.Context, tx *sql.Tx, db *DB, channels []iptvx.EPGChannel) error {
	for i := 0; i < len(channels); i += epgBatchSize {
		end := i + epgBatchSize
		if end > len(channels) {
			end = len(channels)
		}
		batch := channels[i:end]

		var sb strings.Builder
		sb.WriteString(`INSERT INTO epg_channels (id, display_name) VALUES `)
		args := make([]any, 0, len(batch)*2)
		for j, ch := range batch {
			if j > 0 {
				sb.WriteByte(',')
			}
			n := j*2 + 1
			fmt.Fprintf(&sb, "($%d,$%d)", n, n+1)
			args = append(args, ch.ID, ch.DisplayName)
		}
		if _, err := tx.ExecContext(ctx, db.Rebind(sb.String()), args...); err != nil {
			return err
		}
	}
	return nil
}

func insertProgrammes(ctx context.Context, tx *sql.Tx, db *DB, progs []iptvx.EPGProgramme) error {
	const cols = 5
	for i := 0; i < len(progs); i += epgBatchSize {
		end := i + epgBatchSize
		if end > len(progs) {
			end = len(progs)
		}
		batch := progs[i:end]

		var sb strings.Builder
		sb.WriteString(`INSERT INTO epg_programmes (channel_id, title, starts_at, ends_at, description) VALUES `)
		args := make([]any, 0, len(batch)*cols)
		for j, p := range batch {
			if j > 0 {
				sb.WriteByte(',')
			}
			n := j*cols + 1
			fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d)", n, n+1, n+2, n+3, n+4)

			var endsAt any
			if !p.EndsAt.IsZero() {
				endsAt = p.EndsAt.UTC()
			}
			var desc any
			if p.Desc != "" {
				desc = p.Desc
			}
			args = append(args, p.ChannelID, p.Title, p.StartsAt.UTC(), endsAt, desc)
		}
		if _, err := tx.ExecContext(ctx, db.Rebind(sb.String()), args...); err != nil {
			return err
		}
	}
	return nil
}
