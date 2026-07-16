// Package rss implements provider.NewsProvider by fetching and importance-
// ranking items from an RSS 2.0 or Atom feed at a user-supplied URL.
package rss

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nyver2k/remindertgbot/internal/netsafe"
	"github.com/nyver2k/remindertgbot/internal/provider"
)

const (
	providerType = "rss"
	maxFeedBody  = 5 << 20 // 5 MB is enough for any news feed
)

// Provider implements provider.NewsProvider for RSS 2.0 and Atom feeds.
type Provider struct {
	httpClient *http.Client
	log        *slog.Logger
}

func New(timeout time.Duration, log *slog.Logger) *Provider {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Provider{
		httpClient: netsafe.SafeClient(timeout),
		log:        log,
	}
}

func (p *Provider) Type() string { return providerType }

func (p *Provider) Fetch(ctx context.Context, q provider.Query) ([]provider.NewsItem, error) {
	feedURL := strings.TrimSpace(q.Params["url"])
	if feedURL == "" {
		return nil, fmt.Errorf("rss provider: url param required")
	}
	// SSRF guard: reject unsupported schemes and private/loopback/link-local
	// hosts before making any request with a user-supplied URL.
	if err := netsafe.ValidateURL(ctx, feedURL); err != nil {
		return nil, fmt.Errorf("rss provider: %w", err)
	}

	body, err := p.fetch(ctx, feedURL)
	if err != nil {
		return nil, err
	}

	items, err := parseFeed(body)
	if err != nil {
		return nil, fmt.Errorf("rss provider: %w", err)
	}

	now := time.Now()
	for i := range items {
		items[i].Score = scoreItem(items[i], now)
	}
	return dedupAndSort(items), nil
}

func (p *Provider) fetch(ctx context.Context, feedURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil, fmt.Errorf("rss provider: %w", err)
	}
	req.Header.Set("User-Agent", "remindertgbot-rss/1.0")
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml, text/xml;q=0.9, */*;q=0.5")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rss fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rss fetch: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBody))
	if err != nil {
		return nil, fmt.Errorf("rss read: %w", err)
	}
	return data, nil
}

// --- feed parsing ---

type rssDoc struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
}

type atomDoc struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	Title     string     `xml:"title"`
	Summary   string     `xml:"summary"`
	Content   string     `xml:"content"`
	Links     []atomLink `xml:"link"`
	Updated   string     `xml:"updated"`
	Published string     `xml:"published"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

// parseFeed tries RSS 2.0 first, then Atom. Unmarshal fails with an error
// when the document's root element doesn't match the struct's XMLName tag,
// so a successful decode also tells us which format we're looking at.
func parseFeed(body []byte) ([]provider.NewsItem, error) {
	var rss rssDoc
	if err := xml.Unmarshal(body, &rss); err == nil {
		items := make([]provider.NewsItem, 0, len(rss.Channel.Items))
		for _, it := range rss.Channel.Items {
			items = append(items, provider.NewsItem{
				Title:       strings.TrimSpace(stripTags(it.Title)),
				Link:        strings.TrimSpace(it.Link),
				Summary:     extractSummary(it.Description),
				PublishedAt: parseDate(it.PubDate),
			})
		}
		return items, nil
	}

	var atom atomDoc
	if err := xml.Unmarshal(body, &atom); err == nil {
		items := make([]provider.NewsItem, 0, len(atom.Entries))
		for _, e := range atom.Entries {
			pub := e.Published
			if pub == "" {
				pub = e.Updated
			}
			summarySrc := e.Summary
			if summarySrc == "" {
				summarySrc = e.Content
			}
			items = append(items, provider.NewsItem{
				Title:       strings.TrimSpace(stripTags(e.Title)),
				Link:        pickAtomLink(e.Links),
				Summary:     extractSummary(summarySrc),
				PublishedAt: parseDate(pub),
			})
		}
		return items, nil
	}

	return nil, fmt.Errorf("unrecognized feed format (expected RSS 2.0 or Atom)")
}

func pickAtomLink(links []atomLink) string {
	for _, l := range links {
		if l.Rel == "" || l.Rel == "alternate" {
			return strings.TrimSpace(l.Href)
		}
	}
	if len(links) > 0 {
		return strings.TrimSpace(links[0].Href)
	}
	return ""
}

var dateLayouts = []string{
	time.RFC1123Z,
	time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	time.RFC3339,
	"2006-01-02 15:04:05",
}

// parseDate tries known RSS/Atom date layouts. A date this provider can't
// parse doesn't drop the item — it only zeroes out the recency contribution
// to its importance score (see scoreItem).
func parseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// --- summary extraction ---

var reHTMLTag = regexp.MustCompile(`<[^>]*>`)

func stripTags(s string) string {
	s = reHTMLTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

const (
	summaryMaxSentences   = 3
	summaryFallbackMaxLen = 400
)

// extractSummary cleans HTML out of a feed's description/summary field and
// keeps the first 2-3 sentences, so the digest carries enough context to be
// useful without reproducing the whole article. Descriptions that have no
// clear sentence boundaries (e.g. a feed that only echoes the title) fall
// back to a hard character limit instead of being dropped.
func extractSummary(raw string) string {
	text := stripTags(raw)
	if text == "" {
		return ""
	}
	sentences := splitSentences(text)
	if len(sentences) == 0 {
		return truncateRunes(text, summaryFallbackMaxLen)
	}
	n := summaryMaxSentences
	if len(sentences) < n {
		n = len(sentences)
	}
	summary := strings.TrimSpace(strings.Join(sentences[:n], " "))
	if summary == "" {
		return truncateRunes(text, summaryFallbackMaxLen)
	}
	return summary
}

var sentenceBoundary = regexp.MustCompile(`[.!?]+(?:["'»)]*)(?:\s+|$)`)

// abbrevSuffixes are common Russian/English abbreviations whose trailing dot
// must not be mistaken for a sentence end.
var abbrevSuffixes = map[string]bool{
	"т.д": true, "т.п": true, "т.е": true, "т.к": true,
	"др": true, "гг": true, "им": true, "см": true, "г": true,
	"проф": true, "акад": true, "стр": true, "рис": true, "табл": true,
	"mr": true, "mrs": true, "ms": true, "dr": true, "prof": true,
	"vs": true, "etc": true, "i.e": true, "e.g": true,
}

// splitSentences breaks text into sentences on '.', '!', '?', skipping
// boundaries that immediately follow a known abbreviation or a single
// initial letter (e.g. "А." in "А. С. Пушкин").
func splitSentences(text string) []string {
	var sentences []string
	start := 0
	for _, m := range sentenceBoundary.FindAllStringIndex(text, -1) {
		end := m[1]
		if end <= start {
			continue
		}
		if endsWithAbbreviation(text[start:m[0]]) {
			continue
		}
		if s := strings.TrimSpace(text[start:end]); s != "" {
			sentences = append(sentences, s)
		}
		start = end
	}
	if rest := strings.TrimSpace(text[start:]); rest != "" {
		sentences = append(sentences, rest)
	}
	return sentences
}

func endsWithAbbreviation(prefix string) bool {
	fields := strings.Fields(prefix)
	if len(fields) == 0 {
		return false
	}
	last := strings.ToLower(strings.Trim(fields[len(fields)-1], ".,;:"))
	if last == "" {
		return false
	}
	if abbrevSuffixes[last] {
		return true
	}
	// A single letter before the dot is almost always an initial, not a
	// sentence end (e.g. "А." / "J.").
	return len([]rune(last)) == 1
}

func truncateRunes(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen]) + "…"
}

// --- scoring ---

// importantKeywords is a fixed, curated heuristic — not an ML/AI judgement of
// importance. It biases the ranking toward breaking/urgent-sounding items.
var importantKeywords = []string{
	"срочно", "экстренно", "важно", "погиб", "убит", "чп",
	"катастроф", "взрыв", "войн", "рекорд", "кризис", "отставк",
	"чрезвычайн", "трагеди", "жертв",
	"breaking", "urgent", "crisis", "dead", "killed", "war", "explosion",
}

// scoreItem combines a keyword-based importance signal with a recency signal
// that decays linearly to zero over one week, so a strong keyword match can
// still outrank a slightly fresher but unremarkable item.
func scoreItem(it provider.NewsItem, now time.Time) float64 {
	text := strings.ToLower(it.Title + " " + it.Summary)
	var score float64
	for _, kw := range importantKeywords {
		if strings.Contains(text, kw) {
			score += 10
		}
	}
	if !it.PublishedAt.IsZero() {
		age := now.Sub(it.PublishedAt)
		if age < 0 {
			age = 0
		}
		const window = 7 * 24 * time.Hour
		if age < window {
			score += 7 * (1 - float64(age)/float64(window))
		}
	}
	return score
}

func dedupAndSort(items []provider.NewsItem) []provider.NewsItem {
	seen := make(map[string]bool, len(items))
	out := make([]provider.NewsItem, 0, len(items))
	for _, it := range items {
		key := it.Link
		if key == "" {
			key = it.Title
		}
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})
	return out
}
