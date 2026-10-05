package socket

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"time"

	httpmock "github.com/tuannm99/testkit/testkit/adapters/mock/http"
	"github.com/tuannm99/testkit/testkit/core/kit"
)

// Connector configures the socket partners of a service ("socket"). Checks:
// socket.<mock>.received|distinct|duplicates(type=x), connections, acks.
type Connector struct {
	env    *kit.Env
	client *httpmock.Client
	mocks  []string
}

func NewConnector() kit.Connector { return &Connector{} }

func (c *Connector) Name() string            { return "socket" }
func (c *Connector) CheckPrefixes() []string { return []string{"socket"} }

func (c *Connector) Provision(ctx context.Context, env *kit.Env) error {
	c.env = env
	c.client = httpmock.NewClient(env.Runner.Mockhub)
	var names []string
	for n, m := range env.Service.Mocks {
		if m.Kind == "socket" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		proto := env.Service.Mocks[n].Protocol
		if proto == "" {
			proto = "ws"
		}
		// Default partner: acks every message and answers heartbeats.
		if _, err := c.client.ConfigureSocket(ctx, string(env.NS), n, Config{Protocol: proto, AutoAck: true, Pong: true}); err != nil {
			return fmt.Errorf("configure socket mock %s: %w", n, err)
		}
		c.mocks = append(c.mocks, n)
	}
	return nil
}

func (c *Connector) Health(ctx context.Context) error { return c.client.Health(ctx) }

// Apply: socket.configure {mock, auto_ack, pong, faults, script, framing}.
func (c *Connector) Apply(ctx context.Context, s kit.Step) (kit.Result, error) {
	if s.Name != "socket.configure" {
		return kit.Result{}, fmt.Errorf("socket: unknown step %s", s.Name)
	}
	mock := kit.Str(s.With, "mock")
	cfg := map[string]any{"protocol": c.env.Service.Mocks[mock].Protocol, "auto_ack": true, "pong": true}
	if cfg["protocol"] == "" {
		cfg["protocol"] = "ws"
	}
	for _, k := range []string{"auto_ack", "pong", "faults", "script", "framing"} {
		if v, ok := s.With[k]; ok {
			cfg[k] = v
		}
	}
	port, err := c.client.ConfigureSocket(ctx, string(c.env.NS), mock, cfg)
	raw, _ := json.Marshal(cfg)
	return kit.Result{Output: map[string]any{"config": cfg, "port": port}, Note: fmt.Sprintf("%s: %s", mock, raw)}, err
}

// Check resolves journal counters of a socket partner.
func (c *Connector) Check(ctx context.Context, ref kit.CheckRef) (kit.Observation, error) {
	segs := ref.Segments
	at := time.Now().UTC()
	if len(segs) < 3 {
		return kit.Observation{At: at}, fmt.Errorf("expected socket.<mock>.received|distinct|duplicates|connections|acks")
	}
	mock, metric := segs[1].Name, segs[2]
	j, err := c.client.Journal(ctx, string(c.env.NS), mock)
	src := fmt.Sprintf("Mock Hub socket journal of %s in %s (%d entries)", mock, c.env.NS, len(j))
	if err != nil {
		return kit.Observation{Source: src, At: at}, err
	}
	match := func(e httpmock.Entry) bool {
		if e.Path != "message" {
			return false
		}
		if t, ok := metric.KV["type"]; ok && e.Headers["type"] != t {
			return false
		}
		return true
	}
	ids := map[string]int{}
	received := 0
	for _, e := range j {
		if match(e) {
			received++
			ids[e.Headers["id"]]++
		}
	}
	var v int
	switch metric.Name {
	case "received":
		v = received
	case "distinct":
		v = len(ids)
	case "duplicates":
		v = received - len(ids)
	case "connections":
		for _, e := range j {
			if e.Path == "connect" {
				v++
			}
		}
	case "acks":
		for _, e := range j {
			if e.Path == "sent ack" {
				v++
			}
		}
	default:
		return kit.Observation{Source: src, At: at}, fmt.Errorf("unknown socket check %q", metric.Name)
	}
	return kit.Observation{Value: v, Source: src, At: time.Now().UTC()}, nil
}

// Collect saves the journal of every socket partner.
func (c *Connector) Collect(ctx context.Context, _ kit.TimeWindow) ([]kit.Artifact, error) {
	var out []kit.Artifact
	for _, m := range c.mocks {
		j, err := c.client.Journal(ctx, string(c.env.NS), m)
		if err != nil {
			return out, err
		}
		rel := path.Join(c.env.CaseDir, "output", "sockets", m+".journal.json")
		if _, err := c.env.Evidence.WriteJSON(rel, map[string]any{"mock": m, "namespace": c.env.NS, "entries": j}); err != nil {
			return out, err
		}
		out = append(out, kit.Artifact{Kind: "journal", Path: rel, Title: fmt.Sprintf("Socket partner %s journal (%d events)", m, len(j)), Source: "socket"})
	}
	return out, nil
}

func (c *Connector) Teardown(ctx context.Context) error {
	if c.client == nil {
		return nil
	}
	return c.client.Reset(ctx, string(c.env.NS))
}

var (
	_ kit.Connector = (*Connector)(nil)
	_ kit.Checker   = (*Connector)(nil)
)
