package travel

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

// dedup removes duplicate offers by Signature, keeping the cheapest.
func dedup(offers []provider.Offer) []provider.Offer {
	seen := make(map[string]provider.Offer)
	for _, o := range offers {
		sig := offerSignature(o)
		if existing, ok := seen[sig]; !ok || o.Price < existing.Price {
			seen[sig] = o
		}
	}
	result := make([]provider.Offer, 0, len(seen))
	for _, o := range seen {
		result = append(result, o)
	}
	return result
}

func offerSignature(o provider.Offer) string {
	if o.Signature != "" {
		return o.Signature
	}
	return fmt.Sprintf("%s|%s|%s|%s",
		o.Mode, strings.ToUpper(o.Carrier),
		o.Title, o.DepartAt.Format("2006-01-02T15:04"))
}

// PickTopN returns the N cheapest offers with deterministic tie-breaking.
func PickTopN(offers []provider.Offer, n int) []provider.Offer {
	sort.SliceStable(offers, func(i, j int) bool {
		if offers[i].Price != offers[j].Price {
			return offers[i].Price < offers[j].Price
		}
		if offers[i].Duration != offers[j].Duration {
			return offers[i].Duration < offers[j].Duration
		}
		return offers[i].Transfers < offers[j].Transfers
	})
	if n > 0 && len(offers) > n {
		return offers[:n]
	}
	return offers
}

// SplitModes parses "air,rail" into []string{"air","rail"}.
func SplitModes(s string) []string {
	if s == "" {
		return []string{"air", "rail"}
	}
	parts := strings.Split(s, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}
