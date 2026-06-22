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
	evaluator := NewEvaluator(registry, nil, clock.NewFake(now), 180, nil)
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

func TestAnchorTransientErrorReturnsNil(t *testing.T) {
	now := time.Date(2026, 6, 21, 17, 30, 0, 0, time.FixedZone("MSK", 3*60*60))
	registry := provider.NewRegistry()
	registry.RegisterEvent(eventProviderFunc(func(context.Context, provider.Query, time.Time, time.Time) ([]provider.Event, error) {
		return nil, &netError{temporary: true}
	}))
	evaluator := NewEvaluator(registry, nil, clock.NewFake(now), 180, nil)
	rem := domain.Reminder{
		ID: uuid.New(), Kind: domain.KindConditional,
		Spec: domain.Spec{
			Trigger:  domain.TriggerAnchor,
			LeadTime: domain.Duration{Duration: 5 * time.Hour},
			Event:    domain.EventSpec{Type: "tv_program", Title: "Время"},
		},
	}

	planned, err := evaluator.Evaluate(context.Background(), rem)
	if err != nil {
		t.Fatalf("expected nil error on transient provider failure, got: %v", err)
	}
	if len(planned) != 0 {
		t.Fatalf("expected no notifications on transient error, got %+v", planned)
	}
}

// TestAnchorOnlyNotifiesNearestOccurrence verifies that when the provider returns
// multiple upcoming occurrences (e.g. hourly news show), only the nearest one
// produces a notification. Notifying about all occurrences at once would flood
// the user; subsequent watcher ticks handle the rest.
func TestAnchorOnlyNotifiesNearestOccurrence(t *testing.T) {
	now := time.Date(2026, 6, 22, 8, 0, 0, 0, time.UTC)
	registry := provider.NewRegistry()
	registry.RegisterEvent(eventProviderFunc(func(context.Context, provider.Query, time.Time, time.Time) ([]provider.Event, error) {
		return []provider.Event{
			{Identity: "ch:1", Title: "Новости", AnchorAt: now.Add(6 * time.Hour)},  // 14:00 — nearest
			{Identity: "ch:2", Title: "Новости", AnchorAt: now.Add(12 * time.Hour)}, // 20:00
			{Identity: "ch:3", Title: "Новости", AnchorAt: now.Add(30 * time.Hour)}, // tomorrow
		}, nil
	}))
	evaluator := NewEvaluator(registry, nil, clock.NewFake(now), 180, nil)
	rem := domain.Reminder{
		ID: uuid.New(), Kind: domain.KindConditional,
		Spec: domain.Spec{
			Trigger:  domain.TriggerAnchor,
			LeadTime: domain.Duration{Duration: 5 * time.Hour},
			Event:    domain.EventSpec{Type: "tv_program", Title: "Новости"},
		},
	}

	planned, err := evaluator.Evaluate(context.Background(), rem)
	if err != nil {
		t.Fatal(err)
	}
	if len(planned) != 1 {
		t.Fatalf("want 1 notification, got %d: %+v", len(planned), planned)
	}
	wantFire := now.Add(6*time.Hour - 5*time.Hour) // AnchorAt - LeadTime = 09:00
	if !planned[0].FireAt.Equal(wantFire) {
		t.Fatalf("FireAt = %v, want %v", planned[0].FireAt, wantFire)
	}
}

type netError struct {
	temporary bool
}

func (e *netError) Error() string   { return "network error" }
func (e *netError) Temporary() bool { return e.temporary }
func (e *netError) Timeout() bool   { return e.temporary }
