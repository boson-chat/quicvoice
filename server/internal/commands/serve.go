package commands

import (
	"context"
	"crypto/tls"
	"errors"
	"os/signal"
	"syscall"

	"github.com/boson-chat/quicvoice/server/config"
	"github.com/boson-chat/quicvoice/server/http"
	"github.com/boson-chat/quicvoice/server/internal/certs"
	"github.com/boson-chat/quicvoice/server/internal/logger"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/transport"
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

		// 1. Services
		authService, err := buildAuth(cfg)
		if err != nil {
			return err
		}
		if cfg.AuthConfig.Insecure {
			l.Warn().Msg("AUTH_INSECURE is on: any token is accepted")
		}
		roomService := room.NewRoomService(cfg.RoomConfig.MaxParticipants)

		// 2. Transport
		tlsConf, err := buildTLS(cfg)
		if err != nil {
			return err
		}
		quicServer := transport.NewServer(cfg, tlsConf, authService, roomService)

		g, gctx := errgroup.WithContext(ctx)
		g.Go(func() error { return quicServer.Serve(gctx) })
		g.Go(func() error { return http.StartServerWithContext(gctx, cfg.HTTPConfig.Addr, cfg.AppConfig.Version) })
		return g.Wait()
	},
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
