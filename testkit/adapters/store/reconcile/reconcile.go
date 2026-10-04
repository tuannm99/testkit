// Package reconcile compares the same set of business ids across stores
// after an execution ("reconcile" connector): counts, duplicates inside a
// store, ids missing from a store, and a sha256 of each sorted id set.
// A store that silently lost or duplicated writes (ES bulk item errors,
// ClickHouse retries) shows up here even when each store looks fine alone.
package reconcile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/tuannm99/testkit/testkit/adapters/store/postgres"
	"github.com/tuannm99/testkit/testkit/core/config"
	"github.com/tuannm99/testkit/testkit/core/kit"
)

type Connector struct {
	env     *kit.Env
	http    *http.Client
	reports map[string]Report
}

func New() kit.Connector {
	return &Connector{http: &http.Client{Timeout: 30 * time.Second}, reports: map[string]Report{}}
}

func (c *Connector) Name() string            { return "reconcile" }
func (c *Connector) CheckPrefixes() []string { return []string{"reconcile"} }

func (c *Connector) Provision(_ context.Context, env *kit.Env) error { c.env = env; return nil }
func (c *Connector) Health(context.Context) error                    { return nil }
func (c *Connector) Apply(context.Context, kit.Step) (kit.Result, error) {
	return kit.Result{}, fmt.Errorf("reconcile has no steps")
}

// SourceResult is what one store holds.
type SourceResult struct {
	Store      string   `json:"store"`
	Query      string   `json:"query"`
	Count      int      `json:"count"`
	Distinct   int      `json:"distinct"`
	Duplicates []string `json:"duplicates,omitempty"`
	Missing    []string `json:"missing,omitempty"` // ids present elsewhere, absent here
	SHA256     string   `json:"sha256"`            // of the sorted distinct ids
	Error      string   `json:"error,omitempty"`
}

// Report of one reconciliation.
type Report struct {
	Name       string         `json:"name"`
	At         time.Time      `json:"at"`
	Sources    []SourceResult `json:"sources"`
	Union      int            `json:"union"`
	Mismatches int            `json:"mismatches"` // missing ids + duplicates, all stores
	Match      bool           `json:"match"`
}

// Run reconciles one declared set.
func (c *Connector) Run(ctx context.Context, name string) (Report, error) {
	spec, ok := c.env.Service.Reconcile[name]
	if !ok {
		return Report{}, fmt.Errorf("service %s declares no reconcile %q", c.env.Service.Name, name)
	}
	rep := Report{Name: name, At: time.Now().UTC()}
	sets := make([]map[string]int, len(spec.Sources))
	union := map[string]bool{}
	for i, src := range spec.Sources {
		ids, query, err := c.ids(ctx, src)
		r := SourceResult{Store: src.Store, Query: query}
		if err != nil {
			r.Error = err.Error()
			rep.Sources = append(rep.Sources, r)
			return rep, fmt.Errorf("reconcile %s: %s: %w", name, src.Store, err)
		}
		m := map[string]int{}
		for _, id := range ids {
			m[id]++
			union[id] = true
		}
		sets[i] = m
		r.Count, r.Distinct = len(ids), len(m)
		for id, n := range m {
			if n > 1 {
				r.Duplicates = append(r.Duplicates, fmt.Sprintf("%s ×%d", id, n))
			}
		}
		sort.Strings(r.Duplicates)
		r.SHA256 = hashSet(m)
		rep.Sources = append(rep.Sources, r)
	}
	rep.Union = len(union)
	for i := range rep.Sources {
		for id := range union {
			if sets[i][id] == 0 {
				rep.Sources[i].Missing = append(rep.Sources[i].Missing, id)
			}
		}
		sort.Strings(rep.Sources[i].Missing)
		rep.Mismatches += len(rep.Sources[i].Missing) + len(rep.Sources[i].Duplicates)
	}
	rep.Match = rep.Mismatches == 0
	c.reports[name] = rep
	return rep, nil
}

func hashSet(m map[string]int) string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return hex.EncodeToString(sum[:])
}

func (c *Connector) ids(ctx context.Context, src config.ReconcileSource) ([]string, string, error) {
	ns := c.env.NS
	switch src.Store {
	case "postgres":
		pool, err := postgres.Open(ctx, c.env.Runner.Postgres, ns.Database())
		if err != nil {
			return nil, src.SQL, err
		}
		defer pool.Close()
		// The query returns one column: the id.
		rows, err := pool.Query(ctx, "SELECT x::text FROM ("+src.SQL+") AS q(x)")
		if err != nil {
			return nil, src.SQL, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return nil, src.SQL, err
			}
			out = append(out, s)
		}
		return out, src.SQL + " (database " + ns.Database() + ")", rows.Err()
	case "clickhouse":
		q := url.Values{}
		q.Set("database", ns.Database())
		_, _ = c.post(ctx, c.env.Runner.ClickHouse+"/", "SYSTEM FLUSH ASYNC INSERT QUEUE", true)
		raw, err := c.post(ctx, c.env.Runner.ClickHouse+"/?"+q.Encode(), src.SQL+" FORMAT TSV", true)
		if err != nil {
			return nil, src.SQL, err
		}
		var out []string
		for _, l := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			if l != "" {
				out = append(out, strings.SplitN(l, "\t", 2)[0])
			}
		}
		return out, src.SQL + " (database " + ns.Database() + ")", nil
	case "elasticsearch":
		idx := ns.Index(src.Index)
		_, _ = c.post(ctx, c.env.Runner.Elasticsearch+"/"+idx+"/_refresh", "", false)
		var filters []any
		for k, v := range src.Term {
			filters = append(filters, map[string]any{"term": map[string]any{k: v}})
		}
		body, _ := json.Marshal(map[string]any{"size": 10000, "_source": []string{src.Field},
			"query": map[string]any{"bool": map[string]any{"filter": filters}}})
		raw, err := c.post(ctx, c.env.Runner.Elasticsearch+"/"+idx+"/_search", string(body), false)
		query := fmt.Sprintf("POST /%s/_search %s", idx, body)
		if err != nil {
			return nil, query, err
		}
		var r struct {
			Hits struct {
				Hits []struct {
					Source map[string]any `json:"_source"`
				} `json:"hits"`
			} `json:"hits"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, query, err
		}
		var out []string
		for _, h := range r.Hits.Hits {
			out = append(out, fmt.Sprint(h.Source[src.Field]))
		}
		return out, query, nil
	case "mongo":
		cl, err := mongo.Connect(options.Client().ApplyURI(c.env.Runner.Mongo).SetServerSelectionTimeout(10 * time.Second))
		if err != nil {
			return nil, "", err
		}
		defer cl.Disconnect(context.Background()) //nolint:errcheck
		filter := bson.M{}
		for k, v := range src.Filter {
			filter[k] = v
		}
		query := fmt.Sprintf("db.%s.find(%v, {%s: 1}) in %s", src.Collection, filter, src.Field, ns.Database())
		cur, err := cl.Database(ns.Database()).Collection(src.Collection).Find(ctx, filter)
		if err != nil {
			return nil, query, err
		}
		defer cur.Close(ctx)
		var out []string
		for cur.Next(ctx) {
			var d bson.M
			if err := cur.Decode(&d); err == nil {
				out = append(out, fmt.Sprint(d[src.Field]))
			}
		}
		return out, query, cur.Err()
	}
	return nil, "", fmt.Errorf("unknown store %q", src.Store)
}

func (c *Connector) post(ctx context.Context, u, body string, chAuth bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader([]byte(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if chAuth {
		req.SetBasicAuth(c.env.Runner.CHUser, c.env.Runner.CHPassword)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return raw, fmt.Errorf("HTTP %d: %.300s", resp.StatusCode, raw)
	}
	return raw, nil
}

// Check resolves:
//
//	reconcile.<name>.mismatches         missing ids + duplicates over every store (0 = consistent)
//	reconcile.<name>.match              true when every store holds exactly the same id set
//	reconcile.<name>.count(store=x)     ids found in one store
func (c *Connector) Check(ctx context.Context, ref kit.CheckRef) (kit.Observation, error) {
	segs := ref.Segments
	at := time.Now().UTC()
	if len(segs) != 3 {
		return kit.Observation{At: at}, fmt.Errorf("expected reconcile.<name>.mismatches|match|count(store=x)")
	}
	rep, err := c.Run(ctx, segs[1].Name)
	src := fmt.Sprintf("reconcile %s over %d store(s), %d distinct ids in total", rep.Name, len(rep.Sources), rep.Union)
	if err != nil {
		return kit.Observation{Raw: rep, Source: src, At: time.Now().UTC()}, err
	}
	switch segs[2].Name {
	case "mismatches":
		return kit.Observation{Value: rep.Mismatches, Raw: rep, Source: src, At: rep.At}, nil
	case "match":
		return kit.Observation{Value: rep.Match, Raw: rep, Source: src, At: rep.At}, nil
	case "count":
		store := segs[2].KV["store"]
		for _, s := range rep.Sources {
			if s.Store == store {
				return kit.Observation{Value: s.Count, Raw: rep, Source: src, At: rep.At}, nil
			}
		}
		return kit.Observation{At: at}, fmt.Errorf("reconcile %s has no source %q", rep.Name, store)
	}
	return kit.Observation{At: at}, fmt.Errorf("unknown reconcile check %q", segs[2].Name)
}

// Collect runs every declared reconciliation and writes the reports.
func (c *Connector) Collect(ctx context.Context, _ kit.TimeWindow) ([]kit.Artifact, error) {
	if c.env == nil {
		return nil, nil
	}
	var out []kit.Artifact
	for _, name := range config.SortedKeys(c.env.Service.Reconcile) {
		rep, err := c.Run(ctx, name)
		rel := path.Join(c.env.CaseDir, "output", "reconcile", name+".json")
		if _, werr := c.env.Evidence.WriteJSON(rel, rep); werr != nil {
			return out, werr
		}
		title := fmt.Sprintf("Reconcile %s: %d mismatch(es) across %d store(s)", name, rep.Mismatches, len(rep.Sources))
		if err != nil {
			title = fmt.Sprintf("Reconcile %s: error %v", name, err)
		}
		out = append(out, kit.Artifact{Kind: "reconcile", Path: rel, Title: title, Source: "reconcile"})
	}
	return out, nil
}

func (c *Connector) Teardown(context.Context) error { return nil }

var (
	_ kit.Connector = (*Connector)(nil)
	_ kit.Checker   = (*Connector)(nil)
)
