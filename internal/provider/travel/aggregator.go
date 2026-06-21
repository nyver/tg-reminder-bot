package travel

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/nyver2k/remindertgbot/internal/domain"
	"github.com/nyver2k/remindertgbot/internal/observability"
	"github.com/nyver2k/remindertgbot/internal/provider"
	"golang.org/x/sync/errgroup"
)

const providerType = "travel"

// Aggregator fans out to multiple SearchProviders and merges results.
// A single provider failure causes partial results, not a full abort.
type Aggregator struct {
	providers []provider.SearchProvider
	log       *slog.Logger
}

func NewAggregator(log *slog.Logger, providers ...provider.SearchProvider) *Aggregator {
	return &Aggregator{providers: providers, log: log}
}

func (a *Aggregator) Type() string { return providerType }

func (a *Aggregator) Search(ctx context.Context, q provider.SearchQuery) ([]provider.Offer, error) {
	g, ctx := errgroup.WithContext(ctx)
	var mu sync.Mutex
	var all []provider.Offer
	var failures int

	for _, p := range a.providers {
		p := p
		g.Go(func() error {
			offers, err := p.Search(ctx, q)
			if err != nil {
				a.log.Warn("travel source failed", "type", p.Type(), "err", err)
				observability.TravelSearchTotal.WithLabelValues(p.Type(), "error").Inc()
				mu.Lock()
				failures++
				mu.Unlock()
				return nil // degrade gracefully
			}
			observability.TravelSearchTotal.WithLabelValues(p.Type(), "ok").Inc()
			mu.Lock()
			all = append(all, offers...)
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()

	if len(all) == 0 && failures > 0 {
		return nil, errors.Join(domain.ErrAllSourcesFailed,
			errors.New("all travel sources failed"))
	}
	return dedup(all), nil
}
