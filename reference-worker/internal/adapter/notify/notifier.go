// Package notify delivers partner notifications over a WebSocket,
// at-least-once: messages stay pending in Postgres until the partner acks
// them; after a reconnect every unacked message is sent again (the partner
// deduplicates by id). A heartbeat detects silent (half-open) connections.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tuannm99/testkit/reference-worker/internal/failpoint"
)

// Queue records notifications (idempotent by msg id) in Postgres.
type Queue struct {
	Pool *pgxpool.Pool
}

func (q *Queue) Enqueue(ctx context.Context, msgID string, payload map[string]any) error {
	raw, _ := json.Marshal(payload)
	_, err := q.Pool.Exec(ctx, `INSERT INTO notifications (msg_id, payload) VALUES ($1, $2) ON CONFLICT (msg_id) DO NOTHING`, msgID, raw)
	return err
}

type Notifier struct {
	URL       string
	Pool      *pgxpool.Pool
	Heartbeat time.Duration
	Log       *slog.Logger
}

type pending struct {
	msgID   string
	payload json.RawMessage
}

// Run keeps a connection open and delivers pending notifications until ctx ends.
func (n *Notifier) Run(ctx context.Context) {
	backoff := 200 * time.Millisecond
	first := true
	for ctx.Err() == nil {
		err := n.session(ctx, first)
		first = false
		if ctx.Err() != nil {
			return
		}
		n.Log.Warn("notifier connection lost, reconnecting", "err", fmt.Sprint(err), "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 2*time.Second)
	}
}

func (n *Notifier) session(ctx context.Context, first bool) error {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, _, err := websocket.Dial(dctx, n.URL, nil)
	cancel()
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	n.Log.Info("notifier connected", "reconnect", !first)
	sctx, stop := context.WithCancel(ctx)
	defer stop()

	var mu sync.Mutex
	lastSeen := time.Now()
	sent := map[string]bool{} // sent on this connection
	if !first && failpoint.Enabled(failpoint.WSNoResend) {
		// mutation: forget what was pending before the reconnect
		rows, _ := n.Pool.Query(ctx, `SELECT msg_id FROM notifications WHERE acked_at IS NULL`)
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			sent[id] = true
		}
		rows.Close()
	}
	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.Read(sctx)
			if err != nil {
				readErr <- err
				return
			}
			mu.Lock()
			lastSeen = time.Now()
			mu.Unlock()
			var m struct {
				Ack  string `json:"ack"`
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &m) == nil && m.Ack != "" {
				if _, err := n.Pool.Exec(sctx, `UPDATE notifications SET acked_at = now() WHERE msg_id = $1 AND acked_at IS NULL`, m.Ack); err != nil {
					n.Log.Error("ack", "err", err.Error())
				}
			}
		}
	}()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	lastPing := time.Time{}
	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "shutdown")
			return nil
		case err := <-readErr:
			return fmt.Errorf("read: %w", err)
		case <-tick.C:
		}
		mu.Lock()
		silent := time.Since(lastSeen)
		mu.Unlock()
		if !failpoint.Enabled(failpoint.WSNoHeartbeat) && silent > 3*n.Heartbeat {
			return fmt.Errorf("no message from partner for %s (half-open connection?)", silent.Round(time.Millisecond))
		}
		if time.Since(lastPing) >= n.Heartbeat {
			lastPing = time.Now()
			if err := write(sctx, conn, map[string]any{"type": "ping"}); err != nil {
				return fmt.Errorf("ping: %w", err)
			}
		}
		msgs, err := n.unacked(sctx)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if sent[m.msgID] {
				continue
			}
			var body map[string]any
			_ = json.Unmarshal(m.payload, &body)
			body["id"] = m.msgID
			if err := write(sctx, conn, body); err != nil {
				return fmt.Errorf("send %s: %w", m.msgID, err)
			}
			sent[m.msgID] = true
			_, _ = n.Pool.Exec(sctx, `UPDATE notifications SET sent_count = sent_count + 1 WHERE msg_id = $1`, m.msgID)
		}
	}
}

func write(ctx context.Context, c *websocket.Conn, v any) error {
	raw, _ := json.Marshal(v)
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, raw)
}

func (n *Notifier) unacked(ctx context.Context) ([]pending, error) {
	rows, err := n.Pool.Query(ctx, `SELECT msg_id, payload FROM notifications WHERE acked_at IS NULL ORDER BY id LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.msgID, &p.payload); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
