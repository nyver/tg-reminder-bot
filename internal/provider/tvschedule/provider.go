package tvschedule

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

const (
	providerType    = "tv_program"
	maxResponseSize = 16 << 20
	channelIndexTTL = time.Hour
)

type Config struct {
	BaseURL string
	APIKey  string
	Timeout time.Duration
}

// Provider looks up TV broadcasts through EPG Service API v1.
type Provider struct {
	baseURL string
	apiKey  string
	client  *http.Client
	log     *slog.Logger

	indexMu        sync.RWMutex
	channels       []channel
	indexFetchedAt time.Time
}

func New(cfg Config, log *slog.Logger) *Provider {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Provider{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		client:  &http.Client{Timeout: timeout},
		log:     log,
	}
}

func (p *Provider) Type() string { return providerType }

func (p *Provider) Lookup(ctx context.Context, q provider.Query, from, to time.Time) ([]provider.Event, error) {
	if p.baseURL == "" || p.apiKey == "" {
		p.log.Warn("tvschedule: EPG Service is not configured, returning empty")
		return nil, nil
	}
	if strings.TrimSpace(q.Title) == "" || !from.Before(to) {
		return nil, nil
	}

	channel, err := p.resolveChannel(ctx, q.Params)
	if err != nil {
		return nil, err
	}
	if channel.ID == "" {
		return nil, nil
	}

	seen := make(map[string]struct{})
	var result []provider.Event
	for _, week := range weeksInRange(from, to) {
		schedule, err := p.schedule(ctx, channel.ID, week)
		if err != nil {
			return nil, err
		}
		if channel.DisplayName == "" && len(schedule.Channels) > 0 {
			channel.DisplayName = schedule.Channels[0].DisplayName
		}
		for _, programme := range schedule.Programmes {
			if !matches(programme.Title, q.Title) {
				continue
			}
			start, ok := programme.startTime(from.Location())
			if !ok || start.Before(from) || !start.Before(to) {
				continue
			}
			identity := programme.EventID
			if identity == "" {
				identity = fmt.Sprintf("%s:%d:%s", channel.ID, start.Unix(), normalize(programme.Title))
			}
			if _, ok := seen[identity]; ok {
				continue
			}
			seen[identity] = struct{}{}
			meta := map[string]string{
				"channel":    channel.DisplayName,
				"channel_id": channel.ID,
			}
			if programme.StopUnix != nil {
				meta["stop_at"] = time.Unix(*programme.StopUnix, 0).Format(time.RFC3339)
			}
			if programme.Description != "" {
				meta["description"] = programme.Description
			}
			result = append(result, provider.Event{
				Identity: identity,
				Title:    programme.Title,
				AnchorAt: start,
				Meta:     meta,
			})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].AnchorAt.Before(result[j].AnchorAt) })
	return result, nil
}

type channel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

type indexResponse struct {
	Channels []channel `json:"channel"`
}

func (p *Provider) resolveChannel(ctx context.Context, params map[string]string) (channel, error) {
	if id := strings.TrimSpace(params["channel_id"]); id != "" {
		name := strings.TrimSpace(params["channel"])
		if name == "" {
			name = id
		}
		return channel{ID: id, DisplayName: name}, nil
	}
	name := strings.TrimSpace(params["channel"])
	if name == "" {
		return channel{}, fmt.Errorf("tvschedule: event.params.channel or channel_id is required")
	}

	channels, err := p.channelIndex(ctx)
	if err != nil {
		return channel{}, fmt.Errorf("tvschedule index: %w", err)
	}
	bestScore := 0
	var best channel
	for _, candidate := range channels {
		if score := matchScore(candidate.DisplayName, name); score > bestScore {
			bestScore = score
			best = candidate
		}
	}
	return best, nil
}

func (p *Provider) channelIndex(ctx context.Context) ([]channel, error) {
	p.indexMu.RLock()
	if len(p.channels) > 0 && time.Since(p.indexFetchedAt) < channelIndexTTL {
		channels := p.channels
		p.indexMu.RUnlock()
		return channels, nil
	}
	p.indexMu.RUnlock()

	var index indexResponse
	if err := p.getJSON(ctx, "/v1/index", nil, &index); err != nil {
		return nil, err
	}
	p.indexMu.Lock()
	p.channels = index.Channels
	p.indexFetchedAt = time.Now()
	p.indexMu.Unlock()
	return index.Channels, nil
}

type scheduleResponse struct {
	Channels   []channel   `json:"channel"`
	Programmes []programme `json:"programms"`
}

type programme struct {
	Start       string `json:"start"`
	StartUnix   *int64 `json:"start_ut"`
	StopUnix    *int64 `json:"stop_ut"`
	EventID     string `json:"event_id"`
	Title       string `json:"title"`
	Description string `json:"desc_short"`
}

func (p *Provider) schedule(ctx context.Context, channelID, week string) (*scheduleResponse, error) {
	query := url.Values{"week": []string{week}}
	var schedule scheduleResponse
	path := "/v1/schedule/" + url.PathEscape(channelID)
	if err := p.getJSON(ctx, path, query, &schedule); err != nil {
		return nil, fmt.Errorf("tvschedule schedule for channel %s: %w", channelID, err)
	}
	return &schedule, nil
}

func (p *Provider) getJSON(ctx context.Context, path string, query url.Values, dst any) error {
	endpoint := p.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var problem struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(body, &problem)
		if problem.Detail == "" {
			problem.Detail = strings.TrimSpace(string(body))
		}
		return fmt.Errorf("EPG Service HTTP %d: %.300s", resp.StatusCode, problem.Detail)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode EPG Service response: %w", err)
	}
	return nil
}

func (p programme) startTime(loc *time.Location) (time.Time, bool) {
	if p.StartUnix != nil {
		return time.Unix(*p.StartUnix, 0), true
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, p.Start, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func weeksInRange(from, to time.Time) []string {
	start := from
	weekday := (int(start.Weekday()) + 6) % 7
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location()).AddDate(0, 0, -weekday)
	var weeks []string
	for week := start; week.Before(to); week = week.AddDate(0, 0, 7) {
		weeks = append(weeks, week.Format("20060102"))
	}
	return weeks
}

func matches(value, wanted string) bool {
	return matchScore(value, wanted) > 0
}

func matchScore(value, wanted string) int {
	value = normalize(value)
	wanted = normalize(wanted)
	if value == "" || wanted == "" {
		return 0
	}
	if value == wanted {
		return 3
	}
	if strings.Contains(value, wanted) || strings.Contains(wanted, value) {
		return 2
	}
	return 0
}

func normalize(value string) string {
	value = strings.ToLower(strings.ReplaceAll(value, "ё", "е"))
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, value)
}
