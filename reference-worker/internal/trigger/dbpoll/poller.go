// Package dbpoll is the DB-poll trigger. Jobs are claimed with
// FOR UPDATE SKIP LOCKED and a lease; a heartbeat extends the lease of long
// jobs; jobs whose lease expired (crashed worker) are rescued; every time
// comparison uses the database clock (now()), never the worker clock.
// Completion is fenced on locked_by so a worker that lost its lease cannot
// overwrite the job state of the new owner.
package dbpoll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tuannm99/testkit/reference-worker/internal/domain"
	"github.com/tuannm99/testkit/reference-worker/internal/failpoint"
	"github.com/tuannm99/testkit/reference-worker/internal/metrics"
	"github.com/tuannm99/testkit/reference-worker/internal/trigger"
)

type Poller struct {
	Pool        *pgxpool.Pool
	WorkerID    string
	Interval    time.Duration // first idle wait
	MaxIdle     time.Duration // idle backoff cap (no busy loop on an empty table)
	Batch       int
	Lease       time.Duration
	Concurrency int
	Process     trigger.ProcessFunc
	Metrics     *metrics.Metrics
	Log         *slog.Logger

	claims atomic.Int64
}

type claimed struct {
	id          int64
	key         string
	orderID     string
	attempts    int
	maxAttempts int
	rescued     bool
	age         time.Duration
	token       string // locked_by of this claim: fences completion and heartbeats
}

// claimSQL picks due jobs and expired leases in one statement.
const claimSQL = `
WITH c AS (
    SELECT id, (status = 'running') AS rescued
    FROM jobs
    WHERE (status = 'queued'  AND run_at <= %s)
       OR (status = 'running' AND lease_until < now())
    ORDER BY run_at, id
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
UPDATE jobs j
SET status = 'running', locked_by = $2, lease_until = now() + $3::interval,
    attempts = j.attempts + 1, updated_at = now()
FROM c
WHERE j.id = c.id
RETURNING j.id, j.job_key, j.order_id, j.attempts, j.max_attempts, c.rescued,
          EXTRACT(EPOCH FROM (now() - j.created_at))`

func (p *Poller) Run(ctx context.Context) error {
	sem := make(chan struct{}, p.Concurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	idle := p.Interval
	for {
		free := p.Concurrency - len(sem)
		var jobs []claimed
		var err error
		if free > 0 {
			jobs, err = p.claim(ctx, min(free, p.Batch))
		}
		if err != nil && ctx.Err() == nil {
			p.Log.Error("claim failed", "err", err.Error())
		}
		if len(jobs) == 0 {
			if free > 0 && err == nil {
				p.Metrics.PollEmpty.Inc()
			}
			wait := idle
			if failpoint.Enabled(failpoint.PollBusyLoop) {
				wait = 0
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(wait):
			}
			idle = min(idle*2, p.MaxIdle)
			continue
		}
		idle = p.Interval
		for _, j := range jobs {
			sem <- struct{}{}
			wg.Add(1)
			go func(j claimed) {
				defer func() { <-sem; wg.Done() }()
				p.run(ctx, j)
			}(j)
		}
	}
}

func (p *Poller) claim(ctx context.Context, n int) ([]claimed, error) {
	nowExpr := "now()"
	token := fmt.Sprintf("%s:%d", p.WorkerID, p.claims.Add(1))
	var args []any
	args = append(args, n, token, p.Lease.String())
	if failpoint.Enabled(failpoint.UseWorkerClock) {
		nowExpr = "$4"
		args = append(args, time.Now())
	}
	rows, err := p.Pool.Query(ctx, fmt.Sprintf(claimSQL, nowExpr), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []claimed
	for rows.Next() {
		var c claimed
		var ageSec float64
		if err := rows.Scan(&c.id, &c.key, &c.orderID, &c.attempts, &c.maxAttempts, &c.rescued, &ageSec); err != nil {
			return nil, err
		}
		c.age = time.Duration(ageSec * float64(time.Second))
		c.token = token
		p.Metrics.PollClaimed.Inc()
		if c.rescued {
			p.Metrics.PollRescued.Inc()
			p.Log.Warn("rescued job with expired lease", "job_id", c.id, "attempts", c.attempts)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// deadStatus is where a job the worker gave up on goes: the dead state, unless the
// drop_dead_letters failpoint makes it vanish as if it had succeeded.
func deadStatus() string {
	if failpoint.Enabled(failpoint.DropDeadLetters) {
		return "done"
	}
	return "dead"
}

func (p *Poller) run(ctx context.Context, j claimed) {
	// Processing is not interrupted by shutdown: finish, then release.
	pctx := context.WithoutCancel(ctx)
	log := p.Log.With("job_id", j.id, "job_key", j.key, "order_id", j.orderID, "attempt", j.attempts)
	if j.attempts > j.maxAttempts {
		p.finish(pctx, j, deadStatus(), errors.New("max attempts exceeded"), 0)
		return
	}
	hbCtx, stopHB := context.WithCancel(pctx)
	go p.heartbeat(hbCtx, j)
	err := p.Process(pctx, domain.Job{ID: j.key, OrderID: j.orderID, Source: "dbpoll", Attempt: j.attempts,
		Delivery: fmt.Sprintf("job#%d", j.id)})
	stopHB()
	switch {
	case err == nil:
		p.Metrics.E2ESeconds.WithLabelValues("dbpoll").Observe(j.age.Seconds())
		p.finish(pctx, j, "done", nil, 0)
	case domain.IsPermanent(err):
		log.Error("job failed permanently", "err", err.Error())
		p.Metrics.DLQ.Inc()
		p.finish(pctx, j, deadStatus(), err, 0)
	case j.attempts >= j.maxAttempts && !errors.Is(err, domain.ErrBusy):
		log.Error("job exhausted attempts", "err", err.Error())
		p.Metrics.DLQ.Inc()
		p.finish(pctx, j, deadStatus(), err, 0)
	default:
		backoff := time.Duration(j.attempts) * 500 * time.Millisecond
		if errors.Is(err, domain.ErrBusy) {
			backoff = time.Second
		}
		log.Warn("job failed, requeued", "err", err.Error(), "backoff", backoff.String())
		p.finish(pctx, j, "queued", err, backoff)
	}
}

func (p *Poller) finish(ctx context.Context, j claimed, status string, cause error, backoff time.Duration) {
	var msg *string
	if cause != nil {
		s := cause.Error()
		msg = &s
	}
	// Busy is not a failed attempt: give it back.
	refund := 0
	if errors.Is(cause, domain.ErrBusy) {
		refund = 1
	}
	tag, err := p.Pool.Exec(ctx, `UPDATE jobs SET status = $3, last_error = $4, locked_by = NULL, lease_until = NULL,
		run_at = now() + $5::interval, attempts = attempts - $6, updated_at = now()
		WHERE id = $1 AND locked_by = $2 AND status = 'running'`, j.id, j.token, status, msg, backoff.String(), refund)
	if err != nil {
		p.Log.Error("finish job", "job_id", j.id, "err", err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		p.Log.Warn("lease lost before completion; another worker owns the job", "job_id", j.id)
	}
}

// heartbeat extends the lease of a running job every Lease/3.
func (p *Poller) heartbeat(ctx context.Context, j claimed) {
	t := time.NewTicker(p.Lease / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tag, err := p.Pool.Exec(ctx, `UPDATE jobs SET lease_until = now() + $3::interval
				WHERE id = $1 AND locked_by = $2 AND status = 'running'`, j.id, j.token, p.Lease.String())
			if err == nil && tag.RowsAffected() == 0 {
				p.Log.Warn("heartbeat: lease lost", "job_id", j.id)
				return
			}
		}
	}
}
