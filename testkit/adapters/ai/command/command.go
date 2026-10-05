// Package command runs any external program as the model: the rules and the
// task are written to its stdin, the answer is read from its stdout (for
// example `claude -p`, `ollama run <model>`, an in-house gateway client).
package command

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/tuannm99/testkit/testkit/core/ai"
	"github.com/tuannm99/testkit/testkit/core/config"
)

type Provider struct {
	argv    []string
	model   string
	timeout time.Duration
}

func New(cfg *config.AI) (*Provider, error) {
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("ai: provider command needs ai.command (argv)")
	}
	p := &Provider{argv: cfg.Command, model: cfg.Model, timeout: 10 * time.Minute}
	if p.model == "" {
		p.model = cfg.Command[0]
	}
	if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
		p.timeout = d
	}
	return p, nil
}

func (p *Provider) Name() string  { return "command" }
func (p *Provider) Model() string { return p.model }

func (p *Provider) Complete(ctx context.Context, r ai.Request) (ai.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.argv[0], p.argv[1:]...)
	cmd.Stdin = strings.NewReader(r.System + "\n\n---\n\n" + r.Prompt)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return ai.Response{}, fmt.Errorf("ai command %s: %v: %.500s", p.argv[0], err, errb.String())
	}
	return ai.Response{Text: out.String(), Model: p.model, StopReason: "end"}, nil
}

var _ ai.Provider = (*Provider)(nil)
