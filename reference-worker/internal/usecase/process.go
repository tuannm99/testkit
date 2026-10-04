// Package usecase holds Process, the single business entry point shared by
// every trigger (Kafka consumer and DB poller).
package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/tuannm99/testkit/reference-worker/internal/domain"
	"github.com/tuannm99/testkit/reference-worker/internal/failpoint"
	"github.com/tuannm99/testkit/reference-worker/internal/metrics"
)

// Ports used by Process (implemented by adapters).
type (
	Orders interface {
		Get(ctx context.Context, id string) (domain.Order, error)
		Claim(ctx context.Context, id, owner string, lease time.Duration) (domain.Order, bool, error)
		Release(ctx context.Context, id, owner string) error
		MarkFailed(ctx context.Context, id, owner, reason string) error
		MarkPaid(ctx context.Context, o domain.Order, owner, chargeID string) error
		ClaimMail(ctx context.Context, id string) (bool, error)
		UnclaimMail(ctx context.Context, id string) error
	}
	Payments interface {
		Charge(ctx context.Context, o domain.Order) (domain.Charge, error)
	}
	Search interface {
		IndexOrders(ctx context.Context, orders ...domain.Order) error
	}
	Mailer interface {
		Enabled() bool
		SendPaid(ctx context.Context, o domain.Order) error
	}
)

type Processor struct {
	Orders     Orders
	Payments   Payments
	Search     Search
	Mailer     Mailer
	WorkerID   string
	OrderLease time.Duration
	Metrics    *metrics.Metrics
	Log        *slog.Logger
	// Exit is called by the crash failpoint (os.Exit in production wiring).
	Exit func(code int)
}

// Process pays an order and propagates the result. It is idempotent: a job
// delivered twice, or redelivered after a crash, charges at most once (claim +
// Idempotency-Key), indexes the same document and sends at most one mail.
func (p *Processor) Process(ctx context.Context, job domain.Job) (err error) {
	start := time.Now()
	p.Metrics.InFlight.Inc()
	defer func() {
		p.Metrics.InFlight.Dec()
		p.Metrics.ProcessSeconds.WithLabelValues(job.Source).Observe(time.Since(start).Seconds())
		result := "ok"
		switch {
		case err == nil:
		case errors.Is(err, domain.ErrBusy):
			result = "busy"
		case domain.IsPermanent(err):
			result = "permanent_error"
		default:
			result = "retryable_error"
		}
		p.Metrics.Jobs.WithLabelValues(job.Source, result).Inc()
	}()
	log := p.Log.With("job_id", job.ID, "order_id", job.OrderID, "source", job.Source, "attempt", job.Attempt)
	if job.OrderID == "" {
		return domain.Permanent(errors.New("job without order_id"))
	}
	// The claim owner is unique per delivery: duplicates of one job (same job
	// key) delivered concurrently must not both own the order.
	owner := fmt.Sprintf("%s/%s/%d", p.WorkerID, job.Delivery, job.Attempt)

	order, claimed, err := p.Orders.Claim(ctx, job.OrderID, owner, p.OrderLease)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Permanent(err)
	}
	if err != nil {
		return err
	}
	if failpoint.Enabled(failpoint.SkipPaidCheck) && order.Status == domain.StatusPaid {
		claimed = true // mutation: behave as if the order was never paid
	}
	if claimed {
		ch, err := p.Payments.Charge(ctx, order)
		if err != nil {
			if domain.IsPermanent(err) {
				log.Error("payment failed permanently", "err", err.Error())
				_ = p.Orders.MarkFailed(context.WithoutCancel(ctx), order.ID, owner, err.Error())
				return err
			}
			_ = p.Orders.Release(context.WithoutCancel(ctx), order.ID, owner)
			return err
		}
		if err := p.Orders.MarkPaid(ctx, order, owner, ch.ID); err != nil {
			return err
		}
		log.Info("order paid", "payment_ref", ch.ID)
		if failpoint.Enabled(failpoint.CrashAfterDBCommit) {
			log.Error("failpoint: crashing after db commit")
			p.Exit(3)
		}
		if order, err = p.Orders.Get(ctx, order.ID); err != nil {
			return err
		}
	}
	if order.Status == domain.StatusFailed {
		log.Info("order already failed, nothing to do")
		return nil
	}
	if order.Status != domain.StatusPaid {
		return fmt.Errorf("order %s in unexpected status %s", order.ID, order.Status)
	}
	// Side effects after the commit are idempotent and re-run on redelivery.
	if err := p.Search.IndexOrders(ctx, order); err != nil {
		return err
	}
	if p.Mailer != nil && p.Mailer.Enabled() {
		send := true
		if !failpoint.Enabled(failpoint.SkipMailGuard) {
			if send, err = p.Orders.ClaimMail(ctx, order.ID); err != nil {
				return err
			}
		}
		if send {
			if err := p.Mailer.SendPaid(ctx, order); err != nil {
				_ = p.Orders.UnclaimMail(context.WithoutCancel(ctx), order.ID)
				return err
			}
			p.Metrics.MailsSent.Inc()
			log.Info("mail sent", "to_domain", domainOf(order.CustomerEmail))
		}
	}
	return nil
}

func domainOf(email string) string {
	for i := len(email) - 1; i >= 0; i-- {
		if email[i] == '@' {
			return email[i+1:]
		}
	}
	return ""
}

// OSExit is the production Exit.
func OSExit(code int) { os.Exit(code) }
