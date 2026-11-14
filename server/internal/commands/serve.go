package commands

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/boson-chat/quicvoice/server/config"
	"github.com/boson-chat/quicvoice/server/http"
	"github.com/boson-chat/quicvoice/server/internal/certs"
	"github.com/boson-chat/quicvoice/server/internal/logger"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/transport"
	"github.com/boson-chat/quicvoice/server/metrics"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the QUIC voice server, the public HTTP API and the health/metrics HTTP server",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.LoadConfigOrPanic()

		l := logger.Logger(
			logger.WithServerName(cfg.AppConfig.APPName),
			logger.WithVersion(cfg.AppConfig.Version),
			logger.WithEnvironment(cfg.AppConfig.Env),
		)

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		ctx = logger.WithLogger(ctx, l)

		// 0. Telemetry
		provider, err := metrics.NewProvider(ctx, cfg)
		if err != nil {
			return err
		}
		defer shutdownMetrics(ctx, provider)
		if p := provider.OTLPProtocol(); p != "" {
			l.Info().Str("protocol", p).Msg("OTLP metrics push enabled")
		}
		m, err := metrics.New(provider.MeterProvider())
		if err != nil {
			return err
		}

		// 1. Services
		authService, err := buildAuth(ctx, cfg)
		if err != nil {
			return err
		}
		if cfg.AuthConfig.Insecure {
			l.Warn().Msg("AUTH_INSECURE is on: any token is accepted")
		}
		roomDefs, err := room.ParseDefs(cfg.RoomConfig.Rooms)
		if err != nil {
			return fmt.Errorf("VOICE_ROOMS: %w", err)
		}
		roomService := room.NewRoomService(cfg.RoomConfig.MaxParticipants, m,
			room.WithRooms(roomDefs, cfg.RoomConfig.Strict))
		l.Info().Int("rooms", len(roomDefs)).Bool("strict", cfg.RoomConfig.Strict).Msg("voice rooms configured")

		// 2. Transport
		tlsConf, err := buildTLS(cfg)
		if err != nil {
			return err
		}
		quicServer := transport.NewServer(cfg, tlsConf, authService, roomService, m)

		g, gctx := errgroup.WithContext(ctx)
		g.Go(func() error { return quicServer.Serve(gctx) })
		g.Go(func() error {
			return http.StartServerWithContext(gctx, cfg.HTTPConfig.Addr, cfg.AppConfig.Version, provider.Handler())
		})
		if addr := cfg.HTTPConfig.PublicAddr; addr != "" {
			g.Go(func() error { return http.StartPublicServerWithContext(gctx, addr, authService, roomService) })
		}
		return g.Wait()
	},
}

// shutdownMetrics flushes pending OTLP exports. ctx is already cancelled on
// SIGTERM, so it gets its own deadline.
func shutdownMetrics(ctx context.Context, p *metrics.Provider) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := p.Shutdown(sctx); err != nil {
		logger.FromCtx(ctx).Warn().Err(err).Msg("metrics shutdown")
	}
}

func buildAuth(ctx context.Context, cfg config.Config) (auth.AuthServiceImpl, error) {
	a := cfg.AuthConfig
	if a.Insecure {
		return auth.NewInsecureAuthService(), nil
	}
	comp := &auth.CompositeAuthService{}
	if a.JWTSecret != "" {
		comp.HMAC = auth.NewJWTAuthService(a.JWTSecret)
	}
	if a.JWKSURL != "" {
		j, err := auth.NewJWKSAuthService(ctx, a.JWKSURL, a.Issuer, a.Audience)
		if err != nil {
			return nil, err
		}
		comp.JWKS = j
	}
	if comp.HMAC == nil && comp.JWKS == nil {
		return nil, errors.New("set AUTH_JWKS_URL and/or JWT_SECRET, or AUTH_INSECURE=true for testing")
	}
	return comp, nil
}

func buildTLS(cfg config.Config) (*tls.Config, error) {
	q := cfg.QUICConfig
	if q.CertFile != "" {
		src, err := certs.NewFileSource(q.CertFile, q.KeyFile)
		if err != nil {
			return nil, err
		}
		return &tls.Config{GetCertificate: src.GetCertificate}, nil
	}
	c, err := certs.SelfSigned(q.DevHostname)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{c}}, nil
}

func init() {
	rootCmd.AddCommand(serveCmd)
}
