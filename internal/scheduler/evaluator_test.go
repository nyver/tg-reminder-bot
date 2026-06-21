package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/clock"
	"github.com/nyver2k/remindertgbot/internal/domain"
	"github.com/nyver2k/remindertgbot/internal/provider"
)

type eventProviderFunc func(context.Context, provider.Query, time.Time, time.Time) ([]provider.Event, error)

func (eventProviderFunc) Type() string { return "tv_program" }

func (f eventProviderFunc) Lookup(ctx context.Context, q provider.Query, from, to time.Time) ([]provider.Event, error) {
	return f(ctx, q, from, to)
}

func TestAnchorNotifiesImmediatelyWhenLeadTimeWasMissed(t *testing.T) {
	now := time.Date(2026, 6, 21, 17, 30, 0, 0, time.FixedZone("MSK", 3*60*60))
	registry := provider.NewRegistry()
	registry.RegisterEvent(eventProviderFunc(func(context.Context, provider.Query, time.Time, time.Time) ([]provider.Event, error) {
		return []provider.Event{{Identity: "event-1", Title: "Этот день победы", AnchorAt: now.Add(90 * time.Minute)}}, nil
	}))
	evaluator := NewEvaluator(registry, nil, clock.NewFake(now), 180)
	rem := domain.Reminder{
		ID: uuid.New(), Kind: domain.KindConditional,
		Spec: domain.Spec{
			Trigger:  domain.TriggerAnchor,
			LeadTime: domain.Duration{Duration: 5 * time.Hour},
			Event:    domain.EventSpec{Type: "tv_program", Title: "Этот день победы"},
		},
	}

	planned, err := evaluator.Evaluate(context.Background(), rem)
	if err != nil {
		t.Fatal(err)
	}
	if len(planned) != 1 || !planned[0].FireAt.Equal(now) {
		t.Fatalf("planned = %+v", planned)
	}
}
