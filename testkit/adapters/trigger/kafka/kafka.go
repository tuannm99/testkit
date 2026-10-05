// Package kafka holds the Kafka connector (namespaced topics, produce,
// checks on topics and consumer lag, topic dumps as evidence) and the Kafka
// trigger (enqueue a job as a record; drain = consumer lag 0).
package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tuannm99/testkit/testkit/core/kit"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

// Client wraps a franz-go client + admin client for one namespace.
type Client struct {
	cl  *kgo.Client
	adm *kadm.Client
	ns  kit.Namespace
}

func dial(brokers []string) (*Client, error) {
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerLinger(0), kgo.RecordRetries(5))
	if err != nil {
		return nil, err
	}
	return &Client{cl: cl, adm: kadm.NewClient(cl)}, nil
}

func (c *Client) close() {
	if c != nil && c.cl != nil {
		c.cl.Close()
	}
}

// Produce writes records synchronously.
func (c *Client) Produce(ctx context.Context, topic string, key, value []byte, headers map[string]string, n int) error {
	var recs []*kgo.Record
	for i := 0; i < max(n, 1); i++ {
		r := &kgo.Record{Topic: topic, Key: key, Value: value}
		for k, v := range headers {
			r.Headers = append(r.Headers, kgo.RecordHeader{Key: k, Value: []byte(v)})
		}
		recs = append(recs, r)
	}
	return c.cl.ProduceSync(ctx, recs...).FirstErr()
}

// Count returns the number of records currently in a topic.
func (c *Client) Count(ctx context.Context, topic string) (int64, error) {
	start, err := c.adm.ListStartOffsets(ctx, topic)
	if err != nil {
		return 0, err
	}
	end, err := c.adm.ListEndOffsets(ctx, topic)
	if err != nil {
		return 0, err
	}
	var n int64
	var ferr error
	end.Each(func(o kadm.ListedOffset) {
		if o.Err != nil {
			ferr = o.Err
			return
		}
		s, _ := start.Lookup(o.Topic, o.Partition)
		n += o.Offset - s.Offset
	})
	return n, ferr
}

// Message is a dumped record.
type Message struct {
	Partition int32             `json:"partition"`
	Offset    int64             `json:"offset"`
	Timestamp time.Time         `json:"timestamp"`
	Key       string            `json:"key"`
	Value     any               `json:"value"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// Read returns up to limit records of a topic from the beginning.
func (c *Client) Read(ctx context.Context, brokers []string, topic string, limit int) ([]Message, error) {
	total, err := c.Count(ctx, topic)
	if err != nil || total == 0 {
		return nil, err
	}
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()), kgo.FetchMaxWait(200*time.Millisecond))
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	want := min(int(total), limit)
	var out []Message
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for len(out) < want && rctx.Err() == nil {
		f := cl.PollRecords(rctx, want-len(out))
		f.EachRecord(func(r *kgo.Record) {
			m := Message{Partition: r.Partition, Offset: r.Offset, Timestamp: r.Timestamp.UTC(), Key: string(r.Key)}
			var v any
			if json.Unmarshal(r.Value, &v) == nil {
				m.Value = v
			} else {
				m.Value = string(r.Value)
			}
			if len(r.Headers) > 0 {
				m.Headers = map[string]string{}
				for _, h := range r.Headers {
					m.Headers[h.Key] = string(h.Value)
				}
			}
			out = append(out, m)
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Partition != out[j].Partition {
			return out[i].Partition < out[j].Partition
		}
		return out[i].Offset < out[j].Offset
	})
	return out, nil
}

// Lag returns the total lag of a group on a topic. A group that committed
// nothing yet lags by the whole topic.
func (c *Client) Lag(ctx context.Context, group, topic string) (int64, error) {
	end, err := c.adm.ListEndOffsets(ctx, topic)
	if err != nil {
		return 0, err
	}
	committed, err := c.adm.FetchOffsets(ctx, group)
	if err != nil {
		return 0, err
	}
	var lag int64
	end.Each(func(o kadm.ListedOffset) {
		cur := int64(0)
		if co, ok := committed.Lookup(o.Topic, o.Partition); ok && co.At >= 0 {
			cur = co.At
		} else {
			// No commit: lag from the start offset.
			cur = -1
		}
		if cur < 0 {
			lag += o.Offset
			return
		}
		lag += o.Offset - cur
	})
	return lag, nil
}

// Connector is the Kafka store connector ("kafka").
type Connector struct {
	env *kit.Env
	c   *Client
}

func New() kit.Connector { return &Connector{} }

func (k *Connector) Name() string            { return "kafka" }
func (k *Connector) CheckPrefixes() []string { return []string{"kafka"} }

func (k *Connector) Provision(ctx context.Context, env *kit.Env) error {
	k.env = env
	var err error
	if k.c, err = dial(env.Runner.KafkaBrokers); err != nil {
		return err
	}
	k.c.ns = env.NS
	for _, t := range env.Service.Stores.Kafka.Topics {
		p := t.Partitions
		if p <= 0 {
			p = 1
		}
		res, err := k.c.adm.CreateTopic(ctx, int32(p), 1, nil, env.NS.Topic(t.Name))
		if err != nil {
			return fmt.Errorf("create topic %s: %w", env.NS.Topic(t.Name), err)
		}
		if res.Err != nil {
			return fmt.Errorf("create topic %s: %w", env.NS.Topic(t.Name), res.Err)
		}
	}
	return nil
}

func (k *Connector) Health(ctx context.Context) error {
	_, err := k.c.adm.BrokerMetadata(ctx)
	return err
}

func (k *Connector) Apply(ctx context.Context, s kit.Step) (kit.Result, error) {
	switch s.Name {
	case "kafka.produce":
		topic := k.env.NS.Topic(kit.Str(s.With, "topic"))
		value := s.With["value"]
		var raw []byte
		switch v := value.(type) {
		case string:
			raw = []byte(v)
		default:
			raw, _ = json.Marshal(v)
		}
		hdr := map[string]string{}
		if h, ok := s.With["headers"].(map[string]any); ok {
			for kk, v := range h {
				hdr[kk] = fmt.Sprint(v)
			}
		}
		n := kit.Int(s.With, "count", 1)
		err := k.c.Produce(ctx, topic, []byte(kit.Str(s.With, "key")), raw, hdr, n)
		return kit.Result{Note: fmt.Sprintf("%d record(s) to %s", n, topic)}, err
	}
	return kit.Result{}, fmt.Errorf("kafka: unknown step %s", s.Name)
}

// Check resolves:
//
//	kafka.<topic>.count | kafka.topic(<topic.with.dots>).count   records in topic
//	kafka.<topic>.count(key=o1)                                   records with that key
//	kafka.lag(<group>)                                            lag on the trigger topic
//	kafka.<topic>.messages                                        values (list)
func (k *Connector) Check(ctx context.Context, ref kit.CheckRef) (kit.Observation, error) {
	now := time.Now().UTC()
	segs := ref.Segments
	if segs[1].Name == "lag" {
		group := k.env.NS.Group(firstArg(segs[1], ""))
		topic := k.env.NS.Topic(k.env.Service.Triggers["kafka"].Topic)
		if len(segs[1].Args) > 1 {
			topic = k.env.NS.Topic(segs[1].Args[1])
		}
		lag, err := k.c.Lag(ctx, group, topic)
		return kit.Observation{Value: lag, Source: fmt.Sprintf("lag of group %s on %s", group, topic), At: time.Now().UTC()}, err
	}
	topicName := segs[1].Name
	if topicName == "topic" {
		topicName = firstArg(segs[1], "")
	}
	topic := k.env.NS.Topic(topicName)
	if len(segs) < 3 {
		return kit.Observation{At: now}, fmt.Errorf("expected kafka.<topic>.count|messages")
	}
	switch segs[2].Name {
	case "count":
		if len(segs[2].KV) == 0 {
			n, err := k.c.Count(ctx, topic)
			return kit.Observation{Value: n, Source: "record count of " + topic, At: time.Now().UTC()}, err
		}
		msgs, err := k.c.Read(ctx, k.env.Runner.KafkaBrokers, topic, 10000)
		if err != nil {
			return kit.Observation{At: now}, err
		}
		n := 0
		for _, m := range msgs {
			if matches(m, segs[2].KV) {
				n++
			}
		}
		return kit.Observation{Value: n, Source: fmt.Sprintf("records of %s matching %v", topic, segs[2].KV), At: time.Now().UTC()}, nil
	case "messages":
		msgs, err := k.c.Read(ctx, k.env.Runner.KafkaBrokers, topic, 1000)
		vals := make([]any, len(msgs))
		for i, m := range msgs {
			vals[i] = m.Value
		}
		return kit.Observation{Value: vals, Raw: msgs, Source: "records of " + topic, At: time.Now().UTC()}, err
	}
	return kit.Observation{At: now}, fmt.Errorf("unknown kafka check %q", segs[2].Name)
}

func matches(m Message, kv map[string]string) bool {
	for key, want := range kv {
		switch key {
		case "key":
			if m.Key != want {
				return false
			}
		default:
			obj, ok := m.Value.(map[string]any)
			if !ok || fmt.Sprint(obj[key]) != want {
				return false
			}
		}
	}
	return true
}

func firstArg(s kit.Segment, def string) string {
	if len(s.Args) > 0 {
		return s.Args[0]
	}
	return def
}

// Collect dumps every namespace topic and the consumer group offsets.
func (k *Connector) Collect(ctx context.Context, _ kit.TimeWindow) ([]kit.Artifact, error) {
	if k.c == nil {
		return nil, nil
	}
	var out []kit.Artifact
	for _, t := range k.env.Service.Stores.Kafka.Topics {
		topic := k.env.NS.Topic(t.Name)
		msgs, err := k.c.Read(ctx, k.env.Runner.KafkaBrokers, topic, 1000)
		if err != nil {
			return out, err
		}
		rel := path.Join(k.env.CaseDir, "output", "kafka", t.Name+".json")
		if _, err := k.env.Evidence.WriteJSON(rel, map[string]any{"topic": topic, "records": msgs}); err != nil {
			return out, err
		}
		out = append(out, kit.Artifact{Kind: "topic", Path: rel, Title: fmt.Sprintf("Kafka %s (%d records)", t.Name, len(msgs)), Source: "kafka"})
	}
	lags := map[string]any{}
	for _, g := range k.env.Service.Stores.Kafka.Groups {
		group := k.env.NS.Group(g)
		if tr, ok := k.env.Service.Triggers["kafka"]; ok {
			lag, err := k.c.Lag(ctx, group, k.env.NS.Topic(tr.Topic))
			lags[group] = map[string]any{"lag": lag, "error": errStr(err)}
		}
	}
	if len(lags) > 0 {
		rel := path.Join(k.env.CaseDir, "output", "kafka", "consumer-groups.json")
		if _, err := k.env.Evidence.WriteJSON(rel, lags); err == nil {
			out = append(out, kit.Artifact{Kind: "lag", Path: rel, Title: "Kafka consumer lag at the end of the case", Source: "kafka"})
		}
	}
	return out, nil
}

func errStr(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

func (k *Connector) Teardown(ctx context.Context) error {
	if k.c == nil {
		return nil
	}
	defer k.c.close()
	var topics []string
	for _, t := range k.env.Service.Stores.Kafka.Topics {
		topics = append(topics, k.env.NS.Topic(t.Name))
	}
	var groups []string
	for _, g := range k.env.Service.Stores.Kafka.Groups {
		groups = append(groups, k.env.NS.Group(g))
	}
	if len(groups) > 0 {
		_, _ = k.c.adm.DeleteGroups(ctx, groups...)
	}
	_, err := k.c.adm.DeleteTopics(ctx, topics...)
	return err
}

// Trigger delivers jobs as Kafka records ("trigger:kafka").
type Trigger struct {
	env  *kit.Env
	c    *Client
	spec struct{ topic, group, key, value string }
}

func NewTrigger() kit.Connector { return &Trigger{} }

func (t *Trigger) Name() string { return "trigger:kafka" }

func (t *Trigger) Provision(ctx context.Context, env *kit.Env) error {
	t.env = env
	ts, ok := env.Service.Triggers["kafka"]
	if !ok {
		return fmt.Errorf("service %s declares no kafka trigger", env.Service.Name)
	}
	t.spec.topic, t.spec.group, t.spec.key, t.spec.value = env.NS.Topic(ts.Topic), env.NS.Group(ts.Group), ts.Key, ts.Value
	var err error
	t.c, err = dial(env.Runner.KafkaBrokers)
	return err
}

func (t *Trigger) Health(ctx context.Context) error {
	_, err := t.c.adm.BrokerMetadata(ctx)
	return err
}

func (t *Trigger) Apply(context.Context, kit.Step) (kit.Result, error) {
	return kit.Result{}, fmt.Errorf("trigger:kafka has no steps")
}

func jobData(env *kit.Env, j kit.Job) map[string]any {
	fields := map[string]any{"id": j.ID}
	for k, v := range j.Fields {
		fields[k] = v
	}
	return map[string]any{"job": fields, "ns": string(env.NS), "run_id": env.RunID, "vars": env.Vars,
		"now": time.Now().UTC().Format(time.RFC3339Nano)}
}

func (t *Trigger) Enqueue(ctx context.Context, j kit.Job) error {
	data := jobData(t.env, j)
	key, err := scenario.RenderString(t.spec.key, data)
	if err != nil {
		return err
	}
	val, err := scenario.RenderString(t.spec.value, data)
	if err != nil {
		return err
	}
	n := max(j.Duplicate, 1)
	return t.c.Produce(ctx, t.spec.topic, []byte(fmt.Sprint(key)), []byte(fmt.Sprint(val)), nil, n)
}

// Drain waits until the service's consumer group has committed everything.
func (t *Trigger) Drain(ctx context.Context) error {
	var lag int64 = -1
	var err error
	for {
		lag, err = t.c.Lag(ctx, t.spec.group, t.spec.topic)
		if err == nil && lag == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("kafka drain: group %s still lags by %d on %s (%v)", t.spec.group, lag, t.spec.topic, err)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (t *Trigger) Collect(context.Context, kit.TimeWindow) ([]kit.Artifact, error) { return nil, nil }

func (t *Trigger) Teardown(context.Context) error {
	t.c.close()
	return nil
}

// Interface assertions.
var (
	_ kit.Connector        = (*Connector)(nil)
	_ kit.Checker          = (*Connector)(nil)
	_ kit.TriggerConnector = (*Trigger)(nil)
)
