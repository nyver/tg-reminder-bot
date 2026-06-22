package tvschedule

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
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

type httpError struct {
	StatusCode int
	Detail     string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("EPG Service HTTP %d: %.300s", e.StatusCode, e.Detail)
}

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

// Provider looks up TV broadcasts through the EPG Service XMLTV API.
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

	ch, err := p.resolveChannel(ctx, q.Params)
	if err != nil {
		return nil, err
	}
	if ch.ID == "" {
		return nil, nil
	}

	seen := make(map[string]struct{})
	var result []provider.Event
	for _, week := range weeksInRange(from, to) {
		tv, err := p.schedule(ctx, ch.ID, week)
		if err != nil {
			var he *httpError
			if errors.As(err, &he) && he.StatusCode == http.StatusNotFound {
				p.log.Warn("tvschedule: no schedule for week, skipping", "channel_id", ch.ID, "week", week)
				continue
			}
			return nil, err
		}
		if ch.DisplayName == "" && len(tv.Channels) > 0 {
			ch.DisplayName = tv.Channels[0].displayName()
		}
		for _, prog := range tv.Programmes {
			if !matches(prog.Title, q.Title) {
				continue
			}
			start, ok := parseXMLTVTime(prog.Start)
			if !ok || start.Before(from) || !start.Before(to) {
				continue
			}
			identity := fmt.Sprintf("%s:%d:%s", ch.ID, start.Unix(), normalize(prog.Title))
			if _, ok := seen[identity]; ok {
				continue
			}
			seen[identity] = struct{}{}
			meta := map[string]string{
				"channel":    ch.DisplayName,
				"channel_id": ch.ID,
			}
			if prog.Stop != "" {
				if stop, ok := parseXMLTVTime(prog.Stop); ok {
					meta["stop_at"] = stop.UTC().Format(time.RFC3339)
				}
			}
			if desc := prog.shortDesc(); desc != "" {
				meta["description"] = desc
			}
			result = append(result, provider.Event{
				Identity: identity,
				Title:    prog.Title,
				AnchorAt: start,
				Meta:     meta,
			})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].AnchorAt.Before(result[j].AnchorAt) })
	return result, nil
}

// channel is the internal representation of a TV channel.
type channel struct {
	ID          string
	DisplayName string
}

// XMLTV response types (EPG Service returns standard XMLTV XML).

type xmlTV struct {
	XMLName    xml.Name     `xml:"tv"`
	Channels   []xmlChannel `xml:"channel"`
	Programmes []xmlProg    `xml:"programme"`
}

type xmlChannel struct {
	ID           string         `xml:"id,attr"`
	DisplayNames []xmlLangValue `xml:"display-name"`
}

type xmlLangValue struct {
	Lang  string `xml:"lang,attr"`
	Value string `xml:",chardata"`
}

func (c xmlChannel) displayName() string {
	// Prefer Russian, fall back to first available.
	var first string
	for _, d := range c.DisplayNames {
		if first == "" {
			first = d.Value
		}
		if d.Lang == "ru" || d.Lang == "" {
			return d.Value
		}
	}
	return first
}

type xmlDesc struct {
	Size  string `xml:"size,attr"`
	Value string `xml:",chardata"`
}

type xmlProg struct {
	Start string    `xml:"start,attr"`
	Stop  string    `xml:"stop,attr"`
	Title string    `xml:"title"`
	Descs []xmlDesc `xml:"desc"`
}

func (p xmlProg) shortDesc() string {
	var fallback string
	for _, d := range p.Descs {
		if fallback == "" {
			fallback = d.Value
		}
		if d.Size == "short" {
			return d.Value
		}
	}
	return fallback
}

// parseXMLTVTime parses XMLTV datetime strings: "20260621210000 +0300".
func parseXMLTVTime(s string) (time.Time, bool) {
	t, err := time.Parse("20060102150405 -0700", s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
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
	if best.ID == "" {
		p.log.Warn("tvschedule: channel not matched", "query", name, "normalized_query", normalize(name), "index_size", len(channels))
		if len(channels) <= 20 {
			names := make([]string, len(channels))
			for i, c := range channels {
				names[i] = c.DisplayName
			}
			p.log.Warn("tvschedule: channel candidates", "names", names)
		}
	} else {
		p.log.Info("tvschedule: channel resolved", "query", name, "normalized_query", normalize(name), "id", best.ID, "display_name", best.DisplayName, "score", bestScore)
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

	var tv xmlTV
	if err := p.getXML(ctx, "/v1/index", nil, &tv); err != nil {
		return nil, err
	}
	p.log.Debug("tvschedule: index fetched", "raw_channel_count", len(tv.Channels))
	channels := make([]channel, 0, len(tv.Channels))
	for _, c := range tv.Channels {
		if c.ID != "" {
			dn := c.displayName()
			channels = append(channels, channel{ID: c.ID, DisplayName: dn})
			p.log.Debug("tvschedule: channel in index", "id", c.ID, "display_name", dn)
		}
	}
	p.log.Info("tvschedule: channel index loaded", "total", len(channels))
	p.indexMu.Lock()
	p.channels = channels
	p.indexFetchedAt = time.Now()
	p.indexMu.Unlock()
	return channels, nil
}

func (p *Provider) schedule(ctx context.Context, channelID, week string) (*xmlTV, error) {
	query := url.Values{"week": []string{week}}
	var tv xmlTV
	path := "/v1/schedule/" + url.PathEscape(channelID)
	if err := p.getXML(ctx, path, query, &tv); err != nil {
		return nil, fmt.Errorf("tvschedule schedule for channel %s: %w", channelID, err)
	}
	return &tv, nil
}

func (p *Provider) getXML(ctx context.Context, path string, query url.Values, dst any) error {
	endpoint := p.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	p.log.Debug("tvschedule: http request", "method", "GET", "url", endpoint)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Accept", "application/xml")

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		p.log.Error("tvschedule: http request failed", "url", endpoint, "err", err)
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	elapsed := time.Since(start)
	if err != nil {
		return err
	}
	p.log.Debug("tvschedule: http response", "url", endpoint, "status", resp.StatusCode, "bytes", len(body), "elapsed_ms", elapsed.Milliseconds(), "body", truncate(string(body), 4096))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var problem struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(body, &problem)
		if problem.Detail == "" {
			problem.Detail = strings.TrimSpace(string(body))
		}
		p.log.Error("tvschedule: http error response", "url", endpoint, "status", resp.StatusCode, "detail", problem.Detail)
		return &httpError{StatusCode: resp.StatusCode, Detail: problem.Detail}
	}
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	if err := xml.Unmarshal(body, dst); err != nil {
		p.log.Error("tvschedule: xml decode failed", "url", endpoint, "body_preview", string(body[:min(200, len(body))]))
		return fmt.Errorf("decode EPG Service XML (HTTP %d, body: %.200s): %w", resp.StatusCode, body, err)
	}
	return nil
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
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
