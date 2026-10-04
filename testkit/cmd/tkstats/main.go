// Command tkstats exports resource usage of TestKit-managed containers in
// Prometheus format (see adapters/collect/dockerstats). Needs docker.sock.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tuannm99/testkit/testkit/adapters/collect/dockerstats"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sock := os.Getenv("DOCKER_SOCK")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	x := &dockerstats.Exporter{E: dockerstats.NewEngine(sock), Selector: "testkit.managed=true"}
	go x.Poll(ctx, 2*time.Second)
	mux := http.NewServeMux()
	mux.Handle("/metrics", x)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	slog.Info("tkstats listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}
