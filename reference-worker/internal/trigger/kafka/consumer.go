// Package kafka is the Kafka trigger. Offsets are committed only after a
// record is fully handled (processed, or dead-lettered), so a crash before
// the commit means redelivery, never loss. Records of one partition are
// processed in order.
package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tuannm99/testkit/reference-worker/internal/clock"
	"github.com/tuannm99/testkit/reference-worker/internal/domain"
	"github.com/tuannm99/testkit/reference-worker/internal/failpoint"
	"github.com/tuannm99/testkit/reference-worker/internal/metrics"
	"github.com/tuannm99/testkit/reference-worker/internal/trigger"
)

type Consumer struct {
	Client      *kgo.Client
	DLQTopic    string
	MaxAttempts int
	Process     trigger.ProcessFunc
	Clock       clock.Clock
	Metrics     *metrics.Metrics
	Log         *slog.Logger
}

// NewClient builds a consumer-group client with manual commits.
func NewClient(brokers []string, group, topic string, log *slog.Logger) (*kgo.Client, error) {
	return kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topic),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
		kgo.FetchMaxWait(500*time.Millisecond),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.OnPartitionsAssigned(func(_ context.Context, _ *kgo.Client, m map[string][]int32) {
			log.Info("kafka partitions assigned", "partitions", fmt.Sprint(m))
		}),
		kgo.OnPartitionsRevoked(func(ctx context.Context, cl *kgo.Client, m map[string][]int32) {
			log.Info("kafka partitions revoked", "partitions", fmt.Sprint(m))
		}),
	)
}

type message struct {
	JobID      string `json:"job_id"`
	OrderID    string `json:"order_id"`
	EnqueuedAt string `json:"enqueued_at,omitempty"`
}

// Run consumes until ctx is cancelled. The record in flight when ctx is
// cancelled (SIGTERM) is finished and committed before returning.
func (c *Consumer) Run(ctx context.Context) error {
	for {
		fetches := c.Client.PollRecords(ctx, 50)
		if ctx.Err() != nil {
			c.Client.AllowRebalance()
			return nil
		}
		if errs := fetches.Errors(); len(errs) > 0 {
			for _, e := range errs {
				c.Log.Warn("kafka fetch error", "topic", e.Topic, "partition", e.Partition, "err", e.Err.Error())
			}
		}
		var stop bool
		fetches.EachRecord(func(r *kgo.Record) {
			if stop {
				return
			}
			if failpoint.Enabled(failpoint.CommitBeforeProcess) {
				_ = c.Client.CommitRecords(context.WithoutCancel(ctx), r)
			}
			// Finish the current record even if shutdown starts now.
			if err := c.handle(context.WithoutCancel(ctx), ctx, r); err != nil {
				// Not committed: will be redelivered. Rewind so the partition is retried.
				c.Log.Warn("record not handled, will be redelivered", "err", err.Error())
				stop = true
				return
			}
			if err := c.Client.CommitRecords(context.WithoutCancel(ctx), r); err != nil {
				c.Log.Error("commit failed", "err", err.Error())
			}
		})
		c.Client.AllowRebalance()
		if stop {
			if ctx.Err() != nil {
				return nil
			}
			// Records after the failed one were not processed: restart from the committed offsets.
			return errors.New("kafka handler stopped; restarting consumer from committed offsets")
		}
	}
}

// handle processes one record with in-place retries; poison or exhausted
// records go to the DLQ. procCtx is not cancelled by shutdown; stopCtx is
// used to abort retry waits.
func (c *Consumer) handle(procCtx, stopCtx context.Context, r *kgo.Record) error {
	var m message
	if err := json.Unmarshal(r.Value, &m); err != nil || m.OrderID == "" {
		return c.deadLetter(procCtx, r, fmt.Errorf("poison message: %v", err), 0)
	}
	for attempt := 1; ; attempt++ {
		err := c.Process(procCtx, domain.Job{ID: m.JobID, OrderID: m.OrderID, Source: "kafka", Attempt: attempt,
			Delivery: fmt.Sprintf("%s/%d/%d", r.Topic, r.Partition, r.Offset)})
		if err == nil {
			c.observeE2E(m)
			return nil
		}
		if domain.IsPermanent(err) || attempt >= c.MaxAttempts {
			return c.deadLetter(procCtx, r, err, attempt)
		}
		wait := time.Duration(attempt) * 500 * time.Millisecond
		if errors.Is(err, domain.ErrBusy) {
			wait = time.Second
			attempt-- // another worker holds the order: waiting is not a failed attempt
		}
		c.Log.Warn("process failed, retrying", "order_id", m.OrderID, "attempt", attempt, "err", err.Error())
		if err := c.Clock.Sleep(stopCtx, wait); err != nil {
			return err
		}
	}
}

func (c *Consumer) observeE2E(m message) {
	if t, err := time.Parse(time.RFC3339Nano, m.EnqueuedAt); err == nil {
		c.Metrics.E2ESeconds.WithLabelValues("kafka").Observe(time.Since(t).Seconds())
	}
}

func (c *Consumer) deadLetter(ctx context.Context, r *kgo.Record, cause error, attempts int) error {
	c.Metrics.DLQ.Inc()
	c.Log.Error("dead-lettering record", "partition", r.Partition, "offset", r.Offset, "err", cause.Error())
	if c.DLQTopic == "" {
		return nil
	}
	dlq := &kgo.Record{Topic: c.DLQTopic, Key: r.Key, Value: r.Value, Headers: append(r.Headers,
		kgo.RecordHeader{Key: "dlq_error", Value: []byte(cause.Error())},
		kgo.RecordHeader{Key: "dlq_attempts", Value: []byte(strconv.Itoa(attempts))},
		kgo.RecordHeader{Key: "dlq_source", Value: []byte(fmt.Sprintf("%s/%d/%d", r.Topic, r.Partition, r.Offset))},
	)}
	return c.Client.ProduceSync(ctx, dlq).FirstErr()
}
