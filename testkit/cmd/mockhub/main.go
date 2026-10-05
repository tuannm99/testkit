// Command mockhub serves every third-party mock used by services under test:
// HTTP/webhook (namespaced, scripted, journaled, schema-validated), an SMTP
// server with fault injection (relays to Mailpit), and WebSocket/TCP partners.
//
//	:8081  HTTP data plane + control plane (/_mock/...)
//	:2525  SMTP
//	:8082  WebSocket partners (ws://mockhub:8082/ns/{ns}/{mock}); TCP partners get their own port
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	httpmock "github.com/tuannm99/testkit/testkit/adapters/mock/http"
	"github.com/tuannm99/testkit/testkit/adapters/mock/smtp"
	"github.com/tuannm99/testkit/testkit/adapters/mock/socket"
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
	smtpSrv := smtp.NewServer(hub, env("MOCKHUB_MAIL_RELAY", ""), log.With("part", "smtp"))
	sockSrv := socket.NewServer(hub, log.With("part", "socket"))

	mux := http.NewServeMux()
	mux.Handle("/", hub.Handler())
	mux.HandleFunc("PUT /_mock/ns/{ns}/smtp/script", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Behaviours []string `json:"behaviours"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := smtpSrv.SetScript(r.PathValue("ns"), body.Behaviours); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"status": "scripted"})
	})
	mux.HandleFunc("PUT /_mock/ns/{ns}/sockets/{mock}", func(w http.ResponseWriter, r *http.Request) {
		var cfg socket.Config
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		port, err := sockSrv.Configure(r.PathValue("ns"), r.PathValue("mock"), cfg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"status": "configured", "port": port})
	})
	mux.HandleFunc("DELETE /_mock/ns/{ns}", func(w http.ResponseWriter, r *http.Request) {
		ns := r.PathValue("ns")
		hub.Reset(ns)
		smtpSrv.Reset(ns)
		sockSrv.Reset(ns)
		writeJSON(w, map[string]string{"status": "reset"})
	})

	servers := []*http.Server{
		{Addr: env("MOCKHUB_HTTP_ADDR", ":8081"), Handler: mux, ReadHeaderTimeout: 10 * time.Second},
		{Addr: env("MOCKHUB_SOCKET_ADDR", ":8082"), Handler: sockSrv, ReadHeaderTimeout: 10 * time.Second},
	}
	for _, srv := range servers {
		go func(srv *http.Server) {
			log.Info("mockhub listening", "addr", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("http server", "addr", srv.Addr, "err", err)
				os.Exit(1)
			}
		}(srv)
	}
	sl, err := net.Listen("tcp", env("MOCKHUB_SMTP_ADDR", ":2525"))
	if err != nil {
		log.Error("smtp listen", "err", err)
		os.Exit(1)
	}
	go func() { _ = smtpSrv.Serve(ctx, sl) }()
	log.Info("mockhub smtp listening", "addr", sl.Addr().String())

	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(sctx)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
