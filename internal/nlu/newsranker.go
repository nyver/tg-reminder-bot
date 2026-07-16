package nlu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/nyver2k/remindertgbot/internal/provider"
)

// NewsRanker uses an LLM to pick and re-summarize the most important items
// among an RSS/Atom digest's heuristically pre-filtered candidates (see
// internal/provider/rss and scheduler.Evaluator.SetNewsRanker), replacing
// the keyword+recency heuristic score with an actual judgment call. It is
// optional and additive — callers fall back to the heuristic ranking on any
// error, so a flaky or unavailable LLM never blocks a digest.
type NewsRanker struct {
	complete func(context.Context, string) (string, error)
}

// NewConfiguredNewsRanker builds a NewsRanker using the same provider/model
// configuration as the NLU intent parser (see NewConfiguredLLMParser).
func NewConfiguredNewsRanker(providerName, apiKey, model, baseURL string, fallbackModels []string, timeout time.Duration, maxTokens int) (*NewsRanker, error) {
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	switch providerName {
	case "claude":
		client := anthropic.NewClient(option.WithAPIKey(apiKey))
		return &NewsRanker{complete: func(ctx context.Context, prompt string) (string, error) {
			msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
				Model: anthropic.F(model), MaxTokens: anthropic.F(int64(maxTokens)),
				Messages: anthropic.F([]anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))}),
			})
			if err != nil {
				return "", err
			}
			return extractText(msg), nil
		}}, nil
	case "openrouter":
		models := append([]string{model}, fallbackModels...)
		return &NewsRanker{complete: openRouterCompleter(apiKey, models, baseURL, timeout, maxTokens)}, nil
	default:
		return nil, fmt.Errorf("unsupported NLU provider %q", providerName)
	}
}

// rankedItem is the shape the LLM is asked to return for each selected item.
type rankedItem struct {
	Link    string `json:"link"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// newsRankSummaryPreviewLen caps how much of each candidate's existing
// summary is included in the prompt, to keep token usage predictable on
// feeds with long descriptions.
const newsRankSummaryPreviewLen = 300

// Rank asks the LLM to choose the most important items among candidates
// (already pre-filtered by the heuristic), translate each selected item's
// title into Russian and write a fresh Russian summary for it, returning at
// most topN items ordered by importance. Items are matched back to
// candidates by Link, so a hallucinated or unrecognized link in the model's
// response is dropped rather than guessed at.
func (r *NewsRanker) Rank(ctx context.Context, candidates []provider.NewsItem, topN int) ([]provider.NewsItem, error) {
	if len(candidates) == 0 || topN <= 0 {
		return nil, nil
	}

	raw, err := r.complete(ctx, buildNewsRankPrompt(candidates, topN))
	if err != nil {
		return nil, fmt.Errorf("news ranker: %w", err)
	}
	raw = extractJSONArray(raw)
	raw = sanitizeJSONStrings(raw)

	var picked []rankedItem
	if err := json.Unmarshal([]byte(raw), &picked); err != nil {
		return nil, fmt.Errorf("news ranker: json unmarshal: %w (raw: %.200s)", err, raw)
	}

	byLink := make(map[string]provider.NewsItem, len(candidates))
	for _, it := range candidates {
		if it.Link != "" {
			byLink[it.Link] = it
		}
	}

	out := make([]provider.NewsItem, 0, topN)
	for _, p := range picked {
		it, ok := byLink[strings.TrimSpace(p.Link)]
		if !ok {
			continue
		}
		if t := strings.TrimSpace(p.Title); t != "" {
			it.Title = t
		}
		if s := strings.TrimSpace(p.Summary); s != "" {
			it.Summary = s
		}
		out = append(out, it)
		if len(out) >= topN {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("news ranker: no candidates matched the model's response")
	}
	return out, nil
}

func buildNewsRankPrompt(candidates []provider.NewsItem, topN int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, `Ты — редактор новостного дайджеста. Тебе передан список новостей одной RSS/Atom-ленты — заголовки и описания могут быть на любом языке.

Выбери не более %d самых важных новостей (крупные события, серьёзные последствия, широкий охват), отсортируй по убыванию важности. Для каждой выбранной новости:
- переведи заголовок на русский язык (если он уже на русском — оставь как есть, не меняя смысл);
- напиши краткое саммари в 2-3 предложения на русском языке, по существу дела, без воды.

Верни ТОЛЬКО JSON-массив (без markdown, без пояснений) вида:
[{"link": "<ссылка из списка ниже>", "title": "<заголовок на русском>", "summary": "<саммари на русском>"}]

Список новостей:
`, topN)
	for i, it := range candidates {
		summary := it.Summary
		if r := []rune(summary); len(r) > newsRankSummaryPreviewLen {
			summary = string(r[:newsRankSummaryPreviewLen]) + "…"
		}
		fmt.Fprintf(&sb, "%d. %s\n   ссылка: %s\n", i+1, sanitizeForPrompt(it.Title), it.Link)
		if summary != "" {
			fmt.Fprintf(&sb, "   описание: %s\n", sanitizeForPrompt(summary))
		}
	}
	return sb.String()
}

// extractJSONArray trims LLM chatter around a JSON array response, mirroring
// extractJSON's handling of object responses elsewhere in this package.
func extractJSONArray(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "["); i >= 0 {
		s = s[i:]
	}
	if i := strings.LastIndex(s, "]"); i >= 0 {
		s = s[:i+1]
	}
	return s
}

// sanitizeJSONStrings escapes raw control characters (tab, newline, carriage
// return, and other C0 controls) that appear inside JSON string literals.
// Models frequently emit a literal newline/tab in a multi-sentence summary
// instead of the required "\n"/"\t" escape sequence, which makes the
// response invalid JSON per spec even though everything else about it is
// well-formed. Fixing that up here avoids discarding an otherwise-good
// response and falling back to the heuristic ranking unnecessarily.
func sanitizeJSONStrings(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	inString := false
	escaped := false
	for _, r := range s {
		if !inString {
			if r == '"' {
				inString = true
			}
			sb.WriteRune(r)
			continue
		}
		if escaped {
			sb.WriteRune(r)
			escaped = false
			continue
		}
		switch r {
		case '\\':
			escaped = true
			sb.WriteRune(r)
		case '"':
			inString = false
			sb.WriteRune(r)
		case '\t':
			sb.WriteString(`\t`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&sb, `\u%04x`, r)
			} else {
				sb.WriteRune(r)
			}
		}
	}
	return sb.String()
}
