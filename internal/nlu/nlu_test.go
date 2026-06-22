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
