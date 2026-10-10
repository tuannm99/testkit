// Package redisq is the Redis queue trigger, for a stream (consumer group,
// XACK after handling, XAUTOCLAIM of entries a dead consumer left pending) or a
// list (reliable pattern: BLMOVE into a processing list, removed after
// handling; what a crash left there is moved back at start).
package redisq

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/tuannm99/testkit/reference-worker/internal/failpoint"
	"github.com/tuannm99/testkit/reference-worker/internal/trigger"
)

type Consumer struct {
	Client     *redis.Client
	Kind       string // stream | list
	Queue      string // full key
	Group      string // stream
	Consumer   string // stream: this worker's name
	Processing string // list: in-flight list
	DLQ        string // optional
	ClaimIdle  time.Duration
	Handler    *trigger.Handler
	Log        *slog.Logger
}

func (c *Consumer) Run(ctx context.Context) error {
	if c.Kind == "stream" {
		return c.runStream(ctx)
	}
	return c.runList(ctx)
}

func (c *Consumer) runStream(ctx context.Context) error {
	err := c.Client.XGroupCreateMkStream(ctx, c.Queue, c.Group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	c.Log.Info("redis stream consuming", "stream", c.Queue, "group", c.Group)
	for ctx.Err() == nil {
		// Entries another consumer received and never acknowledged (it died).
		claimed, _, err := c.Client.XAutoClaim(ctx, &redis.XAutoClaimArgs{Stream: c.Queue, Group: c.Group,
			Consumer: c.Consumer, MinIdle: c.ClaimIdle, Start: "0", Count: 10}).Result()
		if err != nil && ctx.Err() == nil {
			return err
		}
		if err := c.streamBatch(ctx, claimed); err != nil {
			return err
		}
		res, err := c.Client.XReadGroup(ctx, &redis.XReadGroupArgs{Group: c.Group, Consumer: c.Consumer,
			Streams: []string{c.Queue, ">"}, Count: 10, Block: time.Second}).Result()
		if err == redis.Nil || ctx.Err() != nil {
			continue
		}
		if err != nil {
			return err
		}
		for _, s := range res {
			if err := c.streamBatch(ctx, s.Messages); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Consumer) streamBatch(ctx context.Context, msgs []redis.XMessage) error {
	for _, m := range msgs {
		body, _ := m.Values["payload"].(string)
		if failpoint.Enabled(failpoint.CommitBeforeProcess) {
			c.Client.XAck(context.WithoutCancel(ctx), c.Queue, c.Group, m.ID)
		}
		err := c.Handler.Handle(context.WithoutCancel(ctx), ctx, []byte(body), c.Queue+"/"+m.ID)
		if failpoint.Enabled(failpoint.CommitBeforeProcess) {
			continue
		}
		var dl *trigger.DeadLetter
		switch {
		case err == nil:
		case asDeadLetter(err, &dl):
			c.Log.Error("dead-lettering entry", "stream", c.Queue, "id", m.ID, "attempts", dl.Attempts, "err", dl.Cause.Error())
			if c.DLQ != "" && !failpoint.Enabled(failpoint.DropDeadLetters) {
				if e := c.Client.XAdd(ctx, &redis.XAddArgs{Stream: c.DLQ, Values: map[string]any{
					"payload": body, "dlq_error": dl.Cause.Error(), "dlq_source": m.ID}}).Err(); e != nil {
					return e
				}
			}
		default:
			return nil // shutdown during a retry wait: stays pending, claimed later
		}
		if e := c.Client.XAck(context.WithoutCancel(ctx), c.Queue, c.Group, m.ID).Err(); e != nil {
			return e
		}
	}
	return nil
}

func (c *Consumer) runList(ctx context.Context) error {
	// What a previous run left in flight is delivered again.
	for {
		moved, err := c.Client.LMove(ctx, c.Processing, c.Queue, "LEFT", "RIGHT").Result()
		if err == redis.Nil {
			break
		}
		if err != nil {
			return err
		}
		c.Log.Warn("requeued an unfinished message", "queue", c.Queue, "bytes", len(moved))
	}
	c.Log.Info("redis list consuming", "queue", c.Queue)
	for ctx.Err() == nil {
		body, err := c.Client.BLMove(ctx, c.Queue, c.Processing, "RIGHT", "LEFT", time.Second).Result()
		if err == redis.Nil || ctx.Err() != nil {
			continue
		}
		if err != nil {
			return err
		}
		if failpoint.Enabled(failpoint.CommitBeforeProcess) {
			c.Client.LRem(context.WithoutCancel(ctx), c.Processing, 1, body)
		}
		delivery := fmt.Sprintf("%s/%d", c.Queue, time.Now().UnixNano())
		err = c.Handler.Handle(context.WithoutCancel(ctx), ctx, []byte(body), delivery)
		if failpoint.Enabled(failpoint.CommitBeforeProcess) {
			continue
		}
		var dl *trigger.DeadLetter
		switch {
		case err == nil:
		case asDeadLetter(err, &dl):
			c.Log.Error("dead-lettering message", "queue", c.Queue, "attempts", dl.Attempts, "err", dl.Cause.Error())
			if c.DLQ != "" && !failpoint.Enabled(failpoint.DropDeadLetters) {
				if e := c.Client.LPush(ctx, c.DLQ, body).Err(); e != nil {
					return e
				}
			}
		default:
			return nil // left in the processing list: requeued at the next start
		}
		if e := c.Client.LRem(context.WithoutCancel(ctx), c.Processing, 1, body).Err(); e != nil {
			return e
		}
	}
	return nil
}

func asDeadLetter(err error, out **trigger.DeadLetter) bool {
	d, ok := err.(*trigger.DeadLetter)
	if ok {
		*out = d
	}
	return ok
}
