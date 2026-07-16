package scheduler

import (
	"context"
	"fmt"
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

// fixedHistory returns a preset previous observation, letting tests exercise
// the "value dropped since last observation" branch of evaluateThreshold.
type fixedHistory struct {
	last *domain.Observation
}

func (f *fixedHistory) Last(ctx context.Context, reminderID uuid.UUID) (*domain.Observation, error) {
	if f.last == nil {
		return nil, domain.ErrNotFound
	}
	return f.last, nil
}

func (f *fixedHistory) Save(ctx context.Context, obs *domain.Observation) error { return nil }

// TestThresholdIdempotencyKeyDiffersPerReminder guards against a regression
// where two independent price-watch reminders for the same user, both
// dropping in price on the same day, computed the identical idempotency key
// (scoped only to user+date) — the second notification's INSERT silently
// no-oped against the notifications table's ON CONFLICT, dropping it.
func TestThresholdIdempotencyKeyDiffersPerReminder(t *testing.T) {
	now := time.Date(2026, 6, 22, 8, 0, 0, 0, time.UTC)
	registry := provider.NewRegistry()
	registry.RegisterMetric(metricProviderFunc(func(context.Context, provider.Query) (provider.Measurement, error) {
		return provider.Measurement{Available: true, Value: 90, Currency: "RUB"}, nil
	}))
	hist := &fixedHistory{last: &domain.Observation{Value: 100}}
	evaluator := NewEvaluator(registry, hist, clock.NewFake(now), 180, nil)

	base := domain.Reminder{
		UserID: 42, Kind: domain.KindConditional,
		Spec: domain.Spec{
			Trigger: domain.TriggerThreshold,
			Event:   domain.EventSpec{Type: "price", Params: map[string]string{"url": "https://shop.test/a"}},
		},
	}
	remA, remB := base, base
	remA.ID, remB.ID = uuid.New(), uuid.New()

	plannedA, err := evaluator.Evaluate(context.Background(), remA)
	if err != nil {
		t.Fatal(err)
	}
	plannedB, err := evaluator.Evaluate(context.Background(), remB)
	if err != nil {
		t.Fatal(err)
	}
	if len(plannedA) != 1 || len(plannedB) != 1 {
		t.Fatalf("expected 1 notification each, got %d and %d", len(plannedA), len(plannedB))
	}
	if plannedA[0].IdempotencyKey == plannedB[0].IdempotencyKey {
		t.Fatalf("two independent reminders for the same user collided on idempotency key %q", plannedA[0].IdempotencyKey)
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
	text := planned[0].Text
	if !strings.HasPrefix(text, MarkdownV2Prefix) {
		t.Fatalf("digest text should carry the MarkdownV2Prefix marker: %q", text)
	}
	if !strings.Contains(text, "*[Новость 1](https://example.com/1)*") {
		t.Fatalf("digest text should hyperlink the title, got: %s", text)
	}
	if !strings.Contains(text, "Саммари 1\\.") {
		t.Fatalf("digest text missing escaped summary: %s", text)
	}
	if len(hist.saved) != 1 {
		t.Fatalf("expected 1 observation saved, got %d", len(hist.saved))
	}
	if planned[0].IdempotencyKey == "" {
		t.Fatal("expected non-empty idempotency key")
	}
}

// TestNewsDigestSplitsIntoMultiplePlannedNotifications verifies that a
// digest too large for one Telegram message produces several
// PlannedNotifications, each with a distinct IdempotencyKey — otherwise a
// naive shared key would collide in the notifications table's ON CONFLICT
// and silently drop every chunk but the first.
func TestNewsDigestSplitsIntoMultiplePlannedNotifications(t *testing.T) {
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	items := make([]provider.NewsItem, 30)
	for i := range items {
		items[i] = provider.NewsItem{
			Title:   fmt.Sprintf("Заголовок новости номер %d с некоторой длиной текста", i),
			Link:    fmt.Sprintf("https://example.com/news/%d", i),
			Summary: strings.Repeat("Предложение саммари для проверки длины сообщения. ", 6),
		}
	}
	registry := provider.NewRegistry()
	registry.RegisterNews(newsProviderFunc(func(context.Context, provider.Query) ([]provider.NewsItem, error) {
		return items, nil
	}))
	rem := newsDigestReminder()
	rem.Spec.TopN = len(items)
	evaluator := NewEvaluator(registry, &fakeHistory{}, clock.NewFake(now), 180, nil)

	planned, err := evaluator.Evaluate(context.Background(), rem)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(planned) < 2 {
		t.Fatalf("expected multiple planned notifications for an oversized digest, got %d", len(planned))
	}

	seenKeys := make(map[string]bool, len(planned))
	for i, p := range planned {
		if len([]rune(p.Text)) > 4096 {
			t.Fatalf("planned[%d] exceeds Telegram's message length limit", i)
		}
		if seenKeys[p.IdempotencyKey] {
			t.Fatalf("planned[%d] reuses idempotency key %q — would collide in storage", i, p.IdempotencyKey)
		}
		seenKeys[p.IdempotencyKey] = true
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
	if !strings.Contains(planned[0].Text, mdv2Escape("LLM-саммари 2.")) {
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
	if !strings.Contains(planned[0].Text, "Новость 1") || !strings.Contains(planned[0].Text, mdv2Escape("Саммари 1.")) {
		t.Fatalf("digest text missing heuristic content: %s", planned[0].Text)
	}
}

// TestRenderNewsDigestEscapesAndLinksTitles verifies that renderNewsDigest
// produces MarkdownV2 with the marker prefix, a clickable title per item,
// and that MarkdownV2-special characters in feed-controlled text (title,
// summary) are escaped so they can't break the message's formatting.
func TestRenderNewsDigestEscapesAndLinksTitles(t *testing.T) {
	spec := domain.Spec{Event: domain.EventSpec{Title: "lenta.ru"}}
	items := []provider.NewsItem{
		{
			Title:       "OnePlus quits (for now) - report",
			Link:        "https://example.com/news?a=1&b=2",
			Summary:     "Цена выросла на 10.5%!",
			PublishedAt: time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC),
		},
	}

	texts := renderNewsDigest(spec, items)
	if len(texts) != 1 {
		t.Fatalf("expected a single chunk for one item, got %d: %v", len(texts), texts)
	}
	text := texts[0]

	if !strings.HasPrefix(text, MarkdownV2Prefix) {
		t.Fatalf("expected text to start with MarkdownV2Prefix, got: %q", text)
	}
	body := strings.TrimPrefix(text, MarkdownV2Prefix)

	wantLink := "*[OnePlus quits \\(for now\\) \\- report](https://example.com/news?a=1&b=2)*"
	if !strings.Contains(body, wantLink) {
		t.Fatalf("expected escaped hyperlinked title %q in: %s", wantLink, body)
	}
	if !strings.Contains(body, "Цена выросла на 10\\.5%\\!") {
		t.Fatalf("expected escaped summary in: %s", body)
	}
	if strings.Contains(body, "example.com/news?a=1&b=2\n") {
		t.Fatalf("raw URL should not appear on its own line anymore: %s", body)
	}
}

// TestRenderNewsDigestSplitsWhenTooLong verifies that a digest whose total
// rendered size would exceed Telegram's ~4096-character sendMessage limit is
// split into multiple chunks at item boundaries, each one individually
// under the limit and each carrying its own MarkdownV2Prefix marker.
func TestRenderNewsDigestSplitsWhenTooLong(t *testing.T) {
	items := make([]provider.NewsItem, 30)
	for i := range items {
		items[i] = provider.NewsItem{
			Title:   fmt.Sprintf("Заголовок новости номер %d с некоторой длиной текста", i),
			Link:    fmt.Sprintf("https://example.com/news/%d", i),
			Summary: strings.Repeat("Предложение саммари для проверки длины сообщения. ", 6),
		}
	}

	texts := renderNewsDigest(domain.Spec{Event: domain.EventSpec{Title: "lenta.ru"}}, items)
	if len(texts) < 2 {
		t.Fatalf("expected the digest to split into multiple messages, got %d", len(texts))
	}

	seenItems := 0
	for i, text := range texts {
		if !strings.HasPrefix(text, MarkdownV2Prefix) {
			t.Fatalf("chunk %d missing MarkdownV2Prefix: %q", i, text)
		}
		if len([]rune(text)) > 4096 {
			t.Fatalf("chunk %d exceeds Telegram's message length limit: %d runes", i, len([]rune(text)))
		}
		wantPart := fmt.Sprintf("часть %d из %d", i+1, len(texts))
		if !strings.Contains(text, wantPart) {
			t.Errorf("chunk %d header missing %q: %s", i, wantPart, text)
		}
		seenItems += strings.Count(text, "example.com/news/")
	}
	if seenItems != len(items) {
		t.Fatalf("expected every item to appear exactly once across chunks, got %d of %d", seenItems, len(items))
	}
}

func TestClampDigestSummaryTruncatesLongSummaries(t *testing.T) {
	long := strings.Repeat("а", digestSummaryMaxLen+100)
	got := clampDigestSummary(long)
	if r := []rune(got); len(r) > digestSummaryMaxLen+1 { // +1 for the trailing ellipsis rune
		t.Fatalf("clampDigestSummary did not truncate: %d runes", len(r))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected truncated summary to end with an ellipsis, got: %q", got[len(got)-10:])
	}
	if short := "коротко"; clampDigestSummary(short) != short {
		t.Fatalf("clampDigestSummary altered a short summary: %q", clampDigestSummary(short))
	}
}

// TestRenderNewsDigestEscapesURLParens verifies that a feed URL containing a
// closing paren doesn't prematurely terminate the MarkdownV2 link syntax.
func TestRenderNewsDigestEscapesURLParens(t *testing.T) {
	spec := domain.Spec{}
	items := []provider.NewsItem{
		{Title: "Title", Link: "https://example.com/wiki/Foo_(bar)"},
	}

	texts := renderNewsDigest(spec, items)
	if len(texts) != 1 {
		t.Fatalf("expected a single chunk for one item, got %d: %v", len(texts), texts)
	}
	text := texts[0]
	if !strings.Contains(text, "(https://example.com/wiki/Foo_(bar\\))") {
		t.Fatalf("expected escaped closing paren in link URL, got: %s", text)
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
