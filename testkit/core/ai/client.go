package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Request is what a task asks of a model.
type Request struct {
	Task      string // triage | summary | draft
	System    string
	Prompt    string
	MaxTokens int
}

// Response is the model's answer.
type Response struct {
	Text         string
	Model        string
	StopReason   string
	InputTokens  int64
	OutputTokens int64
}

// Provider is any model: Claude, an OpenAI-compatible endpoint, an external
// command, or a person pasting an answer.
type Provider interface {
	Name() string
	Model() string
	Complete(ctx context.Context, r Request) (Response, error)
}

// ErrManual is returned by the "none" provider: the prompt was written for
// manual use; rerun with --response <file> once you have an answer.
type ErrManual struct{ PromptFile string }

func (e ErrManual) Error() string {
	return "no AI provider configured (ai.provider: none): the redacted prompt is in " + e.PromptFile +
		"; give it to any model and rerun with --response <answer file>"
}

// Client applies the data rules around a provider: every request is
// redacted, checked by Guard and recorded (redacted payload, sha256,
// redaction counts, answer) under AuditDir before anything is sent.
type Client struct {
	Provider Provider
	Secrets  map[string]string
	AuditDir string // absolute
	// Response, when set, is used instead of calling the provider (an
	// answer obtained by hand from any model).
	Response string
	n        int
}

// Record is one audited exchange.
type Record struct {
	Task          string         `json:"task"`
	Provider      string         `json:"provider"`
	Model         string         `json:"model"`
	PromptVersion string         `json:"prompt_version"`
	SentAt        time.Time      `json:"sent_at"`
	PayloadSHA256 string         `json:"payload_sha256"`
	Redactions    map[string]int `json:"redactions"`
	System        string         `json:"system"`
	Prompt        string         `json:"prompt"` // exactly what left the machine (redacted)
	Answer        string         `json:"answer,omitempty"`
	StopReason    string         `json:"stop_reason,omitempty"`
	InputTokens   int64          `json:"input_tokens,omitempty"`
	OutputTokens  int64          `json:"output_tokens,omitempty"`
	Manual        bool           `json:"manual_answer,omitempty"`
	Error         string         `json:"error,omitempty"`
}

// Prepare redacts and checks a request; it is what `ai context` shows. The
// system text is TestKit's own fixed rules (embedded prompts): it is checked
// by Guard but not rewritten.
func (c *Client) Prepare(r Request) (Request, map[string]int, error) {
	red := NewRedactor(c.Secrets)
	r.Prompt = red.Text(r.Prompt)
	if err := Guard(r.System+"\n"+r.Prompt, c.Secrets); err != nil {
		return r, red.Counts(), err
	}
	return r, red.Counts(), nil
}

// Do sends one request (redacted) and returns the answer.
func (c *Client) Do(ctx context.Context, r Request) (Response, error) {
	r, counts, err := c.Prepare(r)
	if err != nil {
		return Response{}, err
	}
	c.n++
	sum := sha256.Sum256([]byte(r.System + "\x00" + r.Prompt))
	rec := Record{Task: r.Task, Provider: c.Provider.Name(), Model: c.Provider.Model(), PromptVersion: PromptVersion,
		SentAt: time.Now().UTC(), PayloadSHA256: hex.EncodeToString(sum[:]), Redactions: counts, System: r.System, Prompt: r.Prompt}
	file := filepath.Join(c.AuditDir, fmt.Sprintf("%02d-%s.json", c.n, r.Task))
	var resp Response
	switch {
	case c.Response != "":
		resp, rec.Manual = Response{Text: c.Response, Model: "manual"}, true
	default:
		if err := c.write(file, rec); err != nil { // recorded before it is sent
			return Response{}, err
		}
		resp, err = c.Provider.Complete(ctx, r)
		if me, ok := err.(ErrManual); ok {
			me.PromptFile = file
			err = me
		}
	}
	rec.Answer, rec.StopReason, rec.InputTokens, rec.OutputTokens = resp.Text, resp.StopReason, resp.InputTokens, resp.OutputTokens
	if resp.Model != "" && !rec.Manual {
		rec.Model = resp.Model
	}
	if err != nil {
		rec.Error = err.Error()
	}
	if werr := c.write(file, rec); werr != nil && err == nil {
		err = werr
	}
	return resp, err
}

func (c *Client) write(file string, rec Record) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, raw, 0o644)
}

// extractBlock returns the content of the first ```lang fence, or the text.
func extractBlock(text, lang string) string {
	t := strings.TrimSpace(text)
	for _, open := range []string{"```" + lang, "```"} {
		if i := strings.Index(t, open); i >= 0 {
			rest := t[i+len(open):]
			if j := strings.Index(rest, "```"); j >= 0 {
				return strings.TrimSpace(rest[:j])
			}
		}
	}
	return t
}

// Manual is the "none" provider: nothing is sent; the redacted prompt is
// left in the audit file for a person to use with any model.
type Manual struct{}

func (Manual) Name() string  { return "none" }
func (Manual) Model() string { return "manual" }
func (Manual) Complete(context.Context, Request) (Response, error) {
	return Response{}, ErrManual{}
}
