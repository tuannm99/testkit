// Package mail reads the mails captured by Mailpit ("mail" connector).
// Isolation between executions relies on recipient addresses containing the
// namespace (e.g. cust+{{ .ns }}@shop.test); checks always filter by recipient.
package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/tuannm99/testkit/testkit/core/kit"
)

type Connector struct {
	env  *kit.Env
	base string
	http *http.Client
}

func New() kit.Connector { return &Connector{http: &http.Client{Timeout: 15 * time.Second}} }

func (c *Connector) Name() string            { return "mail" }
func (c *Connector) CheckPrefixes() []string { return []string{"mail"} }

func (c *Connector) Provision(_ context.Context, env *kit.Env) error {
	c.env = env
	c.base = env.Runner.Mailpit
	return nil
}

func (c *Connector) Health(ctx context.Context) error {
	_, err := c.get(ctx, "/api/v1/info")
	return err
}

func (c *Connector) get(ctx context.Context, p string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+p, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return raw, fmt.Errorf("mailpit %s: HTTP %d: %.200s", p, resp.StatusCode, raw)
	}
	return raw, nil
}

// Summary is a Mailpit message summary.
type Summary struct {
	ID      string    `json:"ID"`
	Subject string    `json:"Subject"`
	Created time.Time `json:"Created"`
	From    struct {
		Address string `json:"Address"`
	} `json:"From"`
	To []struct {
		Address string `json:"Address"`
	} `json:"To"`
	Snippet string `json:"Snippet"`
}

func (s Summary) Recipients() []string {
	var out []string
	for _, t := range s.To {
		out = append(out, t.Address)
	}
	return out
}

// Search returns messages matching a Mailpit query (newest first).
func (c *Connector) Search(ctx context.Context, query string) ([]Summary, error) {
	raw, err := c.get(ctx, "/api/v1/search?limit=500&query="+url.QueryEscape(query))
	if err != nil {
		return nil, err
	}
	var r struct {
		Messages []Summary `json:"messages"`
	}
	return r.Messages, json.Unmarshal(raw, &r)
}

// to returns messages whose recipients include addr exactly.
func (c *Connector) to(ctx context.Context, addr string) ([]Summary, error) {
	msgs, err := c.Search(ctx, fmt.Sprintf("to:%q", addr))
	if err != nil {
		return nil, err
	}
	var out []Summary
	for _, m := range msgs {
		for _, r := range m.Recipients() {
			if strings.EqualFold(r, addr) {
				out = append(out, m)
				break
			}
		}
	}
	return out, nil
}

func (c *Connector) Apply(context.Context, kit.Step) (kit.Result, error) {
	return kit.Result{}, fmt.Errorf("mail has no steps")
}

// Check resolves:
//
//	mail.to(<address|var>).count       messages received by that address
//	mail.to(<address|var>).subject     subject of the latest one
//	mail.to(<address|var>).text        text body of the latest one
//	mail.namespace.count               all messages of this execution (recipient contains the namespace)
func (c *Connector) Check(ctx context.Context, ref kit.CheckRef) (kit.Observation, error) {
	segs := ref.Segments
	at := time.Now().UTC()
	if segs[1].Name == "namespace" {
		msgs, err := c.Search(ctx, "to:"+string(c.env.NS))
		return kit.Observation{Value: len(msgs), Source: "Mailpit search to:" + string(c.env.NS), At: time.Now().UTC()}, err
	}
	if segs[1].Name != "to" || len(segs[1].Args) != 1 || len(segs) < 3 {
		return kit.Observation{At: at}, fmt.Errorf("expected mail.to(<address>).count|subject|text")
	}
	addr := segs[1].Args[0]
	msgs, err := c.to(ctx, addr)
	src := fmt.Sprintf("Mailpit messages to %s", addr)
	if err != nil {
		return kit.Observation{Source: src, At: time.Now().UTC()}, err
	}
	switch segs[2].Name {
	case "count":
		return kit.Observation{Value: len(msgs), Raw: msgs, Source: src, At: time.Now().UTC()}, nil
	case "subject", "text":
		if len(msgs) == 0 {
			return kit.Observation{Value: nil, Source: src, At: time.Now().UTC()}, nil
		}
		if segs[2].Name == "subject" {
			return kit.Observation{Value: msgs[0].Subject, Raw: msgs[0], Source: src + " (latest)", At: time.Now().UTC()}, nil
		}
		raw, err := c.get(ctx, "/api/v1/message/"+msgs[0].ID)
		var m struct {
			Text string `json:"Text"`
		}
		_ = json.Unmarshal(raw, &m)
		return kit.Observation{Value: strings.TrimSpace(m.Text), Source: src + " (latest, text part)", At: time.Now().UTC()}, err
	}
	return kit.Observation{At: at}, fmt.Errorf("unknown mail check %q", segs[2].Name)
}

// Collect saves every message of the namespace (headers + text).
func (c *Connector) Collect(ctx context.Context, _ kit.TimeWindow) ([]kit.Artifact, error) {
	if c.env == nil {
		return nil, nil
	}
	msgs, err := c.Search(ctx, "to:"+string(c.env.NS))
	if err != nil {
		return nil, err
	}
	var full []map[string]any
	for _, m := range msgs {
		raw, err := c.get(ctx, "/api/v1/message/"+m.ID)
		if err != nil {
			continue
		}
		var msg map[string]any
		_ = json.Unmarshal(raw, &msg)
		delete(msg, "HTML")
		delete(msg, "Attachments")
		full = append(full, msg)
	}
	rel := path.Join(c.env.CaseDir, "output", "mail", "messages.json")
	if _, err := c.env.Evidence.WriteJSON(rel, map[string]any{"namespace": c.env.NS, "count": len(full), "messages": full}); err != nil {
		return nil, err
	}
	return []kit.Artifact{{Kind: "mail", Path: rel, Title: fmt.Sprintf("Mails captured by Mailpit (%d)", len(full)), Source: "mail"}}, nil
}

// Teardown deletes the namespace's messages.
func (c *Connector) Teardown(ctx context.Context) error {
	if c.env == nil {
		return nil
	}
	msgs, err := c.Search(ctx, "to:"+string(c.env.NS))
	if err != nil || len(msgs) == 0 {
		return err
	}
	var ids []string
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	body, _ := json.Marshal(map[string]any{"IDs": ids})
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+"/api/v1/messages", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

var (
	_ kit.Connector = (*Connector)(nil)
	_ kit.Checker   = (*Connector)(nil)
)
