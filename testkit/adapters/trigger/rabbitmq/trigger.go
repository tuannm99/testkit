package rabbitmq

import (
	"context"
	"fmt"
	"time"

	"github.com/tuannm99/testkit/testkit/core/kit"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

// Trigger is "trigger:rabbitmq".
type Trigger struct {
	env   *kit.Env
	c     *Client
	queue string
	dlq   string
	body  string
	id    string
}

func NewTrigger() kit.Connector { return &Trigger{} }

func (t *Trigger) Name() string { return "trigger:rabbitmq" }

func (t *Trigger) Provision(_ context.Context, env *kit.Env) error {
	spec, ok := env.Service.Triggers["rabbitmq"]
	if !ok {
		return fmt.Errorf("service %s declares no rabbitmq trigger", env.Service.Name)
	}
	t.env, t.queue, t.body, t.id = env, spec.Queue, spec.Value, spec.Key
	for _, q := range env.Service.Stores.RabbitMQ.Queues {
		if q.Name == spec.Queue {
			t.dlq = q.DLQ
		}
	}
	t.c = newClient(env, string(env.NS))
	return t.c.dial() // the vhost and queues are created by the rabbitmq connector (provisioned first)
}

func (t *Trigger) Health(ctx context.Context) error {
	_, _, err := t.c.ready(t.queue)
	return err
}

func (t *Trigger) Apply(context.Context, kit.Step) (kit.Result, error) {
	return kit.Result{}, fmt.Errorf("trigger:rabbitmq has no steps")
}

// Enqueue publishes the job (Duplicate times: duplicated delivery).
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
	return t.c.publish(ctx, t.queue, []byte(fmt.Sprint(body)), nil, fmt.Sprint(id), j.Duplicate)
}

// Drain waits until the queue is empty and nothing is in flight.
func (t *Trigger) Drain(ctx context.Context) error { return t.c.drain(ctx, t.queue) }

// Backlog is ready + unacked messages of the queue.
func (t *Trigger) Backlog(ctx context.Context) (int64, error) {
	ready, unacked, _, err := t.c.counts(ctx, t.queue)
	return ready + unacked, err
}

// DeadLetters is the number of messages in the queue's declared DLQ.
func (t *Trigger) DeadLetters(context.Context) (int64, error) {
	if t.dlq == "" {
		return 0, fmt.Errorf("queue %s declares no dlq", t.queue)
	}
	n, _, err := t.c.ready(t.dlq)
	return n, err
}

func (t *Trigger) Collect(context.Context, kit.TimeWindow) ([]kit.Artifact, error) { return nil, nil }

func (t *Trigger) Teardown(context.Context) error {
	t.c.close()
	return nil
}

var _ kit.TriggerConnector = (*Trigger)(nil)

var _ kit.TriggerState = (*Trigger)(nil)
