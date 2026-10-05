package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tuannm99/testkit/testkit/core/ai"
	"github.com/tuannm99/testkit/testkit/core/config"
)

// A local stand-in of the Messages API: checks the request and answers in the
// documented response shape.
func server(t *testing.T, stop string, got *map[string]any, hdr *http.Header) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, got)
		*hdr = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
			"content":     []map[string]any{{"type": "text", "text": `{"items":[]}`}},
			"stop_reason": stop, "usage": map[string]any{"input_tokens": 120, "output_tokens": 7}}
		if stop == "refusal" {
			resp["content"] = []map[string]any{}
			resp["stop_details"] = map[string]any{"type": "refusal", "category": "cyber", "explanation": "declined"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestComplete(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	srv := server(t, "end_turn", &body, &hdr)
	defer srv.Close()
	t.Setenv("TK_TEST_KEY", "sk-test")
	p, err := New(&config.AI{BaseURL: srv.URL, APIKeyEnv: "TK_TEST_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Complete(context.Background(), ai.Request{System: "rules", Prompt: "task", MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `{"items":[]}` || resp.InputTokens != 120 || p.Model() != DefaultModel {
		t.Fatalf("%+v", resp)
	}
	if body["model"] != "claude-opus-5-5" || body["max_tokens"].(float64) != 1000 || body["fallbacks"] != "default" ||
		body["output_config"].(map[string]any)["effort"] != "high" || hdr.Get("X-Api-Key") != "sk-test" ||
		!strings.Contains(hdr.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") {
		t.Fatalf("request: %v %v", body, hdr)
	}
	sys := body["system"].([]any)[0].(map[string]any)["text"]
	if sys != "rules" {
		t.Fatalf("system %v", sys)
	}
}

func TestRefusalIsAnError(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	srv := server(t, "refusal", &body, &hdr)
	defer srv.Close()
	t.Setenv("TK_TEST_KEY", "sk-test")
	p, _ := New(&config.AI{BaseURL: srv.URL, APIKeyEnv: "TK_TEST_KEY"})
	if _, err := p.Complete(context.Background(), ai.Request{System: "s", Prompt: "p"}); err == nil || !strings.Contains(err.Error(), "declined (cyber)") {
		t.Fatalf("refusal not reported: %v", err)
	}
}
