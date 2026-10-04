// Package cache keeps the order status in Redis for read paths. Writers
// (payment, refund webhook) update it; a stale value would show a refunded
// order as paid.
package cache

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

type Redis struct {
	Client *redis.Client
	Prefix string
	TTL    time.Duration
}

func (r *Redis) Enabled() bool { return r != nil && r.Client != nil }

func (r *Redis) key(orderID string) string { return r.Prefix + "order:" + orderID + ":status" }

// SetStatus caches the status of an order.
func (r *Redis) SetStatus(ctx context.Context, orderID, status string) error {
	if !r.Enabled() {
		return nil
	}
	return r.Client.Set(ctx, r.key(orderID), status, r.TTL).Err()
}
