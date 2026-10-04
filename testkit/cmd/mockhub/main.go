// Command mockhub serves every third-party mock used by services under test:
// HTTP/webhook (namespaced, scripted, journaled, schema-validated), and in
// later phases SMTP faults and WebSocket/TCP socket scripts.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	httpmock "github.com/tuannm99/testkit/testkit/adapters/mock/http"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub := httpmock.NewHub(env("MOCKHUB_SPEC_DIR", "/specs"))
	srv := &http.Server{Addr: env("MOCKHUB_HTTP_ADDR", ":8081"), Handler: hub.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("mockhub listening", "http", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http server", "err", err)
		os.Exit(1)
	}
}
