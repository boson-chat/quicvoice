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
	"github.com/boson-chat/quicvoice/server/internal/db/repositories/voiceroom"
	"github.com/boson-chat/quicvoice/server/internal/logger"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/ergo"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/services/settings"
	"github.com/boson-chat/quicvoice/server/internal/services/voicechannel"
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
		channels, err := buildChannels(ctx, cfg, roomDefs, m)
		if err != nil {
			return err
		}
		roomService := room.NewRoomService(cfg.RoomConfig.MaxParticipants, m,
			room.WithDirectory(channels, cfg.RoomConfig.Strict), room.WithJoinPolicy(channels))
		l.Info().Int("static", len(roomDefs)).Int("total", len(channels.Defs())).Bool("strict", cfg.RoomConfig.Strict).
			Str("store", cfg.ChannelConfig.StorePath).Bool("ergo_api", cfg.ChannelConfig.ErgoAPIURL != "").
			Bool("auth_settings", cfg.ChannelConfig.AuthInternalURL != "").Msg("voice rooms configured")

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
			g.Go(func() error { return http.StartPublicServerWithContext(gctx, addr, authService, roomService, channels) })
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

func buildChannels(ctx context.Context, cfg config.Config, static []room.Def, m *metrics.Metrics) (*voicechannel.VoiceChannelService, error) {
	c := cfg.ChannelConfig
	fallback, err := settings.ParseVoiceCreation(c.CreationFallback)
	if err != nil {
		return nil, fmt.Errorf("VOICE_CHANNEL_CREATION: %w", err)
	}
	if c.ErgoAPIURL != "" && c.ErgoAPIToken == "" {
		// e.g. a stack without the irc profile: run without IRC roles.
		logger.FromCtx(ctx).Warn().Msg("ERGO_API_URL is set but ERGO_API_TOKEN is empty: IRC founder/role lookups are off")
		c.ErgoAPIURL = ""
	}
	repo, err := voiceroom.NewRoomRepository(c.StorePath)
	if err != nil {
		return nil, fmt.Errorf("VOICE_ROOMS_DB: %w", err)
	}
	return voicechannel.NewVoiceChannelService(ctx, voicechannel.Options{
		Repo: repo,
		Settings: settings.NewSettingsService(settings.Options{
			URL: c.AuthInternalURL, Token: c.InternalAPIToken, Fallback: fallback, TTL: c.SettingsTTL,
		}),
		Ergo:       ergo.NewErgoService(ergo.Options{URL: c.ErgoAPIURL, Token: c.ErgoAPIToken, TTL: c.ErgoCacheTTL}),
		Metrics:    m,
		Static:     static,
		MaxPerUser: c.MaxPerUser,
		MaxRooms:   c.MaxRooms,
	})
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
