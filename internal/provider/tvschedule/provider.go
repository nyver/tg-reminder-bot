package tvschedule

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

const providerType = "tv_program"

// Provider implements provider.EventProvider by querying a TV schedule API/page.
type Provider struct {
	baseURL string
	log     *slog.Logger
}

func New(baseURL string, log *slog.Logger) *Provider {
	return &Provider{baseURL: baseURL, log: log}
}

func (p *Provider) Type() string { return providerType }

func (p *Provider) Lookup(ctx context.Context, q provider.Query, from, to time.Time) ([]provider.Event, error) {
	if p.baseURL == "" {
		p.log.Warn("tvschedule: TV_API_BASE_URL not configured, returning empty")
		return nil, nil
	}

	// TODO M3: implement actual HTTP fetch + goquery HTML parse + fuzzy match.
	// For now return a stub so the evaluator compiles and runs.
	p.log.Info("tvschedule: lookup", "title", q.Title, "from", from, "to", to)

	title := strings.ToLower(q.Title)
	_ = fmt.Sprintf("%s/schedule?date=%s", p.baseURL, from.Format("2006-01-02"))

	// Stub: simulate finding the event with a hardcoded anchor
	if strings.Contains(title, "квн") {
		anchor := time.Date(from.Year(), from.Month(), from.Day(), 21, 0, 0, 0, moscowTZ())
		if anchor.After(from) && anchor.Before(to) {
			return []provider.Event{{
				Identity: fmt.Sprintf("kvn:%s", anchor.Format("2006-01-02")),
				Title:    "КВН",
				AnchorAt: anchor,
				Meta:     map[string]string{"channel": "Первый канал"},
			}}, nil
		}
	}
	return nil, nil
}

func moscowTZ() *time.Location {
	loc, _ := time.LoadLocation("Europe/Moscow")
	return loc
}
