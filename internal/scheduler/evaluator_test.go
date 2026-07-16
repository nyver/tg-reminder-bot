package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/clock"
	"github.com/nyver2k/remindertgbot/internal/domain"
	"github.com/nyver2k/remindertgbot/internal/provider"
)

type eventProviderFunc func(context.Context, provider.Query, time.Time, time.Time) ([]provider.Event, error)

type metricProviderFunc func(context.Context, provider.Query) (provider.Measurement, error)

func (eventProviderFunc) Type() string { return "tv_program" }

func (f eventProviderFunc) Lookup(ctx context.Context, q provider.Query, from, to time.Time) ([]provider.Event, error) {
	return f(ctx, q, from, to)
}

func (metricProviderFunc) Type() string { return "price" }

func (f metricProviderFunc) Sample(ctx context.Context, q provider.Query) (provider.Measurement, error) {
	return f(ctx, q)
}

type newsProviderFunc func(context.Context, provider.Query) ([]provider.NewsItem, error)

func (newsProviderFunc) Type() string { return "rss" }

func (f newsProviderFunc) Fetch(ctx context.Context, q provider.Query) ([]provider.NewsItem, error) {
	return f(ctx, q)
}

// fakeHistory is a minimal in-memory HistoryRepo for tests that exercise the
// digest path, which persists an Observation on every successful evaluation.
type fakeHistory struct {
	saved []*domain.Observation
}

func (f *fakeHistory) Last(ctx context.Context, reminderID uuid.UUID) (*domain.Observation, error) {
	return nil, domain.ErrNotFound
}

func (f *fakeHistory) Save(ctx context.Context, obs *domain.Observation) error {
	f.saved = append(f.saved, obs)
	return nil
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

func TestThresholdProviderErrorNotifiesUser(t *testing.T) {
	now := time.Date(2026, 6, 22, 8, 0, 0, 0, time.UTC)
	registry := provider.NewRegistry()
	registry.RegisterMetric(metricProviderFunc(func(context.Context, provider.Query) (provider.Measurement, error) {
		// Simulate HTTP 429 returned via Measurement.HTTPStatus.
		return provider.Measurement{Available: false, HTTPStatus: 429}, &netError{temporary: true}
	}))
	evaluator := NewEvaluator(registry, nil, clock.NewFake(now), 180, nil)
	rem := domain.Reminder{
		ID: uuid.New(), Kind: domain.KindConditional,
		Spec: domain.Spec{
			Trigger: domain.TriggerThreshold,
			Event: domain.EventSpec{
				Type:   "price",
				Params: map[string]string{"url": "https://shop.test/product"},
			},
		},
	}

	planned, err := evaluator.Evaluate(context.Background(), rem)
	if err != nil {
		t.Fatalf("expected nil error on provider failure, got: %v", err)
	}
	if len(planned) != 1 {
		t.Fatalf("expected 1 unavailability notification, got %d: %+v", len(planned), planned)
	}
	if !strings.Contains(planned[0].Text, "429") {
		t.Fatalf("notification text should contain HTTP status 429, got: %s", planned[0].Text)
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

func newsDigestReminder() domain.Reminder {
	return domain.Reminder{
		ID: uuid.New(), Kind: domain.KindConditional,
		Spec: domain.Spec{
			Trigger: domain.TriggerDigest,
			TopN:    5,
			Event: domain.EventSpec{
				Type:   "rss",
				Title:  "lenta.ru",
				Params: map[string]string{"url": "https://lenta.ru/rss"},
			},
		},
	}
}

func TestNewsDigestHappyPath(t *testing.T) {
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	registry := provider.NewRegistry()
	registry.RegisterNews(newsProviderFunc(func(context.Context, provider.Query) ([]provider.NewsItem, error) {
		return []provider.NewsItem{
			{Title: "Новость 1", Link: "https://example.com/1", Summary: "Саммари 1.", Score: 20},
			{Title: "Новость 2", Link: "https://example.com/2", Summary: "Саммари 2.", Score: 10},
		}, nil
	}))
	hist := &fakeHistory{}
	evaluator := NewEvaluator(registry, hist, clock.NewFake(now), 180, nil)

	planned, err := evaluator.Evaluate(context.Background(), newsDigestReminder())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(planned) != 1 {
		t.Fatalf("want 1 notification, got %d: %+v", len(planned), planned)
	}
	if !strings.Contains(planned[0].Text, "Новость 1") || !strings.Contains(planned[0].Text, "Саммари 1.") {
		t.Fatalf("digest text missing expected content: %s", planned[0].Text)
	}
	if len(hist.saved) != 1 {
		t.Fatalf("expected 1 observation saved, got %d", len(hist.saved))
	}
	if planned[0].IdempotencyKey == "" {
		t.Fatal("expected non-empty idempotency key")
	}
}

func TestNewsDigestProviderErrorRetriesNextTick(t *testing.T) {
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	registry := provider.NewRegistry()
	registry.RegisterNews(newsProviderFunc(func(context.Context, provider.Query) ([]provider.NewsItem, error) {
		return nil, &netError{temporary: true}
	}))
	evaluator := NewEvaluator(registry, &fakeHistory{}, clock.NewFake(now), 180, nil)

	planned, err := evaluator.Evaluate(context.Background(), newsDigestReminder())
	if err != nil {
		t.Fatalf("expected nil error on transient provider failure, got: %v", err)
	}
	if len(planned) != 0 {
		t.Fatalf("expected no notifications on provider error, got %+v", planned)
	}
}

func TestNewsDigestEmptyFeedNoNotification(t *testing.T) {
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	registry := provider.NewRegistry()
	registry.RegisterNews(newsProviderFunc(func(context.Context, provider.Query) ([]provider.NewsItem, error) {
		return nil, nil
	}))
	evaluator := NewEvaluator(registry, &fakeHistory{}, clock.NewFake(now), 180, nil)

	planned, err := evaluator.Evaluate(context.Background(), newsDigestReminder())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(planned) != 0 {
		t.Fatalf("expected no notifications for empty feed, got %+v", planned)
	}
}

type newsRankerFunc func(context.Context, []provider.NewsItem, int) ([]provider.NewsItem, error)

func (f newsRankerFunc) Rank(ctx context.Context, candidates []provider.NewsItem, topN int) ([]provider.NewsItem, error) {
	return f(ctx, candidates, topN)
}

// TestNewsDigestUsesLLMRankerWhenConfigured verifies that a configured
// NewsRanker's order/summary wins over the heuristic's own ranking.
func TestNewsDigestUsesLLMRankerWhenConfigured(t *testing.T) {
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	registry := provider.NewRegistry()
	registry.RegisterNews(newsProviderFunc(func(context.Context, provider.Query) ([]provider.NewsItem, error) {
		return []provider.NewsItem{
			{Title: "Новость 1", Link: "https://example.com/1", Summary: "Эвристическое саммари.", Score: 20},
			{Title: "Новость 2", Link: "https://example.com/2", Summary: "Саммари 2.", Score: 10},
		}, nil
	}))
	evaluator := NewEvaluator(registry, &fakeHistory{}, clock.NewFake(now), 180, nil)
	evaluator.SetNewsRanker(newsRankerFunc(func(_ context.Context, candidates []provider.NewsItem, topN int) ([]provider.NewsItem, error) {
		// Re-rank: item 2 first, with a fresh LLM-written summary.
		return []provider.NewsItem{
			{Title: "Новость 2", Link: "https://example.com/2", Summary: "LLM-саммари 2."},
		}, nil
	}))

	planned, err := evaluator.Evaluate(context.Background(), newsDigestReminder())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(planned) != 1 {
		t.Fatalf("want 1 notification, got %d: %+v", len(planned), planned)
	}
	if !strings.Contains(planned[0].Text, "LLM-саммари 2.") {
		t.Fatalf("digest text should use the ranker's summary, got: %s", planned[0].Text)
	}
	if strings.Contains(planned[0].Text, "Новость 1") {
		t.Fatalf("digest text should not include items the ranker dropped, got: %s", planned[0].Text)
	}
}

// TestNewsDigestFallsBackToHeuristicOnRankerError verifies that a failing
// NewsRanker never blocks the digest — the heuristic's own order is used.
func TestNewsDigestFallsBackToHeuristicOnRankerError(t *testing.T) {
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	registry := provider.NewRegistry()
	registry.RegisterNews(newsProviderFunc(func(context.Context, provider.Query) ([]provider.NewsItem, error) {
		return []provider.NewsItem{
			{Title: "Новость 1", Link: "https://example.com/1", Summary: "Саммари 1.", Score: 20},
		}, nil
	}))
	evaluator := NewEvaluator(registry, &fakeHistory{}, clock.NewFake(now), 180, nil)
	evaluator.SetNewsRanker(newsRankerFunc(func(context.Context, []provider.NewsItem, int) ([]provider.NewsItem, error) {
		return nil, &netError{temporary: true}
	}))

	planned, err := evaluator.Evaluate(context.Background(), newsDigestReminder())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(planned) != 1 {
		t.Fatalf("want 1 notification (heuristic fallback), got %d: %+v", len(planned), planned)
	}
	if !strings.Contains(planned[0].Text, "Новость 1") || !strings.Contains(planned[0].Text, "Саммари 1.") {
		t.Fatalf("digest text missing heuristic content: %s", planned[0].Text)
	}
}

func TestNewsDigestUnregisteredProviderErrors(t *testing.T) {
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	registry := provider.NewRegistry() // no rss provider registered
	evaluator := NewEvaluator(registry, &fakeHistory{}, clock.NewFake(now), 180, nil)

	_, err := evaluator.Evaluate(context.Background(), newsDigestReminder())
	if err == nil {
		t.Fatal("expected error when no news provider is registered")
	}
}
