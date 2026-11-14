// Package http serves the cluster-internal health/metrics listener and the
// public API listener. They are separate so /metrics is never exposed.
package http

import (
	"context"
	"errors"
	nethttp "net/http"
	"time"

	"github.com/boson-chat/quicvoice/server/http/handlers"
	"github.com/boson-chat/quicvoice/server/internal/logger"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
)

// NewRouter serves /healthz, /version and, on /metrics, metricsHandler (the
// OTel Prometheus exporter's registry). Cluster-internal only.
func NewRouter(version string, metricsHandler nethttp.Handler) nethttp.Handler {
	mux := nethttp.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /version", func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		_, _ = w.Write([]byte(version))
	})
	mux.Handle("GET /metrics", metricsHandler)
	return mux
}

// NewPublicRouter serves the public API: GET /v1/rooms and GET /healthz.
func NewPublicRouter(a auth.AuthServiceImpl, r room.RoomServiceImpl) nethttp.Handler {
	rooms := &handlers.RoomsHandler{Auth: a, Rooms: r}
	mux := nethttp.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /v1/rooms", rooms.List)
	return mux
}

func healthz(w nethttp.ResponseWriter, _ *nethttp.Request) {
	_, _ = w.Write([]byte("ok"))
}

// StartServerWithContext serves the internal router until ctx is cancelled.
func StartServerWithContext(ctx context.Context, addr, version string, metricsHandler nethttp.Handler) error {
	return serve(ctx, "HTTP", &nethttp.Server{
		Addr: addr, Handler: NewRouter(version, metricsHandler), ReadHeaderTimeout: 5 * time.Second,
	})
}

// StartPublicServerWithContext serves the public router until ctx is cancelled.
func StartPublicServerWithContext(ctx context.Context, addr string, a auth.AuthServiceImpl, r room.RoomServiceImpl) error {
	return serve(ctx, "public HTTP", &nethttp.Server{
		Addr:              addr,
		Handler:           NewPublicRouter(a, r),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	})
}

func serve(ctx context.Context, name string, srv *nethttp.Server) error {
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	logger.FromCtx(ctx).Info().Str("addr", srv.Addr).Msg(name + " listening")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, nethttp.ErrServerClosed) {
		return err
	}
	return nil
}
