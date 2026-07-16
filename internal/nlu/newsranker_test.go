package nlu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestNewsRankerAcceptsItemsObject(t *testing.T) {
	ranker := &NewsRanker{complete: func(context.Context, string) (string, error) {
		return `{"items":[{"link":"https://example.com/1","title":"Заголовок","summary":"Саммари."}]}`, nil
	}}
	candidates := []provider.NewsItem{
		{Title: "Original", Link: "https://example.com/1", Summary: "Old."},
	}

	out, err := ranker.Rank(context.Background(), candidates, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Title != "Заголовок" || out[0].Summary != "Саммари." {
		t.Fatalf("out = %+v", out)
	}
}

func TestNewsRankerOpenRouterFallbackOnMalformedJSON(t *testing.T) {
	var requests atomic.Int32

	const primaryModel = "bad/model"
	const fallbackModel = "good/model"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := requests.Add(1)
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch idx {
		case 1:
			if req.Model != primaryModel {
				t.Errorf("request 1 model = %q, want %q", req.Model, primaryModel)
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"items\":[{\"link\":\"https://example.com/1\",\"summary\":\"broken\"}"}}]}`))
		case 2:
			if req.Model != fallbackModel {
				t.Errorf("request 2 model = %q, want %q", req.Model, fallbackModel)
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"items\":[{\"link\":\"https://example.com/1\",\"summary\":\"ok\"}]}"}}]}`))
		default:
			t.Errorf("unexpected request %d", idx)
		}
	}))
	defer server.Close()

	ranker, err := NewConfiguredNewsRanker("openrouter", "test-key", primaryModel, server.URL, []string{fallbackModel}, time.Second, 1024)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ranker.Rank(context.Background(), []provider.NewsItem{{Title: "1", Link: "https://example.com/1"}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Summary != "ok" {
		t.Fatalf("out = %+v, want fallback summary", out)
	}
	if n := requests.Load(); n != 2 {
		t.Fatalf("total requests = %d, want 2", n)
	}
}

// TestNewsRankerTranslatesTitle verifies that a translated title returned by
// the model overrides the candidate's original (e.g. English) title, the
// same way a fresh summary does.
func TestNewsRankerTranslatesTitle(t *testing.T) {
	ranker := &NewsRanker{complete: func(context.Context, string) (string, error) {
		return `[{"link":"https://example.com/1","title":"Заголовок на русском","summary":"Саммари на русском."}]`, nil
	}}
	candidates := []provider.NewsItem{
		{Title: "Original English Title", Link: "https://example.com/1", Summary: "Original English summary."},
	}

	out, err := ranker.Rank(context.Background(), candidates, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	if out[0].Title != "Заголовок на русском" {
		t.Fatalf("title = %q, want translated title", out[0].Title)
	}
	if out[0].Summary != "Саммари на русском." {
		t.Fatalf("summary = %q, want translated summary", out[0].Summary)
	}
}

// TestNewsRankerKeepsOriginalTitleWhenModelOmitsIt ensures a missing/empty
// title in the model's response doesn't blank out the original.
func TestNewsRankerKeepsOriginalTitleWhenModelOmitsIt(t *testing.T) {
	ranker := &NewsRanker{complete: func(context.Context, string) (string, error) {
		return `[{"link":"https://example.com/1","summary":"Саммари."}]`, nil
	}}
	candidates := []provider.NewsItem{
		{Title: "Original Title", Link: "https://example.com/1"},
	}

	out, err := ranker.Rank(context.Background(), candidates, 1)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Title != "Original Title" {
		t.Fatalf("title = %q, want original title preserved", out[0].Title)
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

// TestNewsRankerToleratesRawControlCharsInStrings verifies that a raw
// (unescaped) tab or newline emitted by the model inside a JSON string
// value — technically invalid JSON, but a common LLM mistake — is repaired
// rather than treated as a parse failure that falls back to the heuristic.
func TestNewsRankerToleratesRawControlCharsInStrings(t *testing.T) {
	raw := "[{\"link\":\"https://example.com/1\",\"summary\":\"Первое предложение.\tВторое предложение.\n\"}]"
	ranker := &NewsRanker{complete: func(context.Context, string) (string, error) {
		return raw, nil
	}}
	candidates := []provider.NewsItem{{Title: "1", Link: "https://example.com/1"}}

	out, err := ranker.Rank(context.Background(), candidates, 1)
	if err != nil {
		t.Fatalf("expected raw control chars in the response to be tolerated, got: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	if !strings.Contains(out[0].Summary, "Первое предложение.") || !strings.Contains(out[0].Summary, "Второе предложение.") {
		t.Fatalf("summary = %q, want both sentences preserved", out[0].Summary)
	}
}

func TestSanitizeJSONStringsEscapesControlCharsOnlyInsideStrings(t *testing.T) {
	// A raw newline used as pretty-print whitespace between array elements
	// (outside any string) is already legal JSON and must be left alone.
	in := "[\n\t{\"a\":\"line1\tline2\"}\n]"
	out := sanitizeJSONStrings(in)

	var decoded []map[string]string
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("sanitized JSON still invalid: %v (sanitized: %q)", err, out)
	}
	if decoded[0]["a"] != "line1\tline2" {
		t.Fatalf("a = %q, want tab preserved inside the value", decoded[0]["a"])
	}
}

// TestSanitizeJSONStringsDoesNotDoubleEscape ensures an already-valid "\n"
// escape sequence in the input passes through unchanged.
func TestSanitizeJSONStringsDoesNotDoubleEscape(t *testing.T) {
	in := `{"a":"line1\nline2"}`
	out := sanitizeJSONStrings(in)
	if out != in {
		t.Fatalf("sanitizeJSONStrings altered already-valid input: got %q, want %q", out, in)
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

func TestNewsRankerErrorsOnEmptyResponse(t *testing.T) {
	ranker := &NewsRanker{complete: func(context.Context, string) (string, error) {
		return "  \n\t", nil
	}}
	candidates := []provider.NewsItem{{Title: "1", Link: "https://example.com/1"}}

	_, err := ranker.Rank(context.Background(), candidates, 5)
	if err == nil {
		t.Fatal("expected error for empty response")
	}
	if !strings.Contains(err.Error(), "empty model response") {
		t.Fatalf("err = %v, want empty model response", err)
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
