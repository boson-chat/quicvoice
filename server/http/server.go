// Package http serves the cluster-internal health/metrics listener and the
// public API listener. They are separate so /metrics is never exposed.
package http

import (
	"context"
	"encoding/json"
	"errors"
	nethttp "net/http"
	"time"

	"github.com/boson-chat/quicvoice/server/http/handlers"
	"github.com/boson-chat/quicvoice/server/internal/configfile"
	"github.com/boson-chat/quicvoice/server/internal/logger"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/services/videoticket"
	"github.com/boson-chat/quicvoice/server/internal/services/voicechannel"
)

// ConfigStatus reports the config file's state (configfile.Watcher).
type ConfigStatus interface {
	Status() configfile.Status
}

// NewRouter serves /healthz, /version, on /metrics metricsHandler (the
// OTel Prometheus exporter's registry) and GET /internal/config/status
// (cfg; nil = no config file). Cluster-internal only.
func NewRouter(version string, metricsHandler nethttp.Handler, cfg ConfigStatus) nethttp.Handler {
	mux := nethttp.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /internal/config/status", func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		st := configfile.Status{OK: true, RestartRequired: []string{}}
		if cfg != nil {
			st = cfg.Status()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("GET /version", func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		_, _ = w.Write([]byte(version))
	})
	mux.Handle("GET /metrics", metricsHandler)
	return mux
}

// NewPublicRouter serves the public API: the voice room list and registry
// (GET/POST /v1/rooms, PATCH/DELETE /v1/rooms/{id}), GET /healthz and, with
// video tickets, the ticket JWKS (GET /.well-known/video-jwks.json) that
// quicvideod verifies against.
func NewPublicRouter(a auth.AuthServiceImpl, r room.RoomServiceImpl, vc voicechannel.VoiceChannelServiceImpl, tickets ...videoticket.VideoTicketServiceImpl) nethttp.Handler {
	rooms := &handlers.RoomsHandler{Auth: a, Rooms: r, Channels: vc}
	mux := nethttp.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /v1/rooms", rooms.List)
	mux.HandleFunc("POST /v1/rooms", rooms.Create)
	mux.HandleFunc("PATCH /v1/rooms/{id}", rooms.Update)
	mux.HandleFunc("DELETE /v1/rooms/{id}", rooms.Delete)
	if len(tickets) > 0 && tickets[0] != nil {
		t := tickets[0]
		mux.HandleFunc("GET /.well-known/video-jwks.json", func(w nethttp.ResponseWriter, _ *nethttp.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "max-age=300")
			_ = json.NewEncoder(w).Encode(t.JWKS())
		})
	}
	return mux
}

func healthz(w nethttp.ResponseWriter, _ *nethttp.Request) {
	_, _ = w.Write([]byte("ok"))
}

// StartServerWithContext serves the internal router until ctx is cancelled.
func StartServerWithContext(ctx context.Context, addr, version string, metricsHandler nethttp.Handler, cfg ConfigStatus) error {
	return serve(ctx, "HTTP", &nethttp.Server{
		Addr: addr, Handler: NewRouter(version, metricsHandler, cfg), ReadHeaderTimeout: 5 * time.Second,
	})
}

// StartPublicServerWithContext serves the public router until ctx is cancelled.
func StartPublicServerWithContext(ctx context.Context, addr string, a auth.AuthServiceImpl, r room.RoomServiceImpl, vc voicechannel.VoiceChannelServiceImpl, tickets ...videoticket.VideoTicketServiceImpl) error {
	return serve(ctx, "public HTTP", &nethttp.Server{
		Addr:              addr,
		Handler:           NewPublicRouter(a, r, vc, tickets...),
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
