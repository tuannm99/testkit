// Package trigger defines what both triggers share: the Process function.
package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tuannm99/testkit/reference-worker/internal/clock"
	"github.com/tuannm99/testkit/reference-worker/internal/domain"
	"github.com/tuannm99/testkit/reference-worker/internal/failpoint"
	"github.com/tuannm99/testkit/reference-worker/internal/metrics"
)

// ProcessFunc is usecase.Processor.Process.
type ProcessFunc func(ctx context.Context, job domain.Job) error

// DeadLetter is returned by Handler.Handle when a message must be moved to the
// dead-letter queue: poison, permanent failure, or attempts exhausted.
type DeadLetter struct {
	Cause    error
	Attempts int
}

func (d *DeadLetter) Error() string { return d.Cause.Error() }

// Message is the JSON body every queue trigger carries.
type Message struct {
	JobID      string `json:"job_id"`
	OrderID    string `json:"order_id"`
	EnqueuedAt string `json:"enqueued_at,omitempty"`
}

// Handler processes one message body with in-place retries. It is shared by the
// RabbitMQ and Redis consumers (Kafka keeps its own copy tied to its offsets).
type Handler struct {
	Source      string
	Process     ProcessFunc
	MaxAttempts int
	Clock       clock.Clock
	Metrics     *metrics.Metrics
	Log         *slog.Logger
}

// Handle returns nil when the job is done, *DeadLetter when it must be
// dead-lettered, and another error only when shutdown aborted a retry wait
// (the message must then be left unacknowledged: it is redelivered).
func (h *Handler) Handle(procCtx, stopCtx context.Context, body []byte, delivery string) error {
	var m Message
	if err := json.Unmarshal(body, &m); err != nil || m.OrderID == "" {
		return &DeadLetter{Cause: fmt.Errorf("poison message: %v", err)}
	}
	outage := 500 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := h.Process(procCtx, domain.Job{ID: m.JobID, OrderID: m.OrderID, Source: h.Source, Attempt: attempt, Delivery: delivery})
		if err == nil {
			if t, perr := time.Parse(time.RFC3339Nano, m.EnqueuedAt); perr == nil {
				h.Metrics.E2ESeconds.WithLabelValues(h.Source).Observe(time.Since(t).Seconds())
			}
			return nil
		}
		unavailable := domain.IsUnavailable(err) && !failpoint.Enabled(failpoint.OutageIsFailure)
		if !unavailable && (domain.IsPermanent(err) || attempt >= h.MaxAttempts) {
			h.Metrics.DLQ.Inc()
			return &DeadLetter{Cause: err, Attempts: attempt}
		}
		wait := time.Duration(attempt) * 500 * time.Millisecond
		switch {
		case errors.Is(err, domain.ErrBusy):
			wait = time.Second
			attempt--
		case unavailable:
			wait, outage = outage, min(outage*2, 5*time.Second)
			attempt--
		}
		h.Log.Warn("process failed, retrying", "order_id", m.OrderID, "attempt", attempt, "err", err.Error())
		if err := h.Clock.Sleep(stopCtx, wait); err != nil {
			return err
		}
	}
}
