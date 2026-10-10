// Package rabbitmq holds the RabbitMQ store connector ("rabbitmq": one vhost
// per execution, declared queues, publish, depth checks, queue dumps as
// evidence) and the RabbitMQ trigger ("trigger:rabbitmq": enqueue a job as a
// persistent message; drain = nothing ready and nothing unacknowledged).
//
// Isolation is the vhost: it is named like the namespace and deleted at
// teardown, which also drops its queues and closes the service's connections.
// TestKit declares the queues (and their dead-letter queue); the service must
// consume them without re-declaring them with other arguments.
package rabbitmq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/tuannm99/testkit/testkit/core/config"
	"github.com/tuannm99/testkit/testkit/core/kit"
)

// statsStable is how long ready and unacked must both read zero before the
// queue counts as drained: the management API refreshes every 500 ms
// (compose sets collect_statistics_interval), so this is more than two refreshes.
const statsStable = 1300 * time.Millisecond

// Client talks AMQP (publish, exact ready count) and the management API
// (unacknowledged, rates, dumps) for one vhost.
type Client struct {
	ep    config.Endpoints
	vhost string
	http  *http.Client
	conn  *amqp.Connection
	ch    *amqp.Channel
	pubMu sync.Mutex // the publish channel is shared by concurrent Enqueue calls (load generators)
}

func newClient(ep config.Endpoints, vhost string) *Client {
	return &Client{ep: ep, vhost: vhost, http: &http.Client{Timeout: 15 * time.Second}}
}

func (c *Client) amqpURL() string {
	return fmt.Sprintf("amqp://%s:%s@%s/%s", url.PathEscape(c.ep.RabbitUser), url.PathEscape(c.ep.RabbitPass), c.ep.RabbitAMQP, url.PathEscape(c.vhost))
}

func (c *Client) dial() error {
	conn, err := amqp.Dial(c.amqpURL())
	if err != nil {
		return fmt.Errorf("rabbitmq: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return err
	}
	if err := ch.Confirm(false); err != nil {
		conn.Close()
		return err
	}
	c.conn, c.ch = conn, ch
	return nil
}

func (c *Client) close() {
	if c != nil && c.conn != nil {
		_ = c.conn.Close()
	}
}

// mgmt calls the management API.
func (c *Client) mgmt(ctx context.Context, method, p string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.ep.RabbitMgmt+p, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.ep.RabbitUser, c.ep.RabbitPass)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode >= 300 {
		return fmt.Errorf("rabbitmq management %s %s: HTTP %d: %.200s", method, p, res.StatusCode, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// QueueStats are the numbers of one queue.
type QueueStats struct {
	Name      string `json:"name"`
	Ready     int64  `json:"messages_ready"`
	Unacked   int64  `json:"messages_unacknowledged"`
	Consumers int64  `json:"consumers"`
	Stats     struct {
		Publish   int64 `json:"publish"`
		Ack       int64 `json:"ack"`
		Redeliver int64 `json:"redeliver"`
	} `json:"message_stats"`
}

func (c *Client) stats(ctx context.Context, queue string) (QueueStats, error) {
	var s QueueStats
	err := c.mgmt(ctx, http.MethodGet, "/api/queues/"+url.PathEscape(c.vhost)+"/"+url.PathEscape(queue), nil, &s)
	return s, err
}

// ready returns the exact number of ready messages and consumers through a
// passive declare on its own channel (a failed passive declare closes it).
func (c *Client) ready(queue string) (int64, int64, error) {
	ch, err := c.conn.Channel()
	if err != nil {
		return 0, 0, err
	}
	defer ch.Close()
	q, err := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
	if err != nil {
		return 0, 0, err
	}
	return int64(q.Messages), int64(q.Consumers), nil
}

// publish sends n persistent messages to a queue through the default
// exchange and waits for the broker's confirm of each.
func (c *Client) publish(ctx context.Context, queue string, body []byte, headers map[string]any, messageID string, n int) error {
	for i := 0; i < max(n, 1); i++ {
		c.pubMu.Lock()
		dc, err := c.ch.PublishWithDeferredConfirmWithContext(ctx, "", queue, false, false, amqp.Publishing{
			ContentType: "application/json", DeliveryMode: amqp.Persistent, Body: body, Headers: headers, MessageId: messageID,
			Timestamp: time.Now().UTC()})
		c.pubMu.Unlock()
		if err != nil {
			return err
		}
		ok, err := dc.WaitContext(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("rabbitmq: broker did not confirm the message")
		}
	}
	return nil
}

// drained waits until the queue holds nothing ready and nothing unacked, and
// stays that way for statsStable (so a stale management reading cannot
// declare a queue drained while a delivery is in flight).
func (c *Client) drain(ctx context.Context, queue string) error {
	var zeroSince time.Time
	var last string
	for {
		ready, _, err := c.ready(queue)
		var st QueueStats
		if err == nil {
			st, err = c.stats(ctx, queue)
		}
		if err == nil && ready == 0 && st.Unacked == 0 {
			if zeroSince.IsZero() {
				zeroSince = time.Now()
			}
			if time.Since(zeroSince) >= statsStable {
				return nil
			}
		} else {
			zeroSince = time.Time{}
			last = fmt.Sprintf("ready %d, unacked %d, err %v", ready, st.Unacked, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("rabbitmq drain: queue %s not drained (%s)", queue, last)
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// Connector is the RabbitMQ store connector ("rabbitmq").
type Connector struct {
	env *kit.Env
	c   *Client
}

func New() kit.Connector { return &Connector{} }

func (r *Connector) Name() string            { return "rabbitmq" }
func (r *Connector) CheckPrefixes() []string { return []string{"rabbitmq"} }

func (r *Connector) Provision(ctx context.Context, env *kit.Env) error {
	r.env = env
	r.c = newClient(env.Runner, string(env.NS))
	vh := url.PathEscape(r.c.vhost)
	if err := r.c.mgmt(ctx, http.MethodPut, "/api/vhosts/"+vh, map[string]any{}, nil); err != nil {
		return fmt.Errorf("create vhost %s: %w", r.c.vhost, err)
	}
	perm := map[string]string{"configure": ".*", "write": ".*", "read": ".*"}
	if err := r.c.mgmt(ctx, http.MethodPut, "/api/permissions/"+vh+"/"+url.PathEscape(env.Runner.RabbitUser), perm, nil); err != nil {
		return err
	}
	if err := r.c.dial(); err != nil {
		return err
	}
	for _, q := range env.Service.Stores.RabbitMQ.Queues {
		var args amqp.Table
		if q.DLQ != "" {
			if _, err := r.c.ch.QueueDeclare(q.DLQ, true, false, false, false, nil); err != nil {
				return fmt.Errorf("declare %s: %w", q.DLQ, err)
			}
			args = amqp.Table{"x-dead-letter-exchange": "", "x-dead-letter-routing-key": q.DLQ}
		}
		if _, err := r.c.ch.QueueDeclare(q.Name, true, false, false, false, args); err != nil {
			return fmt.Errorf("declare %s: %w", q.Name, err)
		}
	}
	return nil
}

func (r *Connector) Health(ctx context.Context) error {
	return r.c.mgmt(ctx, http.MethodGet, "/api/overview", nil, nil)
}

// Apply: rabbitmq.publish {queue, body, count?, message_id?, headers?}.
func (r *Connector) Apply(ctx context.Context, s kit.Step) (kit.Result, error) {
	if s.Name != "rabbitmq.publish" {
		return kit.Result{}, fmt.Errorf("rabbitmq: unknown step %s", s.Name)
	}
	var raw []byte
	switch v := s.With["body"].(type) {
	case string:
		raw = []byte(v)
	default:
		raw, _ = json.Marshal(v)
	}
	hdr, _ := s.With["headers"].(map[string]any)
	n := kit.Int(s.With, "count", 1)
	q := kit.Str(s.With, "queue")
	err := r.c.publish(ctx, q, raw, hdr, kit.Str(s.With, "message_id"), n)
	return kit.Result{Note: fmt.Sprintf("%d message(s) to %s/%s", n, r.c.vhost, q)}, err
}

// Check resolves:
//
//	rabbitmq.<queue>.ready       messages waiting (exact)
//	rabbitmq.<queue>.unacked     delivered, not yet acknowledged
//	rabbitmq.<queue>.depth       ready + unacked
//	rabbitmq.<queue>.consumers   consumers attached
//	rabbitmq.<queue>.published   messages published to the queue since it was declared
//	rabbitmq.<queue>.acked       messages acknowledged
//	rabbitmq.<queue>.redelivered messages delivered again (nack, crash, lost connection)
//	rabbitmq.<queue>.messages    bodies of the messages waiting (peeked; use on dead-letter queues)
func (r *Connector) Check(ctx context.Context, ref kit.CheckRef) (kit.Observation, error) {
	if len(ref.Segments) != 3 {
		return kit.Observation{At: time.Now().UTC()}, fmt.Errorf("expected rabbitmq.<queue>.<ready|unacked|depth|consumers|published|acked|redelivered|messages>")
	}
	queue, prop := ref.Segments[1].Name, ref.Segments[2].Name
	src := fmt.Sprintf("RabbitMQ %s/%s", r.c.vhost, queue)
	obs := func(v any, raw any) (kit.Observation, error) {
		return kit.Observation{Value: v, Raw: raw, Source: src + " " + prop, At: time.Now().UTC()}, nil
	}
	if prop == "ready" || prop == "consumers" {
		ready, consumers, err := r.c.ready(queue)
		if err != nil {
			return kit.Observation{Source: src, At: time.Now().UTC()}, err
		}
		if prop == "ready" {
			return obs(ready, nil)
		}
		return obs(consumers, nil)
	}
	if prop == "messages" {
		msgs, err := r.c.peek(ctx, queue, 200)
		if err != nil {
			return kit.Observation{Source: src, At: time.Now().UTC()}, err
		}
		return obs(msgs, msgs)
	}
	st, err := r.c.stats(ctx, queue)
	if err != nil {
		return kit.Observation{Source: src, At: time.Now().UTC()}, err
	}
	switch prop {
	case "unacked":
		return obs(st.Unacked, st)
	case "depth":
		ready, _, err := r.c.ready(queue)
		if err != nil {
			return kit.Observation{Source: src, At: time.Now().UTC()}, err
		}
		return obs(ready+st.Unacked, st)
	case "published":
		return obs(st.Stats.Publish, st)
	case "acked":
		return obs(st.Stats.Ack, st)
	case "redelivered":
		return obs(st.Stats.Redeliver, st)
	}
	return kit.Observation{At: time.Now().UTC()}, fmt.Errorf("unknown rabbitmq check %q", prop)
}

// peek reads message bodies without consuming them (requeued; this flags
// them redelivered, which is why it is meant for dead-letter queues).
func (c *Client) peek(ctx context.Context, queue string, limit int) ([]any, error) {
	var res []struct {
		Payload  string `json:"payload"`
		Encoding string `json:"payload_encoding"`
	}
	err := c.mgmt(ctx, http.MethodPost, "/api/queues/"+url.PathEscape(c.vhost)+"/"+url.PathEscape(queue)+"/get",
		map[string]any{"count": limit, "ackmode": "ack_requeue_true", "encoding": "auto", "truncate": 65536}, &res)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(res))
	for _, m := range res {
		var v any
		if json.Unmarshal([]byte(m.Payload), &v) == nil {
			out = append(out, v)
		} else {
			out = append(out, m.Payload)
		}
	}
	return out, nil
}

// Collect saves the numbers of every declared queue and the content of the
// dead-letter queues.
func (r *Connector) Collect(ctx context.Context, _ kit.TimeWindow) ([]kit.Artifact, error) {
	if r.c == nil {
		return nil, nil
	}
	var queues []map[string]any
	dlqs := map[string]bool{}
	for _, q := range r.env.Service.Stores.RabbitMQ.Queues {
		names := []string{q.Name}
		if q.DLQ != "" {
			names = append(names, q.DLQ)
			dlqs[q.DLQ] = true
		}
		for _, n := range names {
			st, err := r.c.stats(ctx, n)
			entry := map[string]any{"queue": n, "stats": st}
			if err != nil {
				entry["error"] = err.Error()
			}
			if dlqs[n] {
				if msgs, err := r.c.peek(ctx, n, 100); err == nil {
					entry["messages"] = msgs
				}
			}
			queues = append(queues, entry)
		}
	}
	rel := path.Join(r.env.CaseDir, "output", "rabbitmq", "queues.json")
	if _, err := r.env.Evidence.WriteJSON(rel, map[string]any{"vhost": r.c.vhost, "queues": queues}); err != nil {
		return nil, err
	}
	return []kit.Artifact{{Kind: "snapshot", Path: rel, Title: fmt.Sprintf("RabbitMQ queues of vhost %s (%d)", r.c.vhost, len(queues)), Source: "rabbitmq"}}, nil
}

// Teardown deletes the vhost (queues, messages and connections with it).
func (r *Connector) Teardown(ctx context.Context) error {
	if r.c == nil {
		return nil
	}
	r.c.close()
	return r.c.mgmt(ctx, http.MethodDelete, "/api/vhosts/"+url.PathEscape(r.c.vhost), nil, nil)
}

var (
	_ kit.Connector = (*Connector)(nil)
	_ kit.Checker   = (*Connector)(nil)
)
