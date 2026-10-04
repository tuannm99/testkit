// Package payment is the HTTP adapter of the payment gateway, with retries
// that honour Retry-After and an Idempotency-Key per order.
package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/tuannm99/testkit/reference-worker/internal/clock"
	"github.com/tuannm99/testkit/reference-worker/internal/domain"
	"github.com/tuannm99/testkit/reference-worker/internal/failpoint"
	"github.com/tuannm99/testkit/reference-worker/internal/metrics"
)

type Client struct {
	BaseURL     string
	HTTP        *http.Client
	MaxAttempts int
	MaxBackoff  time.Duration
	Clock       clock.Clock
	Metrics     *metrics.Metrics
	Log         *slog.Logger
}

type chargeReq struct {
	OrderID  string `json:"order_id"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// Charge charges the order. Transient failures (network, 429, 5xx) are
// retried with exponential backoff (or Retry-After); 4xx are permanent.
func (c *Client) Charge(ctx context.Context, o domain.Order) (domain.Charge, error) {
	body, _ := json.Marshal(chargeReq{OrderID: o.ID, Amount: o.AmountCents, Currency: o.Currency})
	var lastErr error
	for attempt := 1; attempt <= c.MaxAttempts; attempt++ {
		ch, wait, err := c.once(ctx, o, body)
		if err == nil {
			return ch, nil
		}
		lastErr = err
		if failpoint.Enabled(failpoint.NoRetry) {
			return domain.Charge{}, domain.Permanent(err)
		}
		if domain.IsPermanent(err) || attempt == c.MaxAttempts {
			break
		}
		if wait <= 0 {
			wait = backoff(attempt, c.MaxBackoff)
		}
		c.Log.Warn("payment retry", "order_id", o.ID, "attempt", attempt, "wait", wait.String(), "err", err.Error())
		if err := c.Clock.Sleep(ctx, wait); err != nil {
			return domain.Charge{}, err
		}
	}
	return domain.Charge{}, lastErr
}

func (c *Client) once(ctx context.Context, o domain.Order, body []byte) (domain.Charge, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/charges", bytes.NewReader(body))
	if err != nil {
		return domain.Charge{}, 0, domain.Permanent(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if !failpoint.Enabled(failpoint.NoIdempotencyKey) {
		req.Header.Set("Idempotency-Key", "order-"+o.ID)
	}
	start := c.Clock.Now()
	resp, err := c.HTTP.Do(req)
	c.Metrics.PaymentSeconds.Observe(time.Since(start).Seconds())
	if err != nil {
		c.Metrics.PaymentCalls.WithLabelValues("error").Inc()
		return domain.Charge{}, 0, fmt.Errorf("payment transport: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	c.Metrics.PaymentCalls.WithLabelValues(strconv.Itoa(resp.StatusCode)).Inc()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var ch domain.Charge
		if err := json.Unmarshal(raw, &ch); err != nil || ch.ID == "" {
			// A 2xx we cannot understand: do not mark paid, retry later.
			return domain.Charge{}, 0, fmt.Errorf("payment: invalid success body %q", truncate(raw))
		}
		if ch.Status != "succeeded" {
			return domain.Charge{}, 0, domain.Permanent(fmt.Errorf("payment declined: %s", ch.Status))
		}
		return ch, 0, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return domain.Charge{}, retryAfter(resp.Header.Get("Retry-After"), c.MaxBackoff),
			fmt.Errorf("payment: HTTP %d", resp.StatusCode)
	default:
		return domain.Charge{}, 0, domain.Permanent(fmt.Errorf("payment: HTTP %d %s", resp.StatusCode, truncate(raw)))
	}
}

func retryAfter(v string, max time.Duration) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil {
		return min(time.Duration(s)*time.Second, max)
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(time.Until(t), max)
	}
	return 0
}

func backoff(attempt int, max time.Duration) time.Duration {
	d := 100 * time.Millisecond << (attempt - 1)
	d += time.Duration(rand.Int63n(int64(d) / 2))
	return min(d, max)
}

func truncate(b []byte) string {
	if len(b) > 200 {
		return string(b[:200])
	}
	return string(b)
}
