// Package redis is the Redis connector: keys are isolated by the namespace
// prefix ("<ns>:"), which the service receives in its env. Checks read
// values/TTLs under the prefix; teardown deletes the prefix.
package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/tuannm99/testkit/testkit/core/kit"
)

type Connector struct {
	env    *kit.Env
	cl     *goredis.Client
	prefix string
	queues map[string]*Queue
}

func New() kit.Connector { return &Connector{} }

func (c *Connector) Name() string            { return "redis" }
func (c *Connector) CheckPrefixes() []string { return []string{"redis"} }

func (c *Connector) Provision(ctx context.Context, env *kit.Env) error {
	c.env, c.prefix = env, env.NS.KeyPrefix()
	c.cl = goredis.NewClient(&goredis.Options{Addr: env.Runner.Redis})
	if err := c.cl.Ping(ctx).Err(); err != nil {
		return err
	}
	c.queues = map[string]*Queue{}
	for _, spec := range env.Service.Stores.Redis.Queues {
		if spec.Processing == "" {
			spec.Processing = spec.Name + ":processing"
		}
		q := NewQueue(c.cl, c.prefix, spec)
		if err := q.Prepare(ctx); err != nil {
			return err
		}
		c.queues[spec.Name] = q
	}
	return nil
}

func (c *Connector) Health(ctx context.Context) error { return c.cl.Ping(ctx).Err() }

func (c *Connector) Apply(ctx context.Context, s kit.Step) (kit.Result, error) {
	if s.Name == "redis.enqueue" {
		q, ok := c.queues[kit.Str(s.With, "queue")]
		if !ok {
			return kit.Result{}, fmt.Errorf("redis.enqueue: queue %q is not declared in stores.redis.queues", kit.Str(s.With, "queue"))
		}
		var body string
		switch v := s.With["value"].(type) {
		case string:
			body = v
		default:
			raw, _ := json.Marshal(v)
			body = string(raw)
		}
		n := kit.Int(s.With, "count", 1)
		return kit.Result{Note: fmt.Sprintf("%d message(s) to %s", n, q.key)}, q.Enqueue(ctx, body, kit.Str(s.With, "id"), n)
	}
	key := c.prefix + kit.Str(s.With, "key")
	switch s.Name {
	case "redis.set":
		err := c.cl.Set(ctx, key, kit.Str(s.With, "value"), kit.Dur(s.With, "ttl", 0)).Err()
		return kit.Result{Note: "SET " + key}, err
	case "redis.del":
		return kit.Result{Note: "DEL " + key}, c.cl.Del(ctx, key).Err()
	case "redis.expire":
		return kit.Result{Note: "EXPIRE " + key}, c.cl.Expire(ctx, key, kit.Dur(s.With, "ttl", time.Second)).Err()
	}
	return kit.Result{}, fmt.Errorf("redis: unknown step %s", s.Name)
}

// value reads a key whatever its type.
func (c *Connector) value(ctx context.Context, key string) (any, string, error) {
	typ, err := c.cl.Type(ctx, key).Result()
	if err != nil {
		return nil, "", err
	}
	switch typ {
	case "none":
		return nil, typ, nil
	case "string":
		v, err := c.cl.Get(ctx, key).Result()
		return v, typ, err
	case "hash":
		v, err := c.cl.HGetAll(ctx, key).Result()
		return v, typ, err
	case "list":
		v, err := c.cl.LRange(ctx, key, 0, -1).Result()
		return v, typ, err
	case "set":
		v, err := c.cl.SMembers(ctx, key).Result()
		return v, typ, err
	case "stream":
		msgs, err := c.cl.XRange(ctx, key, "-", "+").Result()
		out := make([]map[string]any, len(msgs))
		for i, m := range msgs {
			out[i] = map[string]any{"id": m.ID, "values": m.Values}
		}
		return out, typ, err
	case "zset":
		v, err := c.cl.ZRangeWithScores(ctx, key, 0, -1).Result()
		return v, typ, err
	}
	return nil, typ, nil
}

// Check resolves (keys are relative to the namespace prefix):
//
//	redis.key(<key>)        value (string, hash, list, set, zset); null when absent
//	redis.exists(<key>)     1 or 0
//	redis.ttl(<key>)        seconds to live (-1 no expiry, -2 absent)
//	redis.count(<pattern>)  keys matching the pattern
//	redis.queue(<name>).depth|waiting|pending|total|dlq|messages   declared queues (see queueCheck)
func (c *Connector) Check(ctx context.Context, ref kit.CheckRef) (kit.Observation, error) {
	s := ref.Segments[1]
	at := time.Now().UTC()
	if s.Name == "queue" {
		return c.queueCheck(ctx, ref)
	}
	if len(s.Args) != 1 {
		return kit.Observation{At: at}, fmt.Errorf("expected redis.%s(<key>)", s.Name)
	}
	key := c.prefix + s.Args[0]
	switch s.Name {
	case "key":
		v, typ, err := c.value(ctx, key)
		return kit.Observation{Value: v, Source: fmt.Sprintf("Redis %s (%s)", key, typ), At: time.Now().UTC()}, err
	case "exists":
		n, err := c.cl.Exists(ctx, key).Result()
		return kit.Observation{Value: n, Source: "EXISTS " + key, At: time.Now().UTC()}, err
	case "ttl":
		d, err := c.cl.TTL(ctx, key).Result()
		v := int64(d / time.Second)
		if d < 0 {
			v = int64(d)
		}
		return kit.Observation{Value: v, Source: "TTL " + key, At: time.Now().UTC()}, err
	case "count":
		keys, err := c.scan(ctx, key)
		return kit.Observation{Value: len(keys), Raw: keys, Source: "SCAN MATCH " + key, At: time.Now().UTC()}, err
	}
	return kit.Observation{At: at}, fmt.Errorf("unknown redis check %q", s.Name)
}

func (c *Connector) scan(ctx context.Context, pattern string) ([]string, error) {
	var out []string
	iter := c.cl.Scan(ctx, 0, pattern, 500).Iterator()
	for iter.Next(ctx) {
		out = append(out, iter.Val())
	}
	return out, iter.Err()
}

// queueCheck resolves redis.queue(<name>).<property> for a declared queue:
//
//	waiting   not yet delivered to a consumer
//	pending   delivered, not yet acknowledged (stream PEL / list processing)
//	depth     waiting + pending
//	total     every entry in the stream (acknowledged included) / list length
//	dlq       entries in the dead-letter queue
//	messages  bodies of the dead-letter queue
func (c *Connector) queueCheck(ctx context.Context, ref kit.CheckRef) (kit.Observation, error) {
	at := time.Now().UTC()
	seg := ref.Segments[1]
	if len(ref.Segments) != 3 || len(seg.Args) != 1 {
		return kit.Observation{At: at}, fmt.Errorf("expected redis.queue(<name>).<depth|waiting|pending|total|dlq|messages>")
	}
	q, ok := c.queues[seg.Args[0]]
	if !ok {
		return kit.Observation{At: at}, fmt.Errorf("queue %q is not declared in stores.redis.queues", seg.Args[0])
	}
	prop := ref.Segments[2].Name
	src := fmt.Sprintf("Redis %s %s (%s)", q.spec.Kind, q.key, prop)
	var v any
	var raw any
	var err error
	switch prop {
	case "waiting":
		v, err = q.Waiting(ctx)
	case "pending":
		v, err = q.Pending(ctx)
	case "depth":
		v, err = q.Depth(ctx)
	case "total":
		v, err = q.Total(ctx)
	case "dlq":
		v, _, err = q.DeadLetters(ctx)
	case "messages":
		var msgs []any
		_, msgs, err = q.DeadLetters(ctx)
		v, raw = msgs, msgs
	default:
		return kit.Observation{At: at}, fmt.Errorf("unknown redis queue property %q", prop)
	}
	return kit.Observation{Value: v, Raw: raw, Source: src, At: time.Now().UTC()}, err
}

// Collect dumps every key of the namespace.
func (c *Connector) Collect(ctx context.Context, _ kit.TimeWindow) ([]kit.Artifact, error) {
	if c.cl == nil {
		return nil, nil
	}
	keys, err := c.scan(ctx, c.prefix+"*")
	if err != nil {
		return nil, err
	}
	var dump []map[string]any
	for _, k := range keys {
		v, typ, _ := c.value(ctx, k)
		ttl, _ := c.cl.TTL(ctx, k).Result()
		dump = append(dump, map[string]any{"key": strings.TrimPrefix(k, c.prefix), "type": typ, "value": v, "ttl_seconds": int64(ttl / time.Second)})
	}
	rel := path.Join(c.env.CaseDir, "output", "redis", "keys.json")
	if _, err := c.env.Evidence.WriteJSON(rel, map[string]any{"prefix": c.prefix, "keys": dump}); err != nil {
		return nil, err
	}
	return []kit.Artifact{{Kind: "snapshot", Path: rel, Title: fmt.Sprintf("Redis keys under %s (%d)", c.prefix, len(dump)), Source: "redis"}}, nil
}

func (c *Connector) Teardown(ctx context.Context) error {
	if c.cl == nil {
		return nil
	}
	defer c.cl.Close()
	keys, err := c.scan(ctx, c.prefix+"*")
	if err != nil || len(keys) == 0 {
		return err
	}
	return c.cl.Del(ctx, keys...).Err()
}

var (
	_ kit.Connector = (*Connector)(nil)
	_ kit.Checker   = (*Connector)(nil)
)
