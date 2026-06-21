package travel

import (
	"context"
	"log/slog"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

const railMaxDays = 90 // RZhD booking window ~90 days

// RailProvider implements provider.SearchProvider for railways.
// Note: SPb→Kaliningrad rail involves transit through Lithuania (UPD-ZhD),
// which may result in empty results — treated as partial, not error.
// TODO M5: implement actual RZhD/aggregator endpoint.
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

	if p.apiKey == "" {
		p.log.Warn("rail: RAIL_API_KEY not configured, returning empty (Kaliningrad transit limitation)")
		return nil, nil
	}

	// TODO M5: call real RZhD API with range endpoint.
	p.log.Info("rail: search", "origin", q.Origin, "destination", q.Destination,
		"from", q.DateFrom, "to", dateTo)
	return nil, nil
}
