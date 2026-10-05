// Package anthropic is the Claude provider, through the official Go SDK.
package anthropic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/tuannm99/testkit/testkit/core/ai"
	"github.com/tuannm99/testkit/testkit/core/config"
)

// DefaultModel is used when ai.model is empty.
const DefaultModel = "claude-opus-5-5"

type Provider struct {
	client    sdk.Client
	model     string
	effort    string
	maxTokens int
}

// New builds the provider. Credentials: the env var named by ai.api_key_env
// when set, otherwise the SDK's own resolution (ANTHROPIC_API_KEY, profiles).
func New(cfg *config.AI) (*Provider, error) {
	var opts []option.RequestOption
	if cfg.APIKeyEnv != "" {
		key := os.Getenv(cfg.APIKeyEnv)
		if key == "" {
			return nil, fmt.Errorf("ai: $%s is empty", cfg.APIKeyEnv)
		}
		opts = append(opts, option.WithAPIKey(key))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	timeout := 5 * time.Minute
	if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
		timeout = d
	}
	opts = append(opts, option.WithRequestTimeout(timeout), option.WithMaxRetries(2))
	p := &Provider{client: sdk.NewClient(opts...), model: cfg.Model, effort: cfg.Effort, maxTokens: cfg.MaxTokens}
	if p.model == "" {
		p.model = DefaultModel
	}
	if p.effort == "" {
		p.effort = "high"
	}
	return p, nil
}

func (p *Provider) Name() string  { return "anthropic" }
func (p *Provider) Model() string { return p.model }

// Complete sends one message. A policy refusal is retried by the server on a
// fallback model (fallbacks: "default"); a refusal that still comes back is
// returned as an error, never as an answer.
func (p *Provider) Complete(ctx context.Context, r ai.Request) (ai.Response, error) {
	maxTokens := int64(r.MaxTokens)
	if p.maxTokens > 0 {
		maxTokens = int64(p.maxTokens)
	}
	if maxTokens <= 0 {
		maxTokens = 16000
	}
	params := sdk.BetaMessageNewParams{
		Model:        sdk.Model(p.model),
		MaxTokens:    maxTokens,
		System:       []sdk.BetaTextBlockParam{{Text: r.System}},
		Messages:     []sdk.BetaMessageParam{sdk.NewBetaUserMessage(sdk.NewBetaTextBlock(r.Prompt))},
		OutputConfig: sdk.BetaOutputConfigParam{Effort: sdk.BetaOutputConfigEffort(p.effort)},
		Fallbacks:    sdk.BetaFallbacksParamOfDefault(),
		Betas:        []sdk.AnthropicBeta{sdk.AnthropicBetaServerSideFallback2026_07_01},
	}
	msg, err := p.client.Beta.Messages.New(ctx, params)
	if err != nil {
		var apiErr *sdk.Error
		if errors.As(err, &apiErr) {
			return ai.Response{}, fmt.Errorf("anthropic: HTTP %d: %s", apiErr.StatusCode, strings.TrimSpace(apiErr.Error()))
		}
		return ai.Response{}, fmt.Errorf("anthropic: %w", err)
	}
	resp := ai.Response{Model: string(msg.Model), StopReason: string(msg.StopReason),
		InputTokens: msg.Usage.InputTokens, OutputTokens: msg.Usage.OutputTokens}
	if msg.StopReason == sdk.BetaStopReasonRefusal {
		return resp, fmt.Errorf("anthropic: request declined (%s): %s", msg.StopDetails.Category, msg.StopDetails.Explanation)
	}
	var text strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(sdk.BetaTextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	resp.Text = text.String()
	if msg.StopReason == sdk.BetaStopReasonMaxTokens {
		return resp, fmt.Errorf("anthropic: answer cut at max_tokens (%d); raise ai.max_tokens", maxTokens)
	}
	return resp, nil
}

var _ ai.Provider = (*Provider)(nil)
