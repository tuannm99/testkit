package httpmock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tuannm99/testkit/testkit/core/evidence"
	"github.com/tuannm99/testkit/testkit/core/kit"
)

// Connector configures the HTTP/webhook mocks of a service in the Mock Hub
// for one namespace ("mock"). Checks: mock.<name>.*
type Connector struct {
	env    *kit.Env
	client *Client
	mocks  []string
}

func NewConnector() kit.Connector { return &Connector{} }

func (c *Connector) Name() string            { return "mock" }
func (c *Connector) CheckPrefixes() []string { return []string{"mock"} }

// specName maps a descriptor path to the file name served by the hub.
func specName(env *kit.Env, p string) (string, error) {
	abs := env.Service.Path(p)
	dir := env.Project.Abs(env.Project.MocksDir)
	rel, err := filepath.Rel(dir, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("openapi %s must live under mocks_dir %s (mounted into the Mock Hub)", p, env.Project.MocksDir)
	}
	return filepath.ToSlash(rel), nil
}

func (c *Connector) Provision(ctx context.Context, env *kit.Env) error {
	c.env = env
	c.client = NewClient(env.Runner.Mockhub)
	names := make([]string, 0)
	for n, m := range env.Service.Mocks {
		if m.Kind == "http" || m.Kind == "webhook" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		m := env.Service.Mocks[n]
		cfg := MockConfig{Kind: m.Kind, APIVersion: m.APIVersion, VerifiedAt: m.VerifiedAt, IdempotencyHeader: "Idempotency-Key"}
		if m.OpenAPI != "" {
			s, err := specName(env, m.OpenAPI)
			if err != nil {
				return err
			}
			cfg.OpenAPI = s
		}
		if m.Secret != "" {
			cfg.Signature = &SignatureConfig{Header: "X-Signature", Secret: m.Secret}
		}
		if err := c.client.Configure(ctx, string(env.NS), n, cfg); err != nil {
			return fmt.Errorf("configure mock %s: %w", n, err)
		}
		c.mocks = append(c.mocks, n)
	}
	return nil
}

func (c *Connector) Health(ctx context.Context) error { return c.client.Health(ctx) }

func (c *Connector) Apply(ctx context.Context, s kit.Step) (kit.Result, error) {
	ns := string(c.env.NS)
	switch s.Name {
	case "mock.script":
		mock := kit.Str(s.With, "mock")
		var script Script
		if rules, ok := s.With["rules"]; ok {
			raw, _ := json.Marshal(map[string]any{"rules": rules})
			if err := json.Unmarshal(raw, &script); err != nil {
				return kit.Result{}, fmt.Errorf("mock.script rules: %w", err)
			}
		} else {
			raw, _ := json.Marshal(s.With["responses"])
			var resps []Response
			if err := json.Unmarshal(raw, &resps); err != nil {
				return kit.Result{}, fmt.Errorf("mock.script responses: %w", err)
			}
			match := Match{Method: kit.Str(s.With, "method"), Path: kit.Str(s.With, "path"), Operation: kit.Str(s.With, "operation")}
			if match == (Match{}) {
				match.Operation = c.env.Service.Mocks[mock].Operation
			}
			script.Rules = []Rule{{Match: match, Responses: resps}}
		}
		if err := c.client.Script(ctx, ns, mock, script); err != nil {
			return kit.Result{}, err
		}
		var codes []string
		for _, r := range script.Rules {
			for _, x := range r.Responses {
				if x.Fault != "" {
					codes = append(codes, x.Fault)
				} else {
					codes = append(codes, fmt.Sprint(x.Status))
				}
			}
		}
		return kit.Result{Output: script, Note: fmt.Sprintf("%s answers %s (then repeats the last)", mock, strings.Join(codes, ", "))}, nil
	case "webhook.send":
		req := WebhookRequest{Mock: kit.Str(s.With, "mock"), URL: kit.Str(s.With, "url"), Method: kit.Str(s.With, "method"),
			Sign: kit.Str(s.With, "sign"), Header: kit.Str(s.With, "header"), Repeat: kit.Int(s.With, "repeat", 1),
			Delay: Duration(kit.Dur(s.With, "delay", 0))}
		if m, ok := c.env.Service.Mocks[req.Mock]; ok {
			req.Secret = m.Secret
		}
		raw, _ := json.Marshal(s.With["body"])
		req.Body = raw
		if h, ok := s.With["headers"].(map[string]any); ok {
			req.Headers = map[string]string{}
			for k, v := range h {
				req.Headers[k] = fmt.Sprint(v)
			}
		}
		res, err := c.client.SendWebhook(ctx, ns, req)
		return kit.Result{Output: res}, err
	}
	return kit.Result{}, fmt.Errorf("mock: unknown step %s", s.Name)
}

// Check resolves:
//
//	mock.<name>.calls[(status=201, path=/v1/charges, method=POST, operation=x)]   inbound calls
//	mock.<name>.schema_errors                 calls violating the OpenAPI contract
//	mock.<name>.idempotency_keys              distinct Idempotency-Key values
//	mock.<name>.missing_idempotency_key       calls without the key
//	mock.<name>.replayed                      calls answered from the idempotency store
//	mock.<name>.succeeded                     2xx answers that were not replays (real side effects)
//	mock.<name>.in_flight                     calls received and not answered yet
//	mock.<name>.webhooks[(status=200)]        outbound webhooks sent to the SUT
//	mock.<name>.unconfigured                  calls to a mock never configured
func (c *Connector) Check(ctx context.Context, ref kit.CheckRef) (kit.Observation, error) {
	segs := ref.Segments
	if len(segs) < 3 {
		return kit.Observation{At: time.Now().UTC()}, fmt.Errorf("expected mock.<name>.<metric>")
	}
	name, metric := segs[1].Name, segs[2]
	j, err := c.client.Journal(ctx, string(c.env.NS), name)
	at := time.Now().UTC()
	src := fmt.Sprintf("Mock Hub journal of %s in namespace %s (%d entries)", name, c.env.NS, len(j))
	if err != nil {
		return kit.Observation{Source: src, At: at}, err
	}
	in := func(e Entry) bool { return e.Direction == "in" }
	var n int
	switch metric.Name {
	case "calls":
		for _, e := range j {
			if in(e) && entryMatches(e, metric.KV) {
				n++
			}
		}
	case "schema_errors":
		for _, e := range j {
			if in(e) && len(e.SchemaErrors) > 0 {
				n++
			}
		}
	case "idempotency_keys":
		set := map[string]bool{}
		for _, e := range j {
			if in(e) && e.IdempotencyKey != "" {
				set[e.IdempotencyKey] = true
			}
		}
		n = len(set)
	case "missing_idempotency_key":
		for _, e := range j {
			if in(e) && e.IdempotencyKey == "" {
				n++
			}
		}
	case "succeeded":
		// Side effects really performed by the provider: 2xx answers delivered
		// to the client that were not idempotent replays (e.g. charges made).
		for _, e := range j {
			if in(e) && !e.InFlight && e.Error == "" && e.Status >= 200 && e.Status < 300 && !e.Replayed {
				n++
			}
		}
	case "in_flight":
		for _, e := range j {
			if in(e) && e.InFlight {
				n++
			}
		}
	case "replayed":
		for _, e := range j {
			if in(e) && e.Replayed {
				n++
			}
		}
	case "webhooks":
		for _, e := range j {
			if e.Direction == "out" && entryMatches(e, metric.KV) {
				n++
			}
		}
	case "unconfigured":
		for _, e := range j {
			if e.Unconfigured {
				n++
			}
		}
	default:
		return kit.Observation{Source: src, At: at}, fmt.Errorf("unknown mock check %q", metric.Name)
	}
	if len(metric.KV) > 0 {
		src += fmt.Sprintf(" filtered by %v", metric.KV)
	}
	return kit.Observation{Value: n, Source: src, At: at}, nil
}

func entryMatches(e Entry, kv map[string]string) bool {
	for k, v := range kv {
		switch k {
		case "status":
			if fmt.Sprint(e.Status) != v {
				return false
			}
		case "path":
			if e.Path != v {
				return false
			}
		case "method":
			if !strings.EqualFold(e.Method, v) {
				return false
			}
		case "operation":
			if e.Operation != v {
				return false
			}
		case "fault":
			if e.Fault != v {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// Collect saves the journal and the provenance of every mock.
func (c *Connector) Collect(ctx context.Context, _ kit.TimeWindow) ([]kit.Artifact, error) {
	if c.client == nil {
		return nil, nil
	}
	var out []kit.Artifact
	infos, err := c.client.Mocks(ctx, string(c.env.NS))
	if err != nil {
		return nil, err
	}
	prov := []evidence.MockProvenance{}
	for _, mi := range infos {
		p := evidence.MockProvenance{Service: c.env.Service.Name, Mock: mi.Name, Kind: mi.Config.Kind,
			APIVersion: mi.Config.APIVersion, VerifiedAt: mi.Config.VerifiedAt, Spec: mi.Config.OpenAPI}
		if mi.Config.OpenAPI != "" {
			if raw, err := os.ReadFile(filepath.Join(c.env.Project.Abs(c.env.Project.MocksDir), mi.Config.OpenAPI)); err == nil {
				sum := sha256.Sum256(raw)
				p.SpecSHA256 = hex.EncodeToString(sum[:])
			}
		}
		prov = append(prov, p)
	}
	for _, name := range c.mocks {
		j, err := c.client.Journal(ctx, string(c.env.NS), name)
		if err != nil {
			return out, err
		}
		rel := path.Join(c.env.CaseDir, "output", "mocks", name+".journal.json")
		if _, err := c.env.Evidence.WriteJSON(rel, map[string]any{"mock": name, "namespace": c.env.NS, "entries": j}); err != nil {
			return out, err
		}
		out = append(out, kit.Artifact{Kind: "journal", Path: rel, Title: fmt.Sprintf("Mock %s journal (%d calls)", name, len(j)), Source: "mock:" + name})
	}
	rel := path.Join(c.env.CaseDir, "output", "mocks", "provenance.json")
	if _, err := c.env.Evidence.WriteJSON(rel, prov); err == nil {
		out = append(out, kit.Artifact{Kind: "provenance", Path: rel, Title: "Mock provenance (API version, verification date, spec hash)", Source: "mock"})
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
