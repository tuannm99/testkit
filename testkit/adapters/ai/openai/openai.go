// Package openai is the provider for any model served behind an
// OpenAI-compatible /chat/completions endpoint (hosted or local).
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tuannm99/testkit/testkit/core/ai"
	"github.com/tuannm99/testkit/testkit/core/config"
)

type Provider struct {
	base, model, key string
	maxTokens        int
	http             *http.Client
}

func New(cfg *config.AI) (*Provider, error) {
	if cfg.BaseURL == "" || cfg.Model == "" {
		return nil, fmt.Errorf("ai: openai-compatible needs ai.base_url and ai.model")
	}
	p := &Provider{base: strings.TrimRight(cfg.BaseURL, "/"), model: cfg.Model, maxTokens: cfg.MaxTokens, http: &http.Client{Timeout: 5 * time.Minute}}
	if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
		p.http.Timeout = d
	}
	if cfg.APIKeyEnv != "" {
		if p.key = os.Getenv(cfg.APIKeyEnv); p.key == "" {
			return nil, fmt.Errorf("ai: $%s is empty", cfg.APIKeyEnv)
		}
	}
	return p, nil
}

func (p *Provider) Name() string  { return "openai-compatible" }
func (p *Provider) Model() string { return p.model }

func (p *Provider) Complete(ctx context.Context, r ai.Request) (ai.Response, error) {
	max := r.MaxTokens
	if p.maxTokens > 0 {
		max = p.maxTokens
	}
	body, _ := json.Marshal(map[string]any{"model": p.model, "max_tokens": max, "messages": []map[string]string{
		{"role": "system", "content": r.System}, {"role": "user", "content": r.Prompt}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ai.Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.key != "" {
		req.Header.Set("Authorization", "Bearer "+p.key)
	}
	res, err := p.http.Do(req)
	if err != nil {
		return ai.Response{}, fmt.Errorf("openai-compatible: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode >= 300 {
		return ai.Response{}, fmt.Errorf("openai-compatible: HTTP %d: %.300s", res.StatusCode, raw)
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return ai.Response{}, fmt.Errorf("openai-compatible: unexpected answer: %.300s", raw)
	}
	resp := ai.Response{Text: out.Choices[0].Message.Content, Model: out.Model, StopReason: out.Choices[0].FinishReason,
		InputTokens: out.Usage.Prompt, OutputTokens: out.Usage.Completion}
	if resp.StopReason == "length" {
		return resp, fmt.Errorf("openai-compatible: answer cut at max_tokens (%d)", max)
	}
	return resp, nil
}

var _ ai.Provider = (*Provider)(nil)
