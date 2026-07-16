package nlu

import (
	"context"
	"testing"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

func TestNewsRankerPicksAndSummarizesByLink(t *testing.T) {
	ranker := &NewsRanker{complete: func(context.Context, string) (string, error) {
		return `[{"link":"https://example.com/2","summary":"Свежее саммари 2."},` +
			`{"link":"https://example.com/1","summary":"Свежее саммари 1."}]`, nil
	}}

	candidates := []provider.NewsItem{
		{Title: "Новость 1", Link: "https://example.com/1", Summary: "Старое саммари 1."},
		{Title: "Новость 2", Link: "https://example.com/2", Summary: "Старое саммари 2."},
	}

	out, err := ranker.Rank(context.Background(), candidates, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2", len(out))
	}
	// Order follows the model's response, not the input order.
	if out[0].Link != "https://example.com/2" || out[0].Summary != "Свежее саммари 2." {
		t.Fatalf("out[0] = %+v", out[0])
	}
	if out[1].Link != "https://example.com/1" || out[1].Summary != "Свежее саммари 1." {
		t.Fatalf("out[1] = %+v", out[1])
	}
}

func TestNewsRankerRespectsTopN(t *testing.T) {
	ranker := &NewsRanker{complete: func(context.Context, string) (string, error) {
		return `[{"link":"https://example.com/1","summary":"a"},` +
			`{"link":"https://example.com/2","summary":"b"},` +
			`{"link":"https://example.com/3","summary":"c"}]`, nil
	}}
	candidates := []provider.NewsItem{
		{Title: "1", Link: "https://example.com/1"},
		{Title: "2", Link: "https://example.com/2"},
		{Title: "3", Link: "https://example.com/3"},
	}

	out, err := ranker.Rank(context.Background(), candidates, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2 (topN)", len(out))
	}
}

// TestNewsRankerSkipsHallucinatedLinks verifies that a link the model
// returns which doesn't match any candidate is dropped rather than guessed
// at — the digest must never show an item that wasn't actually in the feed.
func TestNewsRankerSkipsHallucinatedLinks(t *testing.T) {
	ranker := &NewsRanker{complete: func(context.Context, string) (string, error) {
		return `[{"link":"https://example.com/does-not-exist","summary":"a"},` +
			`{"link":"https://example.com/1","summary":"real"}]`, nil
	}}
	candidates := []provider.NewsItem{
		{Title: "1", Link: "https://example.com/1"},
	}

	out, err := ranker.Rank(context.Background(), candidates, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Link != "https://example.com/1" {
		t.Fatalf("out = %+v, want only the real link", out)
	}
}

func TestNewsRankerErrorsOnMalformedJSON(t *testing.T) {
	ranker := &NewsRanker{complete: func(context.Context, string) (string, error) {
		return "not json", nil
	}}
	candidates := []provider.NewsItem{{Title: "1", Link: "https://example.com/1"}}

	if _, err := ranker.Rank(context.Background(), candidates, 5); err == nil {
		t.Fatal("expected error for malformed JSON response")
	}
}

// TestNewsRankerErrorsWhenAllLinksHallucinated ensures a response that
// matches no real candidate surfaces as an error (so the caller falls back
// to the heuristic) instead of silently returning an empty digest.
func TestNewsRankerErrorsWhenAllLinksHallucinated(t *testing.T) {
	ranker := &NewsRanker{complete: func(context.Context, string) (string, error) {
		return `[{"link":"https://example.com/nope","summary":"a"}]`, nil
	}}
	candidates := []provider.NewsItem{{Title: "1", Link: "https://example.com/1"}}

	if _, err := ranker.Rank(context.Background(), candidates, 5); err == nil {
		t.Fatal("expected error when no candidates matched")
	}
}

func TestConfiguredNewsRankerRejectsUnknownProvider(t *testing.T) {
	if _, err := NewConfiguredNewsRanker("unknown", "", "", "", nil, 0, 0); err == nil {
		t.Fatal("expected an error")
	}
}
