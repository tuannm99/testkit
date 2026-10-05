// Package httpserver exposes health and metrics.
package httpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/tuannm99/testkit/reference-worker/internal/metrics"
)

func New(addr string, pool *pgxpool.Pool, m *metrics.Metrics, webhook http.Handler) *http.Server {
	mux := http.NewServeMux()
	if webhook != nil {
		mux.Handle("/webhooks/payment", webhook)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			http.Error(w, "postgres: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /orders/{id}", orderHandler(pool))
	mux.Handle("/metrics", promhttp.HandlerFor(m.Reg, promhttp.HandlerOpts{}))
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}
