// Package server exposes the HTTP endpoints.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/container-image-exporter/container-image-exporter/internal/metrics"
)

// NewHandler returns the exporter's HTTP handler.
//
//	/metrics  the two metric families; 503 while no reliable snapshot exists
//	/healthz  liveness, always 200
//	/readyz   readiness, 200 once a snapshot exists
func NewHandler(source func() *metrics.Snapshot) http.Handler {
	// HandlerFor on a dedicated registry: no promhttp_* instrumentation.
	metricsHandler := promhttp.HandlerFor(metrics.NewRegistry(source), promhttp.HandlerOpts{})

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		// Before the first complete scan, and while the runtime inventory
		// is unknown, an empty 200 response would look like "no containers
		// running". Fail the scrape instead so that "up" becomes 0.
		if source() == nil {
			http.Error(w, "no reliable scan result available yet", http.StatusServiceUnavailable)
			return
		}
		metricsHandler.ServeHTTP(w, r)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if source() == nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ready")
	})
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "container-image-exporter: see /metrics")
	})
	return mux
}

// Run serves handler on addr until ctx is canceled, then shuts down
// gracefully.
func Run(ctx context.Context, addr string, handler http.Handler, logger *slog.Logger) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(listener) }()
	logger.Info("listening", "address", listener.Addr().String())

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http shutdown: %w", err)
	}
	return nil
}
