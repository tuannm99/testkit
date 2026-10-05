// Package clickhouse is the ClickHouse connector (HTTP interface): one
// database per namespace, migrations, checks that first flush pending async
// inserts (consistency wait), duplicate/part counters, OPTIMIZE ... FINAL.
package clickhouse

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
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tuannm99/testkit/testkit/core/kit"
)

type Connector struct {
	env  *kit.Env
	base string
	user string
	pass string
	http *http.Client
	db   string
}

func New() kit.Connector { return &Connector{http: &http.Client{Timeout: 30 * time.Second}} }

func (c *Connector) Name() string            { return "clickhouse" }
func (c *Connector) CheckPrefixes() []string { return []string{"clickhouse"} }

var ident = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func quote(id string) (string, error) {
	if !ident.MatchString(id) {
		return "", fmt.Errorf("invalid identifier %q", id)
	}
	return "`" + id + "`", nil
}

// Query runs one statement; params become {name:String} query parameters.
func (c *Connector) Query(ctx context.Context, sql string, params map[string]string, db string) ([]byte, error) {
	q := url.Values{}
	if db != "" {
		q.Set("database", db)
	}
	for k, v := range params {
		q.Set("param_"+k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/?"+q.Encode(), strings.NewReader(sql))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.user, c.pass)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return raw, fmt.Errorf("clickhouse: HTTP %d: %.400s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	return raw, nil
}

type jsonResult struct {
	Data []map[string]any `json:"data"`
	Rows int              `json:"rows"`
}

func (c *Connector) rows(ctx context.Context, sql string, params map[string]string) ([]map[string]any, error) {
	raw, err := c.Query(ctx, sql+" FORMAT JSON", params, c.db)
	if err != nil {
		return nil, err
	}
	var r jsonResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("clickhouse: decode: %w", err)
	}
	return r.Data, nil
}

func (c *Connector) Provision(ctx context.Context, env *kit.Env) error {
	c.env, c.base, c.user, c.pass = env, env.Runner.ClickHouse, env.Runner.CHUser, env.Runner.CHPassword
	c.db = env.NS.Database()
	qdb, err := quote(c.db)
	if err != nil {
		return err
	}
	if _, err := c.Query(ctx, "CREATE DATABASE "+qdb, nil, ""); err != nil {
		return err
	}
	st := env.Service.Stores.ClickHouse
	if st == nil || st.Migrations == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(env.Service.Path(st.Migrations), "*.sql"))
	sort.Strings(files)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		for _, stmt := range splitStatements(string(raw)) {
			if _, err := c.Query(ctx, stmt, nil, c.db); err != nil {
				return fmt.Errorf("migration %s: %w", filepath.Base(f), err)
			}
		}
	}
	return nil
}

// splitStatements splits a migration on ';' and drops comment-only lines.
func splitStatements(sql string) []string {
	var lines []string
	for _, l := range strings.Split(sql, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "--") {
			continue
		}
		lines = append(lines, l)
	}
	var out []string
	for _, s := range strings.Split(strings.Join(lines, "\n"), ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (c *Connector) Health(ctx context.Context) error {
	_, err := c.Query(ctx, "SELECT 1", nil, "")
	return err
}

func (c *Connector) table(entity string) (string, string, error) {
	if e, ok := c.env.Service.Entities[entity]; ok && e.ClickHouse != nil {
		return e.ClickHouse.Table, e.ClickHouse.Key, nil
	}
	return "", "", fmt.Errorf("entity %q has no clickhouse mapping in service %s", entity, c.env.Service.Name)
}

func (c *Connector) Apply(ctx context.Context, s kit.Step) (kit.Result, error) {
	switch s.Name {
	case "clickhouse.exec":
		_, err := c.Query(ctx, kit.Str(s.With, "sql"), nil, c.db)
		return kit.Result{}, err
	case "clickhouse.optimize":
		t, err := quote(kit.Str(s.With, "table"))
		if err != nil {
			return kit.Result{}, err
		}
		sql := "OPTIMIZE TABLE " + t
		if kit.Str(s.With, "final") != "false" {
			sql += " FINAL"
		}
		_, err = c.Query(ctx, sql, nil, c.db)
		return kit.Result{Note: sql}, err
	}
	return kit.Result{}, fmt.Errorf("clickhouse: unknown step %s", s.Name)
}

// flush waits for pending async inserts (the ClickHouse consistency wait).
func (c *Connector) flush(ctx context.Context) {
	_, _ = c.Query(ctx, "SYSTEM FLUSH ASYNC INSERT QUEUE", nil, "")
}

// Check resolves (after flushing async inserts):
//
//	clickhouse.<entity>.count(col=v)        rows (optional equality filters)
//	clickhouse.<entity>.duplicates          rows minus distinct keys (retries that duplicated data)
//	clickhouse.<entity>.<key>.<col>         a column of the row with that key
//	clickhouse.parts(<table>)               active parts (too many parts = insert pattern problem)
func (c *Connector) Check(ctx context.Context, ref kit.CheckRef) (kit.Observation, error) {
	c.flush(ctx)
	segs := ref.Segments
	at := time.Now().UTC()
	if segs[1].Name == "parts" {
		if len(segs[1].Args) != 1 {
			return kit.Observation{At: at}, fmt.Errorf("expected clickhouse.parts(<table>)")
		}
		sql := "SELECT count() AS n FROM system.parts WHERE database = {db:String} AND table = {t:String} AND active"
		r, err := c.rows(ctx, sql, map[string]string{"db": c.db, "t": segs[1].Args[0]})
		return kit.Observation{Value: first(r, "n"), Source: sql, At: time.Now().UTC()}, err
	}
	tbl, key, err := c.table(segs[1].Name)
	if err != nil {
		return kit.Observation{At: at}, err
	}
	qt, err := quote(tbl)
	if err != nil {
		return kit.Observation{At: at}, err
	}
	if len(segs) < 3 {
		return kit.Observation{At: at}, fmt.Errorf("expected clickhouse.<entity>.count|duplicates|<key>.<col>")
	}
	switch segs[2].Name {
	case "count":
		where, params, err := filters(segs[2].KV)
		if err != nil {
			return kit.Observation{At: at}, err
		}
		sql := "SELECT count() AS n FROM " + qt + where
		r, err := c.rows(ctx, sql, params)
		return kit.Observation{Value: first(r, "n"), Source: fmt.Sprintf("%s %v (database %s, after SYSTEM FLUSH ASYNC INSERT QUEUE)", sql, params, c.db), At: time.Now().UTC()}, err
	case "duplicates":
		qk, err := quote(key)
		if err != nil {
			return kit.Observation{At: at}, err
		}
		sql := fmt.Sprintf("SELECT count() - uniqExact(%s) AS n FROM %s", qk, qt)
		r, err := c.rows(ctx, sql, nil)
		return kit.Observation{Value: first(r, "n"), Source: sql + " (database " + c.db + ")", At: time.Now().UTC()}, err
	}
	if len(segs) != 4 {
		return kit.Observation{At: at}, fmt.Errorf("expected clickhouse.<entity>.<key>.<column>")
	}
	qk, err := quote(key)
	if err != nil {
		return kit.Observation{At: at}, err
	}
	col, err := quote(segs[3].Name)
	if err != nil {
		return kit.Observation{At: at}, err
	}
	sql := fmt.Sprintf("SELECT %s AS v FROM %s WHERE toString(%s) = {k:String} LIMIT 1", col, qt, qk)
	r, err := c.rows(ctx, sql, map[string]string{"k": segs[2].Name})
	return kit.Observation{Value: first(r, "v"), Raw: r, Source: sql, At: time.Now().UTC()}, err
}

func first(rows []map[string]any, k string) any {
	if len(rows) == 0 {
		return nil
	}
	return rows[0][k]
}

func filters(kv map[string]string) (string, map[string]string, error) {
	if len(kv) == 0 {
		return "", nil, nil
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	params := map[string]string{}
	for i, k := range keys {
		q, err := quote(k)
		if err != nil {
			return "", nil, err
		}
		p := fmt.Sprintf("p%d", i)
		parts = append(parts, fmt.Sprintf("toString(%s) = {%s:String}", q, p))
		params[p] = kv[k]
	}
	return " WHERE " + strings.Join(parts, " AND "), params, nil
}

// Collect dumps the snapshot tables.
func (c *Connector) Collect(ctx context.Context, _ kit.TimeWindow) ([]kit.Artifact, error) {
	if c.env == nil || c.env.Service.Stores.ClickHouse == nil {
		return nil, nil
	}
	c.flush(ctx)
	var out []kit.Artifact
	for _, t := range c.env.Service.Stores.ClickHouse.Snapshot {
		qt, err := quote(t)
		if err != nil {
			return out, err
		}
		rows, err := c.rows(ctx, "SELECT * FROM "+qt+" LIMIT 1000", nil)
		if err != nil {
			return out, err
		}
		parts, _ := c.rows(ctx, "SELECT count() AS n FROM system.parts WHERE database = {db:String} AND table = {t:String} AND active",
			map[string]string{"db": c.db, "t": t})
		rel := path.Join(c.env.CaseDir, "output", "clickhouse", t+".json")
		if _, err := c.env.Evidence.WriteJSON(rel, map[string]any{"database": c.db, "table": t, "rows": rows,
			"active_parts": first(parts, "n"), "taken_at": time.Now().UTC()}); err != nil {
			return out, err
		}
		out = append(out, kit.Artifact{Kind: "snapshot", Path: rel, Title: fmt.Sprintf("ClickHouse %s (%d rows)", t, len(rows)), Source: "clickhouse"})
	}
	return out, nil
}

func (c *Connector) Teardown(ctx context.Context) error {
	if c.env == nil {
		return nil
	}
	qdb, err := quote(c.db)
	if err != nil {
		return err
	}
	_, err = c.Query(ctx, "DROP DATABASE IF EXISTS "+qdb+" SYNC", nil, "")
	return err
}

var (
	_ kit.Connector = (*Connector)(nil)
	_ kit.Checker   = (*Connector)(nil)
)
