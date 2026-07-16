package rss

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

const sampleRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed</title>
    <item>
      <title>Первая новость</title>
      <link>https://example.com/1</link>
      <description>&lt;p&gt;Это первое предложение. Это второе предложение! А это третье? А четвёртое предложение уже не войдёт в саммари.&lt;/p&gt;</description>
      <pubDate>Mon, 13 Jul 2026 09:00:00 +0300</pubDate>
    </item>
    <item>
      <title>Срочно: важная новость</title>
      <link>https://example.com/2</link>
      <description>Без пунктуации в конце совсем длинный текст который не содержит явных границ предложений и должен быть обрезан по жёсткому лимиту символов а не потерян полностью</description>
      <pubDate>Mon, 13 Jul 2026 08:00:00 +0300</pubDate>
    </item>
    <item>
      <title>Дубликат</title>
      <link>https://example.com/2</link>
      <description></description>
      <pubDate></pubDate>
    </item>
  </channel>
</rss>`

const sampleAtom = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Test Atom Feed</title>
  <entry>
    <title>Atom-новость</title>
    <link rel="alternate" href="https://example.com/atom/1"/>
    <summary>Одно предложение в саммари.</summary>
    <published>2026-07-13T07:30:00Z</published>
  </entry>
</feed>`

func TestParseFeedRSS(t *testing.T) {
	items, err := parseFeed([]byte(sampleRSS))
	if err != nil {
		t.Fatalf("parseFeed: %v", err)
	}
	// Item 3 duplicates item 2's link and should be removed only by
	// dedupAndSort, not by parseFeed itself — parseFeed returns raw items.
	if len(items) != 3 {
		t.Fatalf("expected 3 raw items, got %d", len(items))
	}

	first := items[0]
	if first.Title != "Первая новость" {
		t.Errorf("unexpected title: %q", first.Title)
	}
	if first.Link != "https://example.com/1" {
		t.Errorf("unexpected link: %q", first.Link)
	}
	wantSummary := "Это первое предложение. Это второе предложение! А это третье?"
	if first.Summary != wantSummary {
		t.Errorf("summary = %q, want %q", first.Summary, wantSummary)
	}
	if first.PublishedAt.IsZero() {
		t.Error("expected non-zero PublishedAt")
	}

	second := items[1]
	if len([]rune(second.Summary)) > summaryFallbackMaxLen+1 {
		t.Errorf("fallback summary too long: %d runes", len([]rune(second.Summary)))
	}
	if second.Summary == "" {
		t.Error("expected non-empty fallback summary for punctuation-less description")
	}

	third := items[2]
	if third.Summary != "" {
		t.Errorf("expected empty summary for empty description, got %q", third.Summary)
	}
	if !third.PublishedAt.IsZero() {
		t.Error("expected zero time for missing pubDate")
	}
}

func TestParseFeedAtom(t *testing.T) {
	items, err := parseFeed([]byte(sampleAtom))
	if err != nil {
		t.Fatalf("parseFeed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	it := items[0]
	if it.Title != "Atom-новость" {
		t.Errorf("unexpected title: %q", it.Title)
	}
	if it.Link != "https://example.com/atom/1" {
		t.Errorf("unexpected link: %q", it.Link)
	}
	if it.Summary != "Одно предложение в саммари." {
		t.Errorf("unexpected summary: %q", it.Summary)
	}
}

func TestParseFeedInvalidXML(t *testing.T) {
	if _, err := parseFeed([]byte("not xml at all")); err == nil {
		t.Fatal("expected error for invalid XML")
	}
}

func TestDedupAndSortByScore(t *testing.T) {
	items, err := parseFeed([]byte(sampleRSS))
	if err != nil {
		t.Fatalf("parseFeed: %v", err)
	}
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	for i := range items {
		items[i].Score = scoreItem(items[i], now)
	}
	out := dedupAndSort(items)

	if len(out) != 2 {
		t.Fatalf("expected 2 items after dedup, got %d", len(out))
	}
	// "Срочно: важная новость" matches a keyword and should outrank the
	// plain first item despite being slightly older.
	if !strings.Contains(out[0].Title, "Срочно") {
		t.Errorf("expected keyword-matching item first, got %q", out[0].Title)
	}
	if out[0].Score <= out[1].Score {
		t.Errorf("expected descending score order: %v then %v", out[0].Score, out[1].Score)
	}
}

func TestExtractSummaryEmptyInput(t *testing.T) {
	if got := extractSummary(""); got != "" {
		t.Errorf("expected empty summary, got %q", got)
	}
	if got := extractSummary("   <p></p>  "); got != "" {
		t.Errorf("expected empty summary for whitespace/markup-only input, got %q", got)
	}
}

func TestExtractSummaryStopsAtThreeSentences(t *testing.T) {
	text := "Одно. Два. Три. Четыре. Пять."
	got := extractSummary(text)
	want := "Одно. Два. Три."
	if got != want {
		t.Errorf("extractSummary = %q, want %q", got, want)
	}
}

func TestExtractSummaryHandlesAbbreviations(t *testing.T) {
	text := "Профессор А. С. Иванов и др. представили доклад. Это второе предложение."
	got := extractSummary(text)
	if !strings.HasSuffix(got, "Это второе предложение.") {
		t.Errorf("abbreviation caused a false sentence split: %q", got)
	}
}

// TestFetchMergesMultipleFeeds verifies that a comma-separated url param
// fetches every feed and combines their items into one deduped, scored
// list — the "one shared top-N" behavior for a multi-feed digest.
func TestFetchMergesMultipleFeeds(t *testing.T) {
	p := &Provider{log: slog.Default()}
	p.fetchOne = func(_ context.Context, feedURL string) ([]provider.NewsItem, error) {
		switch feedURL {
		case "https://a.example/rss":
			return []provider.NewsItem{{Title: "From A", Link: "https://a.example/1"}}, nil
		case "https://b.example/rss":
			return []provider.NewsItem{{Title: "From B", Link: "https://b.example/1"}}, nil
		default:
			t.Fatalf("unexpected feed URL: %q", feedURL)
			return nil, nil
		}
	}

	items, err := p.Fetch(context.Background(), provider.Query{
		Params: map[string]string{"url": "https://a.example/rss, https://b.example/rss"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("expected items from both feeds, got %d: %+v", len(items), items)
	}
}

// TestFetchToleratesOneFeedFailing verifies that a digest with several
// feeds still succeeds with items from the feeds that worked, when one of
// them fails — a single blocked/dead feed must not kill the whole digest.
func TestFetchToleratesOneFeedFailing(t *testing.T) {
	p := &Provider{log: slog.Default()}
	p.fetchOne = func(_ context.Context, feedURL string) ([]provider.NewsItem, error) {
		if feedURL == "https://dead.example/rss" {
			return nil, errors.New("connection reset by peer")
		}
		return []provider.NewsItem{{Title: "OK", Link: "https://ok.example/1"}}, nil
	}

	items, err := p.Fetch(context.Background(), provider.Query{
		Params: map[string]string{"url": "https://dead.example/rss,https://ok.example/rss"},
	})
	if err != nil {
		t.Fatalf("expected partial success, got error: %v", err)
	}
	if len(items) != 1 || items[0].Title != "OK" {
		t.Fatalf("expected only the working feed's item, got %+v", items)
	}
}

// TestFetchErrorsWhenAllFeedsFail ensures a digest whose every feed fails
// surfaces an error instead of silently returning an empty digest.
func TestFetchErrorsWhenAllFeedsFail(t *testing.T) {
	p := &Provider{log: slog.Default()}
	p.fetchOne = func(context.Context, string) ([]provider.NewsItem, error) {
		return nil, errors.New("boom")
	}

	_, err := p.Fetch(context.Background(), provider.Query{
		Params: map[string]string{"url": "https://a.example/rss,https://b.example/rss"},
	})
	if err == nil {
		t.Fatal("expected an error when every feed fails")
	}
}

func TestFetchRejectsTooManyURLs(t *testing.T) {
	p := &Provider{log: slog.Default()}
	p.fetchOne = func(context.Context, string) ([]provider.NewsItem, error) {
		return []provider.NewsItem{{Title: "x", Link: "https://x.example/1"}}, nil
	}

	urls := make([]string, maxFeedURLs+1)
	for i := range urls {
		urls[i] = "https://example.com/feed"
	}
	_, err := p.Fetch(context.Background(), provider.Query{
		Params: map[string]string{"url": strings.Join(urls, ",")},
	})
	if err == nil {
		t.Fatal("expected an error for more than maxFeedURLs feeds")
	}
}

func TestFetchRequiresAtLeastOneURL(t *testing.T) {
	p := &Provider{log: slog.Default()}
	if _, err := p.Fetch(context.Background(), provider.Query{Params: map[string]string{"url": "  , ,"}}); err == nil {
		t.Fatal("expected an error for an empty url param")
	}
}

func TestScoreItemRecencyDecay(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	fresh := scoreItem(provider.NewsItem{PublishedAt: now}, now)
	old := scoreItem(provider.NewsItem{PublishedAt: now.Add(-30 * 24 * time.Hour)}, now)
	if fresh <= old {
		t.Errorf("expected fresher item to score higher: fresh=%v old=%v", fresh, old)
	}
}
