// Package postgres is the Postgres connector: one database per execution
// namespace, migrations from the service descriptor, fixtures, checks on
// entities, table snapshots as evidence, and DROP DATABASE at teardown.
package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tuannm99/testkit/testkit/core/config"
	"github.com/tuannm99/testkit/testkit/core/kit"
)

// Connector implements kit.Connector and kit.Checker for prefix "postgres".
type Connector struct {
	env   *kit.Env
	admin *pgxpool.Pool
	Pool  *pgxpool.Pool // the namespace database (used by the db-poll trigger too)
	db    string
}

func New() kit.Connector { return &Connector{} }

func (c *Connector) Name() string            { return "postgres" }
func (c *Connector) CheckPrefixes() []string { return []string{"postgres"} }

var ident = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// QuoteIdent validates and quotes an SQL identifier.
func QuoteIdent(s string) (string, error) {
	if !ident.MatchString(s) {
		return "", fmt.Errorf("invalid identifier %q", s)
	}
	return `"` + s + `"`, nil
}

// Open connects to a database of the TestKit Postgres.
func Open(ctx context.Context, pg config.PG, db string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(pg.DSN(db))
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres %s:%s/%s: %w", pg.Host, pg.Port, db, err)
	}
	return pool, nil
}

func (c *Connector) Provision(ctx context.Context, env *kit.Env) error {
	c.env = env
	c.db = env.NS.Database()
	var err error
	if c.admin, err = Open(ctx, env.Runner.Postgres, "testkit"); err != nil {
		return err
	}
	q, err := QuoteIdent(c.db)
	if err != nil {
		return err
	}
	if _, err := c.admin.Exec(ctx, "CREATE DATABASE "+q); err != nil {
		return fmt.Errorf("create database %s: %w", c.db, err)
	}
	if c.Pool, err = Open(ctx, env.Runner.Postgres, c.db); err != nil {
		return err
	}
	st := env.Service.Stores.Postgres
	if st == nil || st.Migrations == "" {
		return nil
	}
	files, err := filepath.Glob(filepath.Join(env.Service.Path(st.Migrations), "*.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := c.Pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("migration %s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

func (c *Connector) Health(ctx context.Context) error {
	if c.Pool == nil {
		return fmt.Errorf("not provisioned")
	}
	return c.Pool.Ping(ctx)
}

// table resolves an entity name (or a raw table name) to a table + key.
func (c *Connector) table(entity string) (string, string, error) {
	if e, ok := c.env.Service.Entities[entity]; ok && e.Postgres != nil {
		return e.Postgres.Table, e.Postgres.Key, nil
	}
	return "", "", fmt.Errorf("entity %q has no postgres mapping in service %s", entity, c.env.Service.Name)
}

func (c *Connector) Apply(ctx context.Context, s kit.Step) (kit.Result, error) {
	switch s.Name {
	case "postgres.insert":
		tbl, _, err := c.table(kit.Str(s.With, "entity"))
		if err != nil {
			if t := kit.Str(s.With, "table"); t != "" {
				tbl, err = t, nil
			} else {
				return kit.Result{}, err
			}
		}
		rows, _ := s.With["rows"].([]any)
		n, err := c.insert(ctx, tbl, rows)
		return kit.Result{Output: map[string]any{"table": tbl, "rows": n}, Note: fmt.Sprintf("%d row(s) into %s", n, tbl)}, err
	case "postgres.exec":
		sql := kit.Str(s.With, "sql")
		args, _ := s.With["args"].([]any)
		tag, err := c.Pool.Exec(ctx, sql, args...)
		return kit.Result{Output: map[string]any{"rows_affected": tag.RowsAffected()}, Note: tag.String()}, err
	case "postgres.query":
		rows, err := c.queryRows(ctx, kit.Str(s.With, "sql"))
		return kit.Result{Output: rows}, err
	}
	return kit.Result{}, fmt.Errorf("postgres: unknown step %s", s.Name)
}

func (c *Connector) insert(ctx context.Context, table string, rows []any) (int, error) {
	qt, err := QuoteIdent(table)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			return n, fmt.Errorf("row %d is not a mapping", n+1)
		}
		cols := make([]string, 0, len(m))
		for k := range m {
			cols = append(cols, k)
		}
		sort.Strings(cols)
		qcols := make([]string, len(cols))
		ph := make([]string, len(cols))
		args := make([]any, len(cols))
		for i, k := range cols {
			if qcols[i], err = QuoteIdent(k); err != nil {
				return n, err
			}
			ph[i] = fmt.Sprintf("$%d", i+1)
			args[i] = sqlArg(m[k])
		}
		if _, err := c.Pool.Exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", qt, strings.Join(qcols, ", "), strings.Join(ph, ", ")), args...); err != nil {
			return n, fmt.Errorf("insert into %s: %w", table, err)
		}
		n++
	}
	return n, nil
}

func sqlArg(v any) any {
	switch x := v.(type) {
	case map[string]any, []any:
		b, _ := json.Marshal(x)
		return string(b)
	}
	return v
}

// Check resolves:
//
//	postgres.<entity>.<key>.<column>          value of one column
//	postgres.<entity>.<key>.exists            true when the row exists
//	postgres.<entity>.count(col=v, ...)       number of rows (optional filters)
//	postgres.<entity>.<key>                   the whole row (map)
func (c *Connector) Check(ctx context.Context, ref kit.CheckRef) (kit.Observation, error) {
	now := time.Now().UTC()
	segs := ref.Segments
	tbl, key, err := c.table(segs[1].Name)
	if err != nil {
		return kit.Observation{At: now}, err
	}
	qt, err := QuoteIdent(tbl)
	if err != nil {
		return kit.Observation{At: now}, err
	}
	if len(segs) == 3 && segs[2].Name == "count" {
		where, args, err := filters(segs[2].KV)
		if err != nil {
			return kit.Observation{At: now}, err
		}
		sql := "SELECT count(*) FROM " + qt + where
		var n int64
		err = c.Pool.QueryRow(ctx, sql, args...).Scan(&n)
		return kit.Observation{Value: n, Source: show(sql, args), At: time.Now().UTC()}, err
	}
	if len(segs) < 3 {
		return kit.Observation{At: now}, fmt.Errorf("expected postgres.<entity>.<key>[.<column>] or postgres.<entity>.count")
	}
	qk, err := QuoteIdent(key)
	if err != nil {
		return kit.Observation{At: now}, err
	}
	id := segs[2].Name
	sql := fmt.Sprintf("SELECT row_to_json(t) FROM %s t WHERE %s = $1", qt, qk)
	var raw []byte
	err = c.Pool.QueryRow(ctx, sql, id).Scan(&raw)
	at := time.Now().UTC()
	src := show(sql, []any{id})
	if err == pgx.ErrNoRows {
		if len(segs) == 4 && segs[3].Name == "exists" {
			return kit.Observation{Value: false, Source: src, At: at}, nil
		}
		return kit.Observation{Value: nil, Source: src, At: at}, nil
	}
	if err != nil {
		return kit.Observation{Source: src, At: at}, err
	}
	var row map[string]any
	_ = json.Unmarshal(raw, &row)
	if len(segs) == 3 {
		return kit.Observation{Value: row, Raw: row, Source: src, At: at}, nil
	}
	col := segs[3].Name
	if col == "exists" {
		return kit.Observation{Value: true, Raw: row, Source: src, At: at}, nil
	}
	v, ok := row[col]
	if !ok {
		return kit.Observation{Raw: row, Source: src, At: at}, fmt.Errorf("table %s has no column %q", tbl, col)
	}
	return kit.Observation{Value: v, Raw: row, Source: src + " → ." + col, At: at}, nil
}

func filters(kv map[string]string) (string, []any, error) {
	if len(kv) == 0 {
		return "", nil, nil
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	var args []any
	for i, k := range keys {
		q, err := QuoteIdent(k)
		if err != nil {
			return "", nil, err
		}
		parts = append(parts, fmt.Sprintf("%s::text = $%d", q, i+1))
		args = append(args, kv[k])
	}
	return " WHERE " + strings.Join(parts, " AND "), args, nil
}

func show(sql string, args []any) string {
	if len(args) == 0 {
		return sql
	}
	return fmt.Sprintf("%s  %v", sql, args)
}

func (c *Connector) queryRows(ctx context.Context, sql string) ([]map[string]any, error) {
	rows, err := c.Pool.Query(ctx, "SELECT row_to_json(t) FROM ("+sql+") t")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Collect dumps the snapshot tables of the service (DB state after the case).
func (c *Connector) Collect(ctx context.Context, _ kit.TimeWindow) ([]kit.Artifact, error) {
	if c.Pool == nil {
		return nil, nil
	}
	st := c.env.Service.Stores.Postgres
	var out []kit.Artifact
	for _, t := range st.Snapshot {
		q, err := QuoteIdent(t)
		if err != nil {
			return out, err
		}
		rows, err := c.queryRows(ctx, "SELECT * FROM "+q+" ORDER BY 1 LIMIT 1000")
		if err != nil {
			return out, fmt.Errorf("snapshot %s: %w", t, err)
		}
		rel := path.Join(c.env.CaseDir, "output", "postgres", t+".json")
		if _, err := c.env.Evidence.WriteJSON(rel, map[string]any{"table": t, "database": c.db, "rows": rows,
			"taken_at": time.Now().UTC()}); err != nil {
			return out, err
		}
		out = append(out, kit.Artifact{Kind: "snapshot", Path: rel, Title: fmt.Sprintf("Postgres %s (%d rows)", t, len(rows)), Source: "postgres"})
	}
	return out, nil
}

func (c *Connector) Teardown(ctx context.Context) error {
	if c.Pool != nil {
		c.Pool.Close()
	}
	if c.admin == nil {
		return nil
	}
	defer c.admin.Close()
	q, err := QuoteIdent(c.db)
	if err != nil {
		return err
	}
	_, err = c.admin.Exec(ctx, "DROP DATABASE IF EXISTS "+q+" WITH (FORCE)")
	return err
}
