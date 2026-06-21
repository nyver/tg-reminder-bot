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
