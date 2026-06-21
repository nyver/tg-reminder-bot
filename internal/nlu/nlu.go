package nlu

import (
	"context"

	"github.com/nyver2k/remindertgbot/internal/domain"
)

// ParseResult is the output of the NLU pipeline.
type ParseResult struct {
	Spec       *domain.Spec
	Confidence float64
	Missing    []string // field names that need clarification
	EvalCron   string   // e.g. "0 9 * * *"
	FireAt     *string  // absolute: RFC3339 string
}

// Parser converts free-form Russian text into a structured Spec.
type Parser interface {
	Parse(ctx context.Context, text string) (*ParseResult, error)
}

// Chain tries parsers in order; returns the first with confidence >= threshold.
type Chain struct {
	parsers   []Parser
	threshold float64
}

func NewChain(threshold float64, parsers ...Parser) *Chain {
	return &Chain{parsers: parsers, threshold: threshold}
}

func (c *Chain) Parse(ctx context.Context, text string) (*ParseResult, error) {
	var best *ParseResult
	for _, p := range c.parsers {
		result, err := p.Parse(ctx, text)
		if err != nil {
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
	return &ParseResult{Spec: &domain.Spec{}, Confidence: 0}, nil
}
