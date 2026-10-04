// Package app wires the worker with Uber Fx.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/tuannm99/testkit/reference-worker/internal/adapter/mail"
	"github.com/tuannm99/testkit/reference-worker/internal/adapter/outbox"
	"github.com/tuannm99/testkit/reference-worker/internal/adapter/payment"
	"github.com/tuannm99/testkit/reference-worker/internal/adapter/pgrepo"
	"github.com/tuannm99/testkit/reference-worker/internal/adapter/search"
	"github.com/tuannm99/testkit/reference-worker/internal/clock"
	"github.com/tuannm99/testkit/reference-worker/internal/config"
	"github.com/tuannm99/testkit/reference-worker/internal/failpoint"
	"github.com/tuannm99/testkit/reference-worker/internal/httpserver"
	"github.com/tuannm99/testkit/reference-worker/internal/metrics"
	"github.com/tuannm99/testkit/reference-worker/internal/trigger/dbpoll"
	kafkatrigger "github.com/tuannm99/testkit/reference-worker/internal/trigger/kafka"
	"github.com/tuannm99/testkit/reference-worker/internal/usecase"
)

// Module is the whole worker.
var Module = fx.Options(
	fx.WithLogger(func() fxevent.Logger { return fxevent.NopLogger }),
	fx.Provide(
		config.Load,
		newLogger,
		metrics.New,
		func() clock.Clock { return clock.Real{} },
		newPool,
		newRepo,
		newPayment,
		newSearch,
		newMailer,
		newProcessor,
	),
	fx.Invoke(runHTTP, runTriggers),
)

func newLogger(c config.Config) *slog.Logger {
	l := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	l = l.With("service", c.ServiceName, "worker_id", c.WorkerID)
	if c.RunID != "" {
		l = l.With("run_id", c.RunID)
	}
	l.Info("starting", "triggers", c.Triggers, "failpoints_compiled", failpoint.Compiled, "failpoints", failpoint.Active())
	return l
}

func newPool(lc fx.Lifecycle, c config.Config) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgrepo.Open(ctx, c.DatabaseURL)
	if err != nil {
		return nil, err
	}
	lc.Append(fx.StopHook(pool.Close))
	return pool, nil
}

func newRepo(pool *pgxpool.Pool, c config.Config) *pgrepo.Repo {
	return &pgrepo.Repo{Pool: pool, OutboxTopic: c.OutboxTopic}
}

func newPayment(c config.Config, clk clock.Clock, m *metrics.Metrics, l *slog.Logger) *payment.Client {
	return &payment.Client{BaseURL: c.PaymentURL, HTTP: &http.Client{Timeout: c.PaymentTimeout},
		MaxAttempts: c.PaymentMaxAttempts, MaxBackoff: c.PaymentMaxBackoff, Clock: clk, Metrics: m, Log: l}
}

func newSearch(c config.Config, l *slog.Logger) *search.ES {
	return &search.ES{URL: c.ESURL, Index: c.ESIndex, HTTP: &http.Client{Timeout: 10 * time.Second}, Log: l}
}

func newMailer(c config.Config) *mail.SMTP { return &mail.SMTP{Addr: c.SMTPAddr, From: c.MailFrom} }

func newProcessor(c config.Config, r *pgrepo.Repo, p *payment.Client, s *search.ES, ml *mail.SMTP,
	m *metrics.Metrics, l *slog.Logger) *usecase.Processor {
	return &usecase.Processor{Orders: r, Payments: p, Search: s, Mailer: ml, WorkerID: c.WorkerID,
		OrderLease: c.OrderLease, Metrics: m, Log: l, Exit: usecase.OSExit}
}

func runHTTP(lc fx.Lifecycle, c config.Config, pool *pgxpool.Pool, repo *pgrepo.Repo, m *metrics.Metrics, l *slog.Logger) {
	srv := httpserver.New(c.HTTPAddr, pool, m, &httpserver.WebhookHandler{Secret: c.WebhookSecret, Repo: repo, Log: l.With("component", "webhook")})
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go func() {
				if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					l.Error("http server", "err", err.Error())
				}
			}()
			return nil
		},
		OnStop: srv.Shutdown,
	})
}

// runTriggers starts the configured triggers; OnStop cancels them and waits
// for in-flight jobs to finish (graceful SIGTERM).
func runTriggers(lc fx.Lifecycle, sd fx.Shutdowner, c config.Config, pool *pgxpool.Pool, proc *usecase.Processor,
	clk clock.Clock, m *metrics.Metrics, l *slog.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var clients []*kgo.Client
	start := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if err := fn(ctx); err != nil && ctx.Err() == nil {
					l.Error("trigger stopped, restarting", "trigger", name, "err", err.Error())
					_ = clk.Sleep(ctx, time.Second)
				}
			}
		}()
	}
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			if c.Has("kafka") {
				start("kafka", func(ctx context.Context) error {
					cl, err := kafkatrigger.NewClient(c.KafkaBrokers, c.KafkaGroup, c.KafkaTopic, c.KafkaSession, l)
					if err != nil {
						return err
					}
					defer cl.Close() // leaves the group; uncommitted records are redelivered
					cons := &kafkatrigger.Consumer{Client: cl, DLQTopic: c.KafkaDLQTopic, MaxAttempts: c.KafkaMaxAttempts,
						Process: proc.Process, Clock: clk, Metrics: m, Log: l.With("trigger", "kafka")}
					return cons.Run(ctx)
				})
			}
			if c.Has("dbpoll") {
				p := &dbpoll.Poller{Pool: pool, WorkerID: c.WorkerID, Interval: c.PollInterval, MaxIdle: c.PollMaxIdle,
					Batch: c.PollBatch, Lease: c.PollLease, Concurrency: c.PollConcurrency, Process: proc.Process,
					Metrics: m, Log: l.With("trigger", "dbpoll")}
				start("dbpoll", p.Run)
			}
			if c.OutboxTopic != "" && len(c.KafkaBrokers) > 0 {
				prod, err := kgo.NewClient(kgo.SeedBrokers(c.KafkaBrokers...), kgo.RequiredAcks(kgo.AllISRAcks()))
				if err != nil {
					return err
				}
				clients = append(clients, prod)
				r := &outbox.Relay{Pool: pool, Kafka: prod, Interval: c.OutboxInterval, Metrics: m, Log: l.With("component", "outbox")}
				wg.Add(1)
				go func() { defer wg.Done(); r.Run(ctx) }()
			}
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			l.Info("shutdown: draining in-flight jobs")
			cancel()
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-stopCtx.Done():
				l.Error("shutdown: in-flight jobs did not finish in time")
			}
			for _, cl := range clients {
				cl.Close()
			}
			l.Info("shutdown complete")
			return nil
		},
	})
	_ = sd
}
