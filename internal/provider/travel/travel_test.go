package travel

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

func TestUnimplementedProvidersNeverReturnFabricatedOffers(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	query := provider.SearchQuery{
		Origin:      "MOW",
		Destination: "KZN",
		DateFrom:    time.Now().UTC(),
		DateTo:      time.Now().UTC().AddDate(0, 1, 0),
	}
	providers := []provider.SearchProvider{
		NewAirProvider("", log),
		NewAirProvider("configured-but-not-implemented", log),
		NewRailProvider("", log),
		NewRailProvider("configured-but-not-implemented", log),
	}
	for _, p := range providers {
		t.Run(p.Type(), func(t *testing.T) {
			offers, err := p.Search(context.Background(), query)
			if err != nil {
				t.Fatal(err)
			}
			if len(offers) != 0 {
				t.Fatalf("unimplemented provider returned fabricated offers: %+v", offers)
			}
		})
	}
}
