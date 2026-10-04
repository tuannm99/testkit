// Package metrics declares the worker's Prometheus metrics (RED for the
// processing path, plus trigger-specific gauges). TestKit adds the run_id
// label at scrape time from the container labels.
package metrics

import "github.com/prometheus/client_golang/prometheus"

type Metrics struct {
	Reg            *prometheus.Registry
	Jobs           *prometheus.CounterVec   // source, result
	ProcessSeconds *prometheus.HistogramVec // source
	E2ESeconds     *prometheus.HistogramVec // source: enqueue -> done
	PaymentCalls   *prometheus.CounterVec   // code
	PaymentSeconds prometheus.Histogram
	PollEmpty      prometheus.Counter
	PollClaimed    prometheus.Counter
	PollRescued    prometheus.Counter
	DLQ            prometheus.Counter
	OutboxRelayed  prometheus.Counter
	MailsSent      prometheus.Counter
	InFlight       prometheus.Gauge
}

func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	buckets := []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}
	m := &Metrics{
		Reg:            reg,
		Jobs:           prometheus.NewCounterVec(prometheus.CounterOpts{Name: "worker_jobs_total", Help: "Jobs handled by result."}, []string{"source", "result"}),
		ProcessSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "worker_process_seconds", Help: "Duration of Process.", Buckets: buckets}, []string{"source"}),
		E2ESeconds:     prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "worker_e2e_seconds", Help: "Enqueue to completion.", Buckets: buckets}, []string{"source"}),
		PaymentCalls:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "worker_payment_calls_total", Help: "Payment API calls by HTTP code."}, []string{"code"}),
		PaymentSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "worker_payment_seconds", Help: "Payment API latency.", Buckets: buckets}),
		PollEmpty:      prometheus.NewCounter(prometheus.CounterOpts{Name: "worker_poll_empty_total", Help: "Polls that found no job."}),
		PollClaimed:    prometheus.NewCounter(prometheus.CounterOpts{Name: "worker_poll_claimed_total", Help: "Jobs claimed by the poller."}),
		PollRescued:    prometheus.NewCounter(prometheus.CounterOpts{Name: "worker_poll_rescued_total", Help: "Jobs reclaimed after lease expiry."}),
		DLQ:            prometheus.NewCounter(prometheus.CounterOpts{Name: "worker_dead_letter_total", Help: "Jobs dead-lettered."}),
		OutboxRelayed:  prometheus.NewCounter(prometheus.CounterOpts{Name: "worker_outbox_relayed_total", Help: "Outbox events published."}),
		MailsSent:      prometheus.NewCounter(prometheus.CounterOpts{Name: "worker_mails_sent_total", Help: "Mails sent."}),
		InFlight:       prometheus.NewGauge(prometheus.GaugeOpts{Name: "worker_in_flight", Help: "Jobs being processed."}),
	}
	reg.MustRegister(m.Jobs, m.ProcessSeconds, m.E2ESeconds, m.PaymentCalls, m.PaymentSeconds, m.PollEmpty,
		m.PollClaimed, m.PollRescued, m.DLQ, m.OutboxRelayed, m.MailsSent, m.InFlight)
	return m
}
