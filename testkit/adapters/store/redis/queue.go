package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	goredis "github.com/redis/go-redis/v9"

	"github.com/tuannm99/testkit/testkit/core/config"
)

// Queue is a Redis stream or list declared in stores.redis.queues, under the
// namespace prefix. A stream is consumed through a consumer group; a list
// follows the reliable pattern (BLMOVE into a processing list, remove after
// handling), so "in flight" is visible in both.
type Queue struct {
	cl     *goredis.Client
	spec   config.RedisQueue
	key    string
	proc   string
	dlqKey string
}

// NewQueue binds a declared queue to a client and the namespace prefix.
func NewQueue(cl *goredis.Client, prefix string, spec config.RedisQueue) *Queue {
	q := &Queue{cl: cl, spec: spec, key: prefix + spec.Name, proc: prefix + spec.Processing}
	if spec.DLQ != "" {
		q.dlqKey = prefix + spec.DLQ
	}
	return q
}

// Prepare creates a stream's consumer group at the beginning (and the stream),
// so jobs enqueued before the service starts are delivered to it. A service
// creating the same group gets BUSYGROUP, which it must tolerate.
func (q *Queue) Prepare(ctx context.Context) error {
	if q.spec.Kind != "stream" {
		return nil
	}
	err := q.cl.XGroupCreateMkStream(ctx, q.key, q.spec.Group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("create group %s on %s: %w", q.spec.Group, q.key, err)
	}
	return nil
}

// Enqueue adds n messages: a stream entry {payload, id} or a list element.
func (q *Queue) Enqueue(ctx context.Context, body, id string, n int) error {
	for i := 0; i < max(n, 1); i++ {
		var err error
		switch q.spec.Kind {
		case "stream":
			vals := map[string]any{"payload": body}
			if id != "" {
				vals["id"] = id
			}
			err = q.cl.XAdd(ctx, &goredis.XAddArgs{Stream: q.key, Values: vals}).Err()
		default:
			err = q.cl.LPush(ctx, q.key, body).Err()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Pending is what was delivered to a consumer and not yet acknowledged
// (stream: the group's pending entries; list: the processing list).
func (q *Queue) Pending(ctx context.Context) (int64, error) {
	if q.spec.Kind == "stream" {
		p, err := q.cl.XPending(ctx, q.key, q.spec.Group).Result()
		if err != nil {
			return 0, err
		}
		return p.Count, nil
	}
	return q.cl.LLen(ctx, q.proc).Result()
}

// Waiting is what no consumer received yet (stream: entries after the group's
// last delivered id; list: the queue itself). Counted from the entries rather
// than from the group's lag field, which Redis reports as unknown after XDEL.
func (q *Queue) Waiting(ctx context.Context) (int64, error) {
	if q.spec.Kind != "stream" {
		return q.cl.LLen(ctx, q.key).Result()
	}
	groups, err := q.cl.XInfoGroups(ctx, q.key).Result()
	if err != nil {
		return 0, err
	}
	last := "0-0"
	for _, g := range groups {
		if g.Name == q.spec.Group {
			last = g.LastDeliveredID
		}
	}
	var n int64
	start := "(" + last
	for {
		msgs, err := q.cl.XRangeN(ctx, q.key, start, "+", 1000).Result()
		if err != nil {
			return 0, err
		}
		n += int64(len(msgs))
		if len(msgs) < 1000 {
			return n, nil
		}
		start = "(" + msgs[len(msgs)-1].ID
	}
}

// Depth is waiting + pending: everything not finished.
func (q *Queue) Depth(ctx context.Context) (int64, error) {
	w, err := q.Waiting(ctx)
	if err != nil {
		return 0, err
	}
	p, err := q.Pending(ctx)
	return w + p, err
}

// Total is every entry in the stream (acknowledged ones included) or list.
func (q *Queue) Total(ctx context.Context) (int64, error) {
	if q.spec.Kind == "stream" {
		return q.cl.XLen(ctx, q.key).Result()
	}
	return q.cl.LLen(ctx, q.key).Result()
}

// DeadLetters returns the number and the bodies of the dead-letter queue.
func (q *Queue) DeadLetters(ctx context.Context) (int64, []any, error) {
	if q.dlqKey == "" {
		return 0, nil, fmt.Errorf("queue %s declares no dlq", q.spec.Name)
	}
	var raw []string
	if q.spec.Kind == "stream" {
		msgs, err := q.cl.XRange(ctx, q.dlqKey, "-", "+").Result()
		if err != nil {
			return 0, nil, err
		}
		for _, m := range msgs {
			raw = append(raw, fmt.Sprint(m.Values["payload"]))
		}
	} else {
		var err error
		if raw, err = q.cl.LRange(ctx, q.dlqKey, 0, -1).Result(); err != nil {
			return 0, nil, err
		}
	}
	out := make([]any, len(raw))
	for i, r := range raw {
		out[i] = decode(r)
	}
	return int64(len(out)), out, nil
}

// Drained reports nothing waiting and nothing in flight.
func (q *Queue) Drained(ctx context.Context) (bool, string, error) {
	w, err := q.Waiting(ctx)
	if err != nil {
		return false, "", err
	}
	p, err := q.Pending(ctx)
	if err != nil {
		return false, "", err
	}
	return w == 0 && p == 0, fmt.Sprintf("waiting %d, pending %d", w, p), nil
}

func decode(s string) any {
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		return v
	}
	return s
}
