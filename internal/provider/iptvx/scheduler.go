package iptvx

import (
	"context"
	"time"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

// Scheduler answers ad-hoc TV schedule queries against the EPG database.
// Unlike Provider it has no background runner — it queries on demand.
// Inject it into the Telegram handler via provider.TVScheduler.
type Scheduler struct {
	store EPGStore
}

var _ provider.TVScheduler = (*Scheduler)(nil)

func NewScheduler(store EPGStore) *Scheduler {
	return &Scheduler{store: store}
}

// QuerySchedule implements provider.TVScheduler.
// Returns broadcasts of programmes whose title fuzzy-matches title in [from, to).
// If channel is non-empty, restricts results to the best-matching channel.
func (s *Scheduler) QuerySchedule(ctx context.Context, title, channel string, from, to time.Time) ([]provider.TVShowtime, error) {
	channelID := ""
	if channel != "" {
		channels, err := s.store.Channels(ctx)
		if err != nil {
			return nil, err
		}
		ch := bestChannelMatch(channels, channel)
		if ch.ID == "" {
			return nil, nil // no matching channel
		}
		channelID = ch.ID
	}

	results, err := s.store.SearchProgrammes(ctx, title, channelID, from, to)
	if err != nil {
		return nil, err
	}

	var out []provider.TVShowtime
	for _, r := range results {
		if !matches(r.Title, title) {
			continue
		}
		out = append(out, provider.TVShowtime{
			Title:    r.Title,
			Channel:  r.ChannelName,
			StartsAt: r.StartsAt,
			EndsAt:   r.EndsAt,
		})
	}
	return out, nil
}

// ChannelDaySchedule implements provider.TVScheduler.
// Returns all programmes for the named channel in [from, to).
func (s *Scheduler) ChannelDaySchedule(ctx context.Context, channel string, from, to time.Time) (string, []provider.TVShowtime, error) {
	channels, err := s.store.Channels(ctx)
	if err != nil {
		return "", nil, err
	}
	ch := bestChannelMatch(channels, channel)
	if ch.ID == "" {
		return "", nil, nil
	}
	progs, err := s.store.Programmes(ctx, ch.ID, from, to)
	if err != nil {
		return "", nil, err
	}
	out := make([]provider.TVShowtime, 0, len(progs))
	for _, p := range progs {
		out = append(out, provider.TVShowtime{
			Title:    p.Title,
			Channel:  ch.DisplayName,
			StartsAt: p.StartsAt,
			EndsAt:   p.EndsAt,
		})
	}
	return ch.DisplayName, out, nil
}

// bestChannelMatch returns the channel whose display name best fuzzy-matches query.
func bestChannelMatch(channels []EPGChannel, query string) EPGChannel {
	var best EPGChannel
	bestScore := 0
	for _, ch := range channels {
		if s := matchScore(ch.DisplayName, query); s > bestScore {
			bestScore = s
			best = ch
		}
	}
	return best
}
