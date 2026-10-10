// Package redisq is the Redis trigger ("trigger:redis"): a job is a stream
// entry or a list element in a queue declared under stores.redis.queues, in
// the namespace's key prefix. Drained means nothing waits and nothing is in
// flight (stream: no entry after the group's last delivered id and an empty
// pending list; list: empty queue and empty processing list).
package redisq

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/tuannm99/testkit/testkit/adapters/store/redis"
	"github.com/tuannm99/testkit/testkit/core/kit"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

type Trigger struct {
	env  *kit.Env
	cl   *goredis.Client
	q    *redis.Queue
	body string
	id   string
}

func New() kit.Connector { return &Trigger{} }

func (t *Trigger) Name() string { return "trigger:redis" }

func (t *Trigger) Provision(ctx context.Context, env *kit.Env) error {
	spec, ok := env.Service.Triggers["redis"]
	if !ok {
		return fmt.Errorf("service %s declares no redis trigger", env.Service.Name)
	}
	qs, ok := env.Service.RedisQueueOf(spec.Queue)
	if !ok {
		return fmt.Errorf("redis trigger queue %q is not declared in stores.redis.queues", spec.Queue)
	}
	t.env, t.body, t.id = env, spec.Value, spec.Key
	t.cl = goredis.NewClient(&goredis.Options{Addr: env.Runner.Redis})
	t.q = redis.NewQueue(t.cl, env.NS.KeyPrefix(), qs)
	return t.cl.Ping(ctx).Err() // the group is created by the redis connector (provisioned first)
}

func (t *Trigger) Health(ctx context.Context) error { return t.cl.Ping(ctx).Err() }

func (t *Trigger) Apply(context.Context, kit.Step) (kit.Result, error) {
	return kit.Result{}, fmt.Errorf("trigger:redis has no steps")
}

// Enqueue adds the job (Duplicate times: duplicated delivery).
func (t *Trigger) Enqueue(ctx context.Context, j kit.Job) error {
	fields := map[string]any{"id": j.ID}
	for k, v := range j.Fields {
		fields[k] = v
	}
	data := map[string]any{"job": fields, "ns": string(t.env.NS), "run_id": t.env.RunID, "vars": t.env.Vars,
		"now": time.Now().UTC().Format(time.RFC3339Nano)}
	body, err := scenario.RenderString(t.body, data)
	if err != nil {
		return err
	}
	var id any = ""
	if t.id != "" {
		if id, err = scenario.RenderString(t.id, data); err != nil {
			return err
		}
	}
	return t.q.Enqueue(ctx, fmt.Sprint(body), fmt.Sprint(id), j.Duplicate)
}

// Drain polls until nothing waits and nothing is in flight.
func (t *Trigger) Drain(ctx context.Context) error {
	var last string
	for {
		ok, state, err := t.q.Drained(ctx)
		if err == nil && ok {
			return nil
		}
		last = state
		if err != nil {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("redis drain: queue not drained (%s)", last)
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (t *Trigger) Collect(context.Context, kit.TimeWindow) ([]kit.Artifact, error) { return nil, nil }

func (t *Trigger) Teardown(context.Context) error {
	if t.cl != nil {
		return t.cl.Close()
	}
	return nil
}

var _ kit.TriggerConnector = (*Trigger)(nil)
