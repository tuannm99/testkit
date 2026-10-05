// Package outbox relays committed outbox rows to Kafka. Rows are marked as
// published only after the broker acknowledged them (at-least-once; the
// event_id lets consumers deduplicate). Nothing is published that was not
// committed, so there are no "ghost" events.
package outbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tuannm99/testkit/reference-worker/internal/metrics"
)

type Relay struct {
	Pool     *pgxpool.Pool
	Kafka    *kgo.Client
	Interval time.Duration
	Metrics  *metrics.Metrics
	Log      *slog.Logger
}

func (r *Relay) Run(ctx context.Context) {
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for {
			n, err := r.relayBatch(ctx)
			if err != nil {
				if ctx.Err() == nil {
					r.Log.Error("outbox relay", "err", err.Error())
				}
				break
			}
			if n == 0 {
				break
			}
		}
	}
}

func (r *Relay) relayBatch(ctx context.Context) (int, error) {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, `SELECT id, event_id, topic, key, payload::text FROM outbox
		WHERE published_at IS NULL ORDER BY id LIMIT 100 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return 0, err
	}
	var ids []int64
	var recs []*kgo.Record
	for rows.Next() {
		var id int64
		var eventID, topic, key, payload string
		if err := rows.Scan(&id, &eventID, &topic, &key, &payload); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		recs = append(recs, &kgo.Record{Topic: topic, Key: []byte(key), Value: []byte(payload),
			Headers: []kgo.RecordHeader{{Key: "event_id", Value: []byte(eventID)}}})
	}
	rows.Close()
	if len(recs) == 0 {
		return 0, nil
	}
	if err := r.Kafka.ProduceSync(ctx, recs...).FirstErr(); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, ids); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	r.Metrics.OutboxRelayed.Add(float64(len(ids)))
	return len(ids), nil
}
