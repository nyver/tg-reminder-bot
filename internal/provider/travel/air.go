package travel

import (
	"context"
	"log/slog"
	"time"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

const airMaxDays = 330 // ~11 months ahead

// AirProvider implements provider.SearchProvider for aviation via a metasearch API.
// TODO M5: implement actual Aviasales/Travelpayouts range endpoint.
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

	if p.apiKey == "" {
		p.log.Warn("air: AIR_API_KEY not configured, returning stub offers")
		return stubAirOffers(q.DateFrom, q.Origin, q.Destination), nil
	}

	// TODO M5: call real API with range endpoint.
	p.log.Info("air: search", "origin", q.Origin, "destination", q.Destination,
		"from", q.DateFrom, "to", dateTo)
	return nil, nil
}

func stubAirOffers(from time.Time, origin, dest string) []provider.Offer {
	base := from.AddDate(0, 0, 5)
	loc, _ := time.LoadLocation("Europe/Moscow")
	return []provider.Offer{
		{
			Mode:      "air",
			Title:     "Победа DP-408",
			Carrier:   "Победа",
			Price:     319000, // 3190 руб. в копейках
			Currency:  "RUB",
			DepartAt:  time.Date(base.Year(), base.Month(), base.Day(), 6, 25, 0, 0, loc),
			Duration:  2*time.Hour + 5*time.Minute,
			Transfers: 0,
			BookURL:   "https://example.com/book/1",
			Signature: "air|DP|408|" + base.Format("2006-01-02"),
			Meta:      map[string]string{"origin": origin, "destination": dest},
		},
		{
			Mode:      "air",
			Title:     "Россия FV-6045",
			Carrier:   "Россия",
			Price:     354000,
			Currency:  "RUB",
			DepartAt:  time.Date(base.Year(), base.Month(), base.Day()+7, 21, 10, 0, 0, loc),
			Duration:  2*time.Hour + 10*time.Minute,
			Transfers: 0,
			BookURL:   "https://example.com/book/2",
			Signature: "air|FV|6045|" + base.AddDate(0, 0, 7).Format("2006-01-02"),
		},
	}
}
