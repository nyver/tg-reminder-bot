package travel

import (
	"context"
	"log/slog"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

const airMaxDays = 330 // ~11 months ahead

// AirProvider reserves the aviation integration point. Live search is not
// implemented yet; it must never return fabricated offers.
type AirProvider struct {
	apiKey string
	log    *slog.Logger
}

func NewAirProvider(apiKey string, log *slog.Logger) *AirProvider {
	return &AirProvider{apiKey: apiKey, log: log}
}

func (p *AirProvider) Type() string { return "travel_air" }

func (p *AirProvider) Search(ctx context.Context, q provider.SearchQuery) ([]provider.Offer, error) {
	// Clamp DateTo to the airline booking window.
	dateTo := q.DateTo
	if limit := q.DateFrom.AddDate(0, 0, airMaxDays); dateTo.After(limit) {
		dateTo = limit
		p.log.Info("air: clamped date range", "max_days", airMaxDays)
	}

	p.log.Warn("air: live search is not implemented; returning no offers",
		"configured", p.apiKey != "",
		"origin", q.Origin, "destination", q.Destination,
		"from", q.DateFrom, "to", dateTo)
	return nil, nil
}
