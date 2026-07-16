package travel

import (
	"context"
	"log/slog"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

const railMaxDays = 90 // RZhD booking window ~90 days

// RailProvider reserves the railway integration point. Live search is not
// implemented yet; it must never return fabricated offers.
type RailProvider struct {
	apiKey string
	log    *slog.Logger
}

func NewRailProvider(apiKey string, log *slog.Logger) *RailProvider {
	return &RailProvider{apiKey: apiKey, log: log}
}

func (p *RailProvider) Type() string { return "travel_rail" }

func (p *RailProvider) Search(ctx context.Context, q provider.SearchQuery) ([]provider.Offer, error) {
	// Clamp DateTo to the rail booking window.
	dateTo := q.DateTo
	if limit := q.DateFrom.AddDate(0, 0, railMaxDays); dateTo.After(limit) {
		dateTo = limit
		p.log.Info("rail: clamped date range to 90 days", "original_to", q.DateTo)
	}

	p.log.Warn("rail: live search is not implemented; returning no offers",
		"configured", p.apiKey != "",
		"origin", q.Origin, "destination", q.Destination,
		"from", q.DateFrom, "to", dateTo)
	return nil, nil
}
