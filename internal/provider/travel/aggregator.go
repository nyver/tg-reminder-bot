package travel

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
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
		g.Go(func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					a.log.Error("travel source panicked",
						"type", p.Type(), "panic", r, "stack", string(debug.Stack()))
					mu.Lock()
					failures++
					mu.Unlock()
					err = nil // degrade gracefully, same as a search error
				}
			}()
			offers, searchErr := p.Search(ctx, q)
			if searchErr != nil {
				a.log.Warn("travel source failed", "type", p.Type(), "err", searchErr)
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
