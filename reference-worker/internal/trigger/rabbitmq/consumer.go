// Package rabbitmq is the RabbitMQ trigger. A message is acknowledged only
// after the job is handled, so a crash (the connection drops) means
// redelivery, never loss. Poison and exhausted messages are rejected without
// requeue: the broker's dead-letter exchange moves them to the DLQ. Messages
// are handled one at a time, in order.
package rabbitmq

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/tuannm99/testkit/reference-worker/internal/failpoint"
	"github.com/tuannm99/testkit/reference-worker/internal/trigger"
)

type Consumer struct {
	URL      string
	Queue    string
	Prefetch int
	Handler  *trigger.Handler
	Log      *slog.Logger
}

// Run consumes until ctx is cancelled; the message in flight is finished and
// acknowledged first. A lost connection returns an error (the caller restarts).
func (c *Consumer) Run(ctx context.Context) error {
	conn, err := amqp.Dial(c.URL)
	if err != nil {
		return err
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	if err := ch.Qos(max(c.Prefetch, 1), 0, false); err != nil {
		return err
	}
	// Passive: the queue (and its dead-lettering) is declared by the deployment.
	if _, err := ch.QueueDeclarePassive(c.Queue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("queue %s: %w", c.Queue, err)
	}
	msgs, err := ch.Consume(c.Queue, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	closed := conn.NotifyClose(make(chan *amqp.Error, 1))
	connID := fmt.Sprintf("%x", time.Now().UnixNano())
	c.Log.Info("rabbitmq consuming", "queue", c.Queue)
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-closed:
			return fmt.Errorf("rabbitmq connection closed: %v", e)
		case d, ok := <-msgs:
			if !ok {
				return fmt.Errorf("rabbitmq delivery channel closed")
			}
			if failpoint.Enabled(failpoint.CommitBeforeProcess) {
				_ = d.Ack(false)
			}
			delivery := fmt.Sprintf("%s/%s/%d", c.Queue, connID, d.DeliveryTag)
			err := c.Handler.Handle(context.WithoutCancel(ctx), ctx, d.Body, delivery)
			if failpoint.Enabled(failpoint.CommitBeforeProcess) {
				continue // already acknowledged: a failure now loses the message
			}
			var dl *trigger.DeadLetter
			switch {
			case err == nil:
				if e := d.Ack(false); e != nil {
					return e
				}
			case asDeadLetter(err, &dl):
				c.Log.Error("dead-lettering message", "queue", c.Queue, "attempts", dl.Attempts, "err", dl.Cause.Error())
				if failpoint.Enabled(failpoint.DropDeadLetters) {
					err = d.Ack(false) // mutation: the message vanishes
				} else {
					err = d.Nack(false, false)
				}
				if err != nil {
					return err
				}
			default:
				return nil // shutdown during a retry wait: left unacked, redelivered
			}
		}
	}
}

func asDeadLetter(err error, out **trigger.DeadLetter) bool {
	d, ok := err.(*trigger.DeadLetter)
	if ok {
		*out = d
	}
	return ok
}
