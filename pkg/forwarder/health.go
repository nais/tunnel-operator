package forwarder

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type HealthServer struct {
	addr   string
	ready  atomic.Bool
	server *http.Server
}

func NewHealthServer(addr string) *HealthServer {
	hs := &HealthServer{addr: addr}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", hs.handleHealthz)
	mux.HandleFunc("/readyz", hs.handleReadyz)
	mux.Handle("/metrics", promhttp.Handler())
	hs.server = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	return hs
}

func (h *HealthServer) Start(ctx context.Context) {
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := h.server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("health server shutdown failed", "addr", h.addr, "err", err)
		}
	}()

	slog.Info("starting health server", "addr", h.addr)
	if err := h.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("health server failed", "addr", h.addr, "err", err)
	}

	<-shutdownDone
}

func (h *HealthServer) SetReady(ready bool) {
	h.ready.Store(ready)
}

func (h *HealthServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (h *HealthServer) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !h.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready\n"))
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}
