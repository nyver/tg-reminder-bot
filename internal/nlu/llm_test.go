package nlu

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestOpenRouterParser_fallbackOn429 verifies that a 429 from the primary model
// causes an immediate switch to the fallback model (no delay, no retry of primary).
func TestOpenRouterParser_fallbackOn429(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32

	const primaryModel = "primary/model"
	const fallbackModel = "fallback/model"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := requests.Add(1)
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if idx == 1 {
			// Primary model gets rate-limited.
			if req.Model != primaryModel {
				t.Errorf("request 1 model = %q, want %q", req.Model, primaryModel)
			}
			http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
			return
		}
		// Second request must use the fallback model.
		if req.Model != fallbackModel {
			t.Errorf("request 2 model = %q, want %q", req.Model, fallbackModel)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"kind\":\"absolute\",\"message\":\"test\",\"confidence\":0.95}"}}]}`))
	}))
	defer server.Close()

	parser, err := NewConfiguredLLMParser("openrouter", "test-key", primaryModel, server.URL, []string{fallbackModel}, 0, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	result, err := parser.Parse(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Confidence != 0.95 || result.Spec.Message != "test" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if n := requests.Load(); n != 2 {
		t.Fatalf("total requests = %d, want 2", n)
	}
}

// TestOpenRouterParser_fallbackOn404 verifies that a 404 from the primary
// model (e.g. a free model slug that OpenRouter has retired) causes an
// immediate switch to the fallback model, the same as a 429 does.
func TestOpenRouterParser_fallbackOn404(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32

	const primaryModel = "primary/model:free"
	const fallbackModel = "fallback/model"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := requests.Add(1)
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if idx == 1 {
			if req.Model != primaryModel {
				t.Errorf("request 1 model = %q, want %q", req.Model, primaryModel)
			}
			http.Error(w, `{"error":{"message":"This model is unavailable for free","code":404}}`, http.StatusNotFound)
			return
		}
		if req.Model != fallbackModel {
			t.Errorf("request 2 model = %q, want %q", req.Model, fallbackModel)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"kind\":\"absolute\",\"message\":\"test\",\"confidence\":0.95}"}}]}`))
	}))
	defer server.Close()

	parser, err := NewConfiguredLLMParser("openrouter", "test-key", primaryModel, server.URL, []string{fallbackModel}, 0, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	result, err := parser.Parse(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Confidence != 0.95 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if n := requests.Load(); n != 2 {
		t.Fatalf("total requests = %d, want 2", n)
	}
}

func TestOpenRouterParserFallbackOnEmptyContent(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32

	const primaryModel = "primary/model"
	const fallbackModel = "fallback/model"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := requests.Add(1)
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if idx == 1 {
			if req.Model != primaryModel {
				t.Errorf("request 1 model = %q, want %q", req.Model, primaryModel)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""}}]}`))
			return
		}
		if req.Model != fallbackModel {
			t.Errorf("request 2 model = %q, want %q", req.Model, fallbackModel)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"kind\":\"absolute\",\"message\":\"test\",\"confidence\":0.95}"}}]}`))
	}))
	defer server.Close()

	parser, err := NewConfiguredLLMParser("openrouter", "test-key", primaryModel, server.URL, []string{fallbackModel}, 0, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	result, err := parser.Parse(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Confidence != 0.95 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if n := requests.Load(); n != 2 {
		t.Fatalf("total requests = %d, want 2", n)
	}
}

func TestOpenRouterParserLogsInitialModelAndFallback(t *testing.T) {
	var requests atomic.Int32

	const primaryModel = "primary/model"
	const fallbackModel = "fallback/model"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := requests.Add(1)
		if idx == 1 {
			http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"kind\":\"absolute\",\"message\":\"test\",\"confidence\":0.95}"}}]}`))
	}))
	defer server.Close()

	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	parser, err := NewConfiguredLLMParser(
		"openrouter", "test-key", primaryModel, server.URL,
		[]string{fallbackModel}, 0, 0, time.UTC, log,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.Parse(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}

	out := logs.String()
	for _, want := range []string{
		`"msg":"llm request"`,
		`"component":"nlu_parser"`,
		`"provider":"openrouter"`,
		`"model":"primary/model"`,
		`"fallback":false`,
		`"msg":"llm fallback"`,
		`"failed_model":"primary/model"`,
		`"next_model":"fallback/model"`,
		`"model":"fallback/model"`,
		`"fallback":true`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("logs missing %s in:\n%s", want, out)
		}
	}
}

// TestOpenRouterParser_allModelsRateLimited verifies that if every model returns
// 429, the error is propagated to the caller.
func TestOpenRouterParser_allModelsRateLimited(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
	}))
	defer server.Close()

	parser, err := NewConfiguredLLMParser(
		"openrouter", "test-key", "m1/free", server.URL,
		[]string{"m2/free", "m3/free"}, 0, 0, time.UTC,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.Parse(context.Background(), "test"); err == nil {
		t.Fatal("expected error when all models are rate-limited")
	}
}

func TestConfiguredLLMParserRejectsUnknownProvider(t *testing.T) {
	t.Parallel()
	if _, err := NewConfiguredLLMParser("unknown", "", "", "", nil, 0, 0, time.UTC); err == nil {
		t.Fatal("expected an error")
	}
}
