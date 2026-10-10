// Package dbpoll is the DB-poll trigger: a job is a row inserted into the
// service's job table (SQL template from the descriptor); drained means the
// descriptor's `drained` query returns 0 (no queued/running job left).
package dbpoll

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tuannm99/testkit/testkit/adapters/store/postgres"
	"github.com/tuannm99/testkit/testkit/core/kit"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

type Trigger struct {
	env     *kit.Env
	pool    *pgxpool.Pool
	sql     string
	drained string
	dead    string
}

func New() kit.Connector { return &Trigger{} }

func (t *Trigger) Name() string { return "trigger:db-poll" }

func (t *Trigger) Provision(ctx context.Context, env *kit.Env) error {
	t.env = env
	ts, ok := env.Service.Triggers["db-poll"]
	if !ok {
		return fmt.Errorf("service %s declares no db-poll trigger", env.Service.Name)
	}
	t.sql, t.drained, t.dead = ts.SQL, ts.Drained, ts.Dead
	var err error
	t.pool, err = postgres.Open(ctx, env.Runner.Postgres, env.NS.Database())
	return err
}

func (t *Trigger) Health(ctx context.Context) error { return t.pool.Ping(ctx) }

func (t *Trigger) Apply(context.Context, kit.Step) (kit.Result, error) {
	return kit.Result{}, fmt.Errorf("trigger:db-poll has no steps")
}

// Enqueue inserts the job row (Duplicate times: duplicated delivery).
func (t *Trigger) Enqueue(ctx context.Context, j kit.Job) error {
	fields := map[string]any{"id": j.ID}
	for k, v := range j.Fields {
		fields[k] = v
	}
	sql, err := scenario.RenderString(t.sql, map[string]any{"job": fields, "ns": string(t.env.NS), "run_id": t.env.RunID,
		"vars": t.env.Vars, "now": time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return err
	}
	for i := 0; i < max(j.Duplicate, 1); i++ {
		if _, err := t.pool.Exec(ctx, fmt.Sprint(sql)); err != nil {
			return fmt.Errorf("enqueue job %s: %w", j.ID, err)
		}
	}
	return nil
}

// Drain polls the `drained` query until it returns 0.
func (t *Trigger) Drain(ctx context.Context) error {
	if t.drained == "" {
		return nil
	}
	var n int64 = -1
	var err error
	for {
		err = t.pool.QueryRow(ctx, t.drained).Scan(&n)
		if err == nil && n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("db-poll drain: %d job(s) not finished (%v)", n, err)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Backlog is the number of jobs the descriptor's `drained` query counts.
func (t *Trigger) Backlog(ctx context.Context) (int64, error) {
	if t.drained == "" {
		return 0, fmt.Errorf("triggers.db-poll declares no drained query")
	}
	var n int64
	err := t.pool.QueryRow(ctx, t.drained).Scan(&n)
	return n, err
}

// DeadLetters is the number of jobs the descriptor's `dead` query counts.
func (t *Trigger) DeadLetters(ctx context.Context) (int64, error) {
	if t.dead == "" {
		return 0, fmt.Errorf("triggers.db-poll declares no dead query (SQL returning the number of jobs given up on)")
	}
	var n int64
	err := t.pool.QueryRow(ctx, t.dead).Scan(&n)
	return n, err
}

func (t *Trigger) Collect(context.Context, kit.TimeWindow) ([]kit.Artifact, error) { return nil, nil }

func (t *Trigger) Teardown(context.Context) error {
	if t.pool != nil {
		t.pool.Close()
	}
	return nil
}

var (
	_ kit.TriggerConnector = (*Trigger)(nil)
	_ kit.TriggerState     = (*Trigger)(nil)
)
