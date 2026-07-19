package nlu

import (
	"context"
	"fmt"
	"time"

	"github.com/nyver2k/remindertgbot/internal/domain"
)

// ParseResult is the output of the NLU pipeline.
type ParseResult struct {
	Kind       domain.Kind
	Spec       *domain.Spec
	Confidence float64
	Missing    []string // field names that need clarification
	EvalCron   string   // e.g. "0 9 * * *"
	FireAt     *string  // absolute: RFC3339 string
}

// Parser converts free-form Russian text into a structured Spec. loc is the
// user's timezone for this request; implementations fall back to UTC if nil.
type Parser interface {
	Parse(ctx context.Context, text string, loc *time.Location) (*ParseResult, error)
}

// Chain tries parsers in order; returns the first with confidence >= threshold.
type Chain struct {
	parsers   []Parser
	threshold float64
}

func NewChain(threshold float64, parsers ...Parser) *Chain {
	return &Chain{parsers: parsers, threshold: threshold}
}

func (c *Chain) Parse(ctx context.Context, text string, loc *time.Location) (*ParseResult, error) {
	var best *ParseResult
	var lastErr error
	for _, p := range c.parsers {
		result, err := p.Parse(ctx, text, loc)
		if err != nil {
			lastErr = err
			continue
		}
		if emptyResult(result) {
			continue
		}
		if result.Confidence >= c.threshold {
			return result, nil
		}
		if best == nil || result.Confidence > best.Confidence {
			best = result
		}
	}
	if best != nil {
		return best, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("all NLU parsers failed: %w", lastErr)
	}
	return nil, fmt.Errorf("could not recognize reminder")
}

func emptyResult(result *ParseResult) bool {
	if result == nil || result.Spec == nil {
		return true
	}
	return result.Confidence == 0 && result.Kind == "" && result.FireAt == nil &&
		result.EvalCron == "" && result.Spec.Trigger == "" &&
		result.Spec.Event.Type == "" && result.Spec.Message == ""
}
