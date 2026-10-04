// Package elasticsearch is the Elasticsearch connector: one index per
// execution namespace (replicas forced to 0), `_refresh` before every check
// (search is near-real-time), document dumps as evidence, and faults such as
// blocking writes to produce `_bulk` answers that are HTTP 200 with
// per-item errors.
package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/tuannm99/testkit/testkit/core/kit"
)

type Connector struct {
	env  *kit.Env
	base string
	http *http.Client
}

func New() kit.Connector { return &Connector{http: &http.Client{Timeout: 30 * time.Second}} }

func (c *Connector) Name() string            { return "elasticsearch" }
func (c *Connector) CheckPrefixes() []string { return []string{"es"} }

func (c *Connector) do(ctx context.Context, method, p string, body any) ([]byte, int, error) {
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		r = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+p, r)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return raw, resp.StatusCode, err
}

// Index returns the namespaced index of a logical index name.
func (c *Connector) Index(name string) string { return c.env.NS.Index(name) }

func (c *Connector) Provision(ctx context.Context, env *kit.Env) error {
	c.env = env
	c.base = env.Runner.Elasticsearch
	names := make([]string, 0, len(env.Service.Stores.Elasticsearch.Indices))
	for n := range env.Service.Stores.Elasticsearch.Indices {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		def := map[string]any{}
		if f := env.Service.Stores.Elasticsearch.Indices[n].Mappings; f != "" {
			raw, err := os.ReadFile(env.Service.Path(f))
			if err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &def); err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
		}
		settings, _ := def["settings"].(map[string]any)
		if settings == nil {
			settings = map[string]any{}
		}
		settings["number_of_replicas"] = 0 // single node: replicas would keep the index yellow
		def["settings"] = settings
		raw, code, err := c.do(ctx, http.MethodPut, "/"+c.Index(n), def)
		if err != nil {
			return err
		}
		if code != 200 {
			return fmt.Errorf("create index %s: HTTP %d: %.300s", c.Index(n), code, raw)
		}
	}
	return nil
}

func (c *Connector) Health(ctx context.Context) error {
	_, code, err := c.do(ctx, http.MethodGet, "/_cluster/health", nil)
	if err == nil && code != 200 {
		err = fmt.Errorf("HTTP %d", code)
	}
	return err
}

func (c *Connector) entityIndex(entity string) (string, error) {
	if e, ok := c.env.Service.Entities[entity]; ok && e.Elasticsearch != nil {
		return c.Index(e.Elasticsearch.Table), nil
	}
	return "", fmt.Errorf("entity %q has no elasticsearch mapping in service %s", entity, c.env.Service.Name)
}

func (c *Connector) refresh(ctx context.Context, index string) error {
	raw, code, err := c.do(ctx, http.MethodPost, "/"+index+"/_refresh", nil)
	if err == nil && code != 200 {
		err = fmt.Errorf("refresh %s: HTTP %d: %.200s", index, code, raw)
	}
	return err
}

func (c *Connector) Apply(ctx context.Context, s kit.Step) (kit.Result, error) {
	switch s.Name {
	case "es.insert":
		idx, err := c.entityIndex(kit.Str(s.With, "entity"))
		if err != nil {
			return kit.Result{}, err
		}
		rows, _ := s.With["rows"].([]any)
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for _, r := range rows {
			m, _ := r.(map[string]any)
			meta := map[string]any{"_index": idx}
			if id, ok := m["_id"]; ok {
				meta["_id"] = fmt.Sprint(id)
				delete(m, "_id")
			}
			_ = enc.Encode(map[string]any{"index": meta})
			_ = enc.Encode(m)
		}
		raw, code, err := c.do(ctx, http.MethodPost, "/_bulk?refresh=true", buf.Bytes())
		if err == nil && (code != 200 || bytes.Contains(raw, []byte(`"errors":true`))) {
			err = fmt.Errorf("es.insert: HTTP %d: %.300s", code, raw)
		}
		return kit.Result{Note: fmt.Sprintf("%d doc(s) into %s", len(rows), idx)}, err
	case "es.refresh":
		return kit.Result{}, c.refresh(ctx, c.Index(kit.Str(s.With, "index")))
	case "es.block_writes":
		// Fault: the index rejects writes; _bulk still answers HTTP 200 with errors=true per item.
		idx := c.Index(kit.Str(s.With, "index"))
		on := kit.Str(s.With, "enabled") != "false"
		raw, code, err := c.do(ctx, http.MethodPut, "/"+idx+"/_settings", map[string]any{"index.blocks.write": on})
		if err == nil && code != 200 {
			err = fmt.Errorf("block writes %s: HTTP %d: %.200s", idx, code, raw)
		}
		return kit.Result{Note: fmt.Sprintf("index.blocks.write=%v on %s", on, idx)}, err
	}
	return kit.Result{}, fmt.Errorf("elasticsearch: unknown step %s", s.Name)
}

// Check resolves (after _refresh):
//
//	es.<entity>.<id>.<field>        field of one document (null when absent)
//	es.<entity>.<id>.exists         document exists
//	es.<entity>.count(field=v)      number of documents (term filters)
func (c *Connector) Check(ctx context.Context, ref kit.CheckRef) (kit.Observation, error) {
	now := time.Now().UTC()
	segs := ref.Segments
	idx, err := c.entityIndex(segs[1].Name)
	if err != nil {
		return kit.Observation{At: now}, err
	}
	if err := c.refresh(ctx, idx); err != nil {
		return kit.Observation{At: now}, err
	}
	if len(segs) == 3 && segs[2].Name == "count" {
		q := map[string]any{"query": map[string]any{"match_all": map[string]any{}}}
		if len(segs[2].KV) > 0 {
			var terms []any
			keys := make([]string, 0, len(segs[2].KV))
			for k := range segs[2].KV {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				terms = append(terms, map[string]any{"term": map[string]any{k: segs[2].KV[k]}})
			}
			q = map[string]any{"query": map[string]any{"bool": map[string]any{"filter": terms}}}
		}
		raw, code, err := c.do(ctx, http.MethodPost, "/"+idx+"/_count", q)
		src := fmt.Sprintf("POST /%s/_count %s (after _refresh)", idx, mustJSON(q))
		if err != nil || code != 200 {
			return kit.Observation{Source: src, At: time.Now().UTC()}, fmt.Errorf("count: HTTP %d %v %.200s", code, err, raw)
		}
		var r struct {
			Count int64 `json:"count"`
		}
		_ = json.Unmarshal(raw, &r)
		return kit.Observation{Value: r.Count, Source: src, At: time.Now().UTC()}, nil
	}
	if len(segs) < 3 {
		return kit.Observation{At: now}, fmt.Errorf("expected es.<entity>.<id>[.<field>] or es.<entity>.count")
	}
	id := segs[2].Name
	raw, code, err := c.do(ctx, http.MethodGet, "/"+idx+"/_doc/"+url.PathEscape(id), nil)
	src := fmt.Sprintf("GET /%s/_doc/%s (after _refresh)", idx, id)
	at := time.Now().UTC()
	if err != nil {
		return kit.Observation{Source: src, At: at}, err
	}
	if code == 404 {
		if len(segs) == 4 && segs[3].Name == "exists" {
			return kit.Observation{Value: false, Source: src, At: at}, nil
		}
		return kit.Observation{Value: nil, Source: src, At: at}, nil
	}
	var doc struct {
		Source map[string]any `json:"_source"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || code != 200 {
		return kit.Observation{Source: src, At: at}, fmt.Errorf("get doc: HTTP %d %.200s", code, raw)
	}
	if len(segs) == 3 {
		return kit.Observation{Value: doc.Source, Raw: doc.Source, Source: src, At: at}, nil
	}
	if segs[3].Name == "exists" {
		return kit.Observation{Value: true, Raw: doc.Source, Source: src, At: at}, nil
	}
	return kit.Observation{Value: doc.Source[segs[3].Name], Raw: doc.Source, Source: src + " → ." + segs[3].Name, At: at}, nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Collect dumps the documents of every namespace index.
func (c *Connector) Collect(ctx context.Context, _ kit.TimeWindow) ([]kit.Artifact, error) {
	if c.env == nil {
		return nil, nil
	}
	var out []kit.Artifact
	for n := range c.env.Service.Stores.Elasticsearch.Indices {
		idx := c.Index(n)
		_ = c.refresh(ctx, idx)
		raw, code, err := c.do(ctx, http.MethodPost, "/"+idx+"/_search?size=1000&sort=_doc", map[string]any{"query": map[string]any{"match_all": map[string]any{}}})
		if err != nil || code != 200 {
			continue
		}
		var r struct {
			Hits struct {
				Total struct {
					Value int `json:"value"`
				} `json:"total"`
				Hits []struct {
					ID     string         `json:"_id"`
					Source map[string]any `json:"_source"`
				} `json:"hits"`
			} `json:"hits"`
		}
		_ = json.Unmarshal(raw, &r)
		rel := path.Join(c.env.CaseDir, "output", "elasticsearch", strings.ReplaceAll(n, "/", "_")+".json")
		if _, err := c.env.Evidence.WriteJSON(rel, map[string]any{"index": idx, "total": r.Hits.Total.Value, "documents": r.Hits.Hits}); err != nil {
			return out, err
		}
		out = append(out, kit.Artifact{Kind: "snapshot", Path: rel, Title: fmt.Sprintf("Elasticsearch %s (%d docs)", n, r.Hits.Total.Value), Source: "es"})
	}
	return out, nil
}

func (c *Connector) Teardown(ctx context.Context) error {
	if c.env == nil {
		return nil
	}
	var idx []string
	for n := range c.env.Service.Stores.Elasticsearch.Indices {
		idx = append(idx, c.Index(n))
	}
	if len(idx) == 0 {
		return nil
	}
	_, code, err := c.do(ctx, http.MethodDelete, "/"+strings.Join(idx, ",")+"?ignore_unavailable=true", nil)
	if err == nil && code != 200 {
		err = fmt.Errorf("delete indices: HTTP %d", code)
	}
	return err
}

var (
	_ kit.Connector = (*Connector)(nil)
	_ kit.Checker   = (*Connector)(nil)
)
