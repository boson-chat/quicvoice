package commands

import (
	"context"
	"crypto/tls"
	"errors"
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
	Short: "Start the QUIC voice server and the health/metrics HTTP server",
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
		authService, err := buildAuth(cfg)
		if err != nil {
			return err
		}
		if cfg.AuthConfig.Insecure {
			l.Warn().Msg("AUTH_INSECURE is on: any token is accepted")
		}
		roomService := room.NewRoomService(cfg.RoomConfig.MaxParticipants, m)

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

func buildAuth(cfg config.Config) (auth.AuthServiceImpl, error) {
	switch {
	case cfg.AuthConfig.Insecure:
		return auth.NewInsecureAuthService(), nil
	case cfg.AuthConfig.JWTSecret != "":
		return auth.NewJWTAuthService(cfg.AuthConfig.JWTSecret), nil
	default:
		return nil, errors.New("set JWT_SECRET, or AUTH_INSECURE=true for testing")
	}
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
