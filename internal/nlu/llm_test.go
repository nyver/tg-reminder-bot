package nlu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOpenRouterParser(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if request.Model != "test/model" {
			t.Errorf("model = %q", request.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"kind\":\"absolute\",\"message\":\"test\",\"confidence\":0.95}"}}]}`))
	}))
	defer server.Close()

	parser, err := NewConfiguredLLMParser("openrouter", "test-key", "test/model", server.URL, time.UTC)
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
}

func TestConfiguredLLMParserRejectsUnknownProvider(t *testing.T) {
	t.Parallel()
	if _, err := NewConfiguredLLMParser("unknown", "", "", "", time.UTC); err == nil {
		t.Fatal("expected an error")
	}
}
