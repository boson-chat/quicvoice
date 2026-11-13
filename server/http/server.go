// Package http serves health checks and Prometheus metrics (cluster-internal).
package http

import (
	"context"
	"errors"
	nethttp "net/http"
	"time"

	"github.com/boson-chat/quicvoice/server/internal/logger"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func NewRouter(version string) nethttp.Handler {
	mux := nethttp.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /version", func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		_, _ = w.Write([]byte(version))
	})
	mux.Handle("GET /metrics", promhttp.Handler())
	return mux
}

// StartServerWithContext serves until ctx is cancelled.
func StartServerWithContext(ctx context.Context, addr, version string) error {
	srv := &nethttp.Server{Addr: addr, Handler: NewRouter(version), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	logger.FromCtx(ctx).Info().Str("addr", addr).Msg("HTTP listening")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, nethttp.ErrServerClosed) {
		return err
	}
	return nil
}
