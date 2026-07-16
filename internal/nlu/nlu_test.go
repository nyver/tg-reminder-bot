package nlu

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nyver2k/remindertgbot/internal/domain"
)

type parserFunc func(context.Context, string) (*ParseResult, error)

func (f parserFunc) Parse(ctx context.Context, text string) (*ParseResult, error) {
	return f(ctx, text)
}

func TestChainReturnsParserErrorInsteadOfEmptyResult(t *testing.T) {
	wantErr := errors.New("provider unavailable")
	chain := NewChain(0.85,
		parserFunc(func(context.Context, string) (*ParseResult, error) {
			return &ParseResult{Spec: &domain.Spec{}, Confidence: 0}, nil
		}),
		parserFunc(func(context.Context, string) (*ParseResult, error) {
			return nil, wantErr
		}),
	)

	result, err := chain.Parse(context.Background(), "conditional reminder")
	if result != nil || !errors.Is(err, wantErr) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestChainReturnsMeaningfulLowConfidenceResult(t *testing.T) {
	want := &ParseResult{
		Kind:       domain.KindConditional,
		Spec:       &domain.Spec{Trigger: domain.TriggerAnchor, Event: domain.EventSpec{Type: "tv_program"}},
		Confidence: 0.5,
	}
	chain := NewChain(0.85, parserFunc(func(context.Context, string) (*ParseResult, error) {
		return want, nil
	}))

	got, err := chain.Parse(context.Background(), "conditional reminder")
	if err != nil || got != want {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestFastPathParsesTVAnchorReminder(t *testing.T) {
	parser := NewFastPath(time.UTC)
	result, err := parser.Parse(context.Background(), `уведоми за 5 часов до программы "Этот день победы" на первом канале`)
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != domain.KindConditional || result.Spec.Trigger != domain.TriggerAnchor {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Spec.LeadTime.Duration != 5*time.Hour || result.Spec.Event.Title != "Этот день победы" {
		t.Fatalf("unexpected spec: %+v", result.Spec)
	}
	if got := result.Spec.Event.Params["channel"]; got != "Первый канал" {
		t.Fatalf("channel = %q", got)
	}
}

func TestFastPathTVAnchorDaysAndWeeks(t *testing.T) {
	parser := NewFastPath(time.UTC)
	cases := []struct {
		text     string
		leadTime time.Duration
	}{
		{"уведоми за 1 день до КВН на Первом канале", 24 * time.Hour},
		{"уведоми за 3 дня до КВН на Первом канале", 3 * 24 * time.Hour},
		{"уведоми за 5 дней до КВН на Первом канале", 5 * 24 * time.Hour},
		{"уведоми за 1 неделю до КВН на Первом канале", 7 * 24 * time.Hour},
		{"уведоми за 2 недели до КВН на Первом канале", 14 * 24 * time.Hour},
	}
	for _, tc := range cases {
		result, err := parser.Parse(context.Background(), tc.text)
		if err != nil {
			t.Fatalf("%q: %v", tc.text, err)
		}
		if result.Kind != domain.KindConditional || result.Spec.Trigger != domain.TriggerAnchor {
			t.Fatalf("%q: unexpected kind/trigger: %+v", tc.text, result)
		}
		if result.Spec.LeadTime.Duration != tc.leadTime {
			t.Fatalf("%q: lead_time = %v, want %v", tc.text, result.Spec.LeadTime.Duration, tc.leadTime)
		}
		if result.Spec.Event.Title != "КВН" {
			t.Fatalf("%q: title = %q", tc.text, result.Spec.Event.Title)
		}
		if got := result.Spec.Event.Params["channel"]; got != "Первый канал" {
			t.Fatalf("%q: channel = %q", tc.text, got)
		}
	}
}

func TestFastPathParsesURLPriceDrop(t *testing.T) {
	t.Parallel()
	parser := NewFastPath(time.UTC)

	cases := []string{
		"https://www.dns-shop.ru/product/abc/ - уведоми при снижении цены",
		"https://www.dns-shop.ru/product/abc/ уведоми при снижении цены",
		"уведоми при снижении цены https://www.dns-shop.ru/product/abc/",
	}
	for _, text := range cases {
		result, err := parser.Parse(context.Background(), text)
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		if result.Kind != domain.KindConditional || result.Spec.Trigger != domain.TriggerThreshold {
			t.Fatalf("%q: unexpected kind/trigger: %+v", text, result)
		}
		if result.Spec.Event.Type != "price" {
			t.Fatalf("%q: event.type = %q, want price", text, result.Spec.Event.Type)
		}
		if got := result.Spec.Event.Params["url"]; got != "https://www.dns-shop.ru/product/abc/" {
			t.Fatalf("%q: url = %q", text, got)
		}
	}
}

func TestFastPathParsesRSSDigest(t *testing.T) {
	t.Parallel()
	parser := NewFastPath(time.UTC)

	result, err := parser.Parse(context.Background(), "каждый день в 18:00 создай дайджест новостей на основе https://lenta.ru/rss")
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != domain.KindConditional || result.Spec.Trigger != domain.TriggerDigest {
		t.Fatalf("unexpected kind/trigger: %+v", result)
	}
	if result.Spec.Event.Type != "rss" {
		t.Fatalf("event.type = %q, want rss", result.Spec.Event.Type)
	}
	if got := result.Spec.Event.Params["url"]; got != "https://lenta.ru/rss" {
		t.Fatalf("url = %q", got)
	}
	if result.EvalCron != "0 18 * * *" {
		t.Fatalf("eval_cron = %q, want %q", result.EvalCron, "0 18 * * *")
	}
}

func TestFastPathRSSDigestDefaultsAndTopN(t *testing.T) {
	t.Parallel()
	parser := NewFastPath(time.UTC)

	result, err := parser.Parse(context.Background(), "дайджест новостей по ленте https://lenta.ru/rss")
	if err != nil {
		t.Fatal(err)
	}
	if result.EvalCron != "0 9 * * *" {
		t.Fatalf("eval_cron = %q, want default 0 9 * * *", result.EvalCron)
	}
	if result.Spec.TopN != 0 {
		t.Fatalf("top_n = %d, want 0 (evaluator applies its own default)", result.Spec.TopN)
	}

	result, err = parser.Parse(context.Background(), "дайджест новостей топ 10 по ленте https://lenta.ru/rss")
	if err != nil {
		t.Fatal(err)
	}
	if result.Spec.TopN != 10 {
		t.Fatalf("top_n = %d, want 10", result.Spec.TopN)
	}
}

// TestFastPathIgnoresRSSDigestWithoutURL checks that a "дайджест" message
// without a URL is not misclassified as an rss digest reminder — it falls
// through to the plain recurring-text pattern instead.
func TestFastPathIgnoresRSSDigestWithoutURL(t *testing.T) {
	t.Parallel()
	parser := NewFastPath(time.UTC)

	result, err := parser.Parse(context.Background(), "каждый день в 18:00 покажи дайджест новостей")
	if err != nil {
		t.Fatal(err)
	}
	if result.Spec.Event.Type == "rss" {
		t.Fatalf("expected no rss match without a URL, got %+v", result)
	}
}

func TestMapToResultInfersRSSEventTrigger(t *testing.T) {
	resp := &llmResponse{
		Kind:       "conditional",
		Message:    "дайджест новостей",
		Confidence: 0.9,
		Event:      llmEvent{Type: "rss", Params: map[string]string{"url": "https://lenta.ru/rss"}},
		// trigger intentionally missing — inferred from event.type
	}
	result, err := mapToResult(resp)
	if err != nil {
		t.Fatal(err)
	}
	if result.Spec.Trigger != domain.TriggerDigest {
		t.Fatalf("trigger = %q, want digest", result.Spec.Trigger)
	}
}

func TestMapToResultRescuesRSSURLFromMessage(t *testing.T) {
	resp := &llmResponse{
		Kind:       "conditional",
		Trigger:    "digest",
		Message:    "дайджест новостей https://lenta.ru/rss",
		Confidence: 0.9,
		Event:      llmEvent{Type: "rss"},
		// URL is in message, not in event.params
	}
	result, err := mapToResult(resp)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Spec.Event.Params["url"]; got != "https://lenta.ru/rss" {
		t.Fatalf("url = %q", got)
	}
}

func TestMapToResultInfersPriceEventType(t *testing.T) {
	resp := &llmResponse{
		Kind:       "conditional",
		Trigger:    "threshold",
		Message:    "уведоми при снижении цены",
		Confidence: 0.95,
		Event:      llmEvent{Params: map[string]string{"url": "https://example.com/product"}},
		// event.type intentionally missing — LLM forgot to set it
	}
	result, err := mapToResult(resp)
	if err != nil {
		t.Fatal(err)
	}
	if result.Spec.Event.Type != "price" {
		t.Fatalf("event.type = %q, want price", result.Spec.Event.Type)
	}
}

func TestMapToResultRescuesURLFromMessage(t *testing.T) {
	resp := &llmResponse{
		Kind:       "conditional",
		Trigger:    "threshold",
		Message:    "https://example.com/product уведоми при снижении цены",
		Confidence: 0.95,
		Event:      llmEvent{Type: "price"},
		// URL is in message, not in event.params
	}
	result, err := mapToResult(resp)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Spec.Event.Params["url"]; got != "https://example.com/product" {
		t.Fatalf("url = %q", got)
	}
}

func TestParseLeadTime(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
		ok    bool
	}{
		{"3h", 3 * time.Hour, true},
		{"30m", 30 * time.Minute, true},
		{"24h", 24 * time.Hour, true},
		{"168h", 168 * time.Hour, true},
		{"1d", 24 * time.Hour, true},
		{"7d", 7 * 24 * time.Hour, true},
		{"1w", 7 * 24 * time.Hour, true},
		{"2w", 14 * 24 * time.Hour, true},
		{"7 days", 7 * 24 * time.Hour, true},
		{"1 week", 7 * 24 * time.Hour, true},
		{"1 день", 24 * time.Hour, true},
		{"1 неделю", 7 * 24 * time.Hour, true},
		{"2 недели", 14 * 24 * time.Hour, true},
		{"", 0, false},
		{"abc", 0, false},
	}
	for _, tc := range cases {
		got, err := parseLeadTime(tc.input)
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("parseLeadTime(%q) = %v, %v; want %v, nil", tc.input, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Errorf("parseLeadTime(%q) expected error, got %v", tc.input, got)
		}
	}
}
