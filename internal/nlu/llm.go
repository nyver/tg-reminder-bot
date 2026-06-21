package nlu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/nyver2k/remindertgbot/internal/domain"
)

// LLMParser uses a configured LLM to parse free-form Russian reminder text into a Spec.
type LLMParser struct {
	complete func(context.Context, string) (string, error)
	model    string
	loc      *time.Location
}

func NewLLMParser(apiKey string, loc *time.Location) *LLMParser {
	client := anthropic.NewClient(option.WithAPIKey(apiKey))
	if loc == nil {
		loc = time.UTC
	}
	return &LLMParser{
		complete: func(ctx context.Context, prompt string) (string, error) {
			msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
				Model:     anthropic.F("claude-haiku-4-5-20251001"),
				MaxTokens: anthropic.F(int64(1024)),
				Messages: anthropic.F([]anthropic.MessageParam{
					anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
				}),
			})
			if err != nil {
				return "", err
			}
			return extractText(msg), nil
		},
		model: "claude-haiku-4-5-20251001",
		loc:   loc,
	}
}

// NewConfiguredLLMParser creates an Anthropic or OpenRouter-backed parser.
func NewConfiguredLLMParser(provider, apiKey, model, baseURL string, loc *time.Location) (*LLMParser, error) {
	if loc == nil {
		loc = time.UTC
	}
	switch provider {
	case "claude":
		client := anthropic.NewClient(option.WithAPIKey(apiKey))
		return &LLMParser{model: model, loc: loc, complete: func(ctx context.Context, prompt string) (string, error) {
			msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
				Model: anthropic.F(model), MaxTokens: anthropic.F(int64(1024)),
				Messages: anthropic.F([]anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))}),
			})
			if err != nil {
				return "", err
			}
			return extractText(msg), nil
		}}, nil
	case "openrouter":
		if _, err := url.ParseRequestURI(baseURL); err != nil {
			return nil, fmt.Errorf("invalid OpenRouter base URL: %w", err)
		}
		return &LLMParser{model: model, loc: loc, complete: openRouterCompleter(apiKey, model, baseURL)}, nil
	default:
		return nil, fmt.Errorf("unsupported NLU provider %q", provider)
	}
}

func openRouterCompleter(apiKey, model, baseURL string) func(context.Context, string) (string, error) {
	const maxAttempts = 3
	client := &http.Client{Timeout: 30 * time.Second}
	endpoint := strings.TrimRight(baseURL, "/") + "/chat/completions"
	return func(ctx context.Context, prompt string) (string, error) {
		payload := struct {
			Model          string              `json:"model"`
			Messages       []map[string]string `json:"messages"`
			ResponseFormat map[string]string   `json:"response_format,omitempty"`
		}{Model: model, Messages: []map[string]string{{"role": "user", "content": prompt}}, ResponseFormat: map[string]string{"type": "json_object"}}
		body, err := json.Marshal(payload)
		if err != nil {
			return "", err
		}
		var lastErr error
		for attempt := 0; attempt < maxAttempts; attempt++ {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
			if err != nil {
				return "", err
			}
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				lastErr = err
			} else {
				data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				_ = resp.Body.Close()
				if readErr != nil {
					return "", readErr
				}
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					var result struct {
						Choices []struct {
							Message struct {
								Content string `json:"content"`
							} `json:"message"`
						} `json:"choices"`
					}
					if err := json.Unmarshal(data, &result); err != nil {
						return "", err
					}
					if len(result.Choices) == 0 {
						return "", fmt.Errorf("OpenRouter returned no choices")
					}
					return result.Choices[0].Message.Content, nil
				}
				lastErr = fmt.Errorf("OpenRouter HTTP %d: %.300s", resp.StatusCode, data)
				if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
					return "", lastErr
				}
				if delay, ok := retryAfter(resp.Header.Get("Retry-After")); ok && attempt+1 < maxAttempts {
					if err := waitForRetry(ctx, delay); err != nil {
						return "", err
					}
					continue
				}
			}
			if attempt+1 < maxAttempts {
				if err := waitForRetry(ctx, time.Duration(1<<attempt)*time.Second); err != nil {
					return "", err
				}
			}
		}
		return "", lastErr
	}
}

func retryAfter(value string) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	if seconds, err := time.ParseDuration(value + "s"); err == nil {
		return max(seconds, 0), true
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(time.Until(at), 0), true
	}
	return 0, false
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// llmResponse is the JSON schema we ask the LLM to produce.
type llmResponse struct {
	Kind        string   `json:"kind"`
	Trigger     string   `json:"trigger,omitempty"`
	Message     string   `json:"message,omitempty"`
	FireAt      string   `json:"fire_at,omitempty"`
	EvalCron    string   `json:"eval_cron,omitempty"`
	LeadTime    string   `json:"lead_time,omitempty"`
	TopN        int      `json:"top_n,omitempty"`
	HorizonDays int      `json:"horizon_days,omitempty"`
	Event       llmEvent `json:"event,omitempty"`
	Confidence  float64  `json:"confidence"`
	Missing     []string `json:"missing,omitempty"`
}

type llmEvent struct {
	Type   string            `json:"type,omitempty"`
	Title  string            `json:"title,omitempty"`
	Params map[string]string `json:"params,omitempty"`
}

func (p *LLMParser) Parse(ctx context.Context, text string) (*ParseResult, error) {
	now := time.Now().In(p.loc)
	prompt := buildPrompt(text, now)

	raw, err := p.complete(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("llm parse: %w", err)
	}
	raw = extractJSON(raw)

	var resp llmResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return nil, fmt.Errorf("llm json unmarshal: %w (raw: %.200s)", err, raw)
	}

	return mapToResult(&resp)
}

func buildPrompt(text string, now time.Time) string {
	return fmt.Sprintf(`Ты — система распознавания намерений (NLU) для бота напоминаний.
Сейчас: %s (MSK).

Распарси следующий запрос на русском языке и верни строго JSON без markdown-блоков.

JSON-схема ответа:
{
  "kind": "absolute|recurring|conditional",
  "trigger": "anchor|threshold|digest",
  "message": "текст напоминания",
  "fire_at": "RFC3339",
  "eval_cron": "cron-выражение",
  "lead_time": "3h",
  "top_n": 5,
  "horizon_days": 30,
  "event": {
    "type": "tv_program|price|travel",
    "title": "название",
    "params": {"key": "value"}
  },
  "confidence": 0.0-1.0,
  "missing": ["field1"]
}

Правила:
- «напомни завтра в 9:00 текст» → kind=absolute, fire_at=RFC3339
- «каждый день в 9:00» → kind=recurring, eval_cron="0 9 * * *"
- «за 3 часа до КВН на Первом» → kind=conditional, trigger=anchor, event.type=tv_program, event.params.channel="Первый канал"
- «уведоми при снижении цены» + URL → kind=conditional, trigger=threshold, event.type=price, event.params.url=<URL>
- «5 дешёвых билетов СПб→Калининград» → kind=conditional, trigger=digest, event.type=travel
- horizon_days: «неделя»→7, «месяц»→30, «2 недели»→14, «45 дней»→45, default→30
- confidence: 0.9+ всё ясно, 0.5-0.9 допущения, <0.5 неясно

Запрос: %s

Ответь только JSON.`, now.Format("02 Jan 2006 15:04 MST"), text)
}

func extractText(msg *anthropic.Message) string {
	var sb strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return sb.String()
}

func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "{"); i >= 0 {
		s = s[i:]
	}
	if i := strings.LastIndex(s, "}"); i >= 0 {
		s = s[:i+1]
	}
	return s
}

func mapToResult(resp *llmResponse) (*ParseResult, error) {
	spec := &domain.Spec{
		Message:     resp.Message,
		TopN:        resp.TopN,
		HorizonDays: resp.HorizonDays,
		Event: domain.EventSpec{
			Type:   resp.Event.Type,
			Title:  resp.Event.Title,
			Params: resp.Event.Params,
		},
	}

	if resp.Trigger != "" {
		spec.Trigger = domain.Trigger(resp.Trigger)
	}
	if resp.LeadTime != "" {
		d, err := time.ParseDuration(resp.LeadTime)
		if err == nil {
			spec.LeadTime = domain.Duration{Duration: d}
		}
	}

	result := &ParseResult{
		Kind:       domain.Kind(resp.Kind),
		Spec:       spec,
		Confidence: resp.Confidence,
		Missing:    resp.Missing,
		EvalCron:   resp.EvalCron,
	}
	if resp.FireAt != "" {
		result.FireAt = &resp.FireAt
	}
	return result, nil
}
