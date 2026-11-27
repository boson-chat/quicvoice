package commands

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/boson-chat/quicvoice/server/config"
	"github.com/boson-chat/quicvoice/server/http"
	"github.com/boson-chat/quicvoice/server/internal/certs"
	"github.com/boson-chat/quicvoice/server/internal/configfile"
	"github.com/boson-chat/quicvoice/server/internal/db/repositories/voiceroom"
	"github.com/boson-chat/quicvoice/server/internal/logger"
	"github.com/boson-chat/quicvoice/server/internal/origins"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/ergo"
	"github.com/boson-chat/quicvoice/server/internal/services/liveconfig"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/services/settings"
	"github.com/boson-chat/quicvoice/server/internal/services/videolink"
	"github.com/boson-chat/quicvoice/server/internal/services/videoticket"
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
		base := config.LoadConfigOrPanic()
		// The stack config file's voice section (BOSON_CONFIG) on top of env.
		// An invalid file at start: run on env and report it on
		// /internal/config/status, like an invalid reload.
		file, fileErr := config.ReadVoiceFile(base.AppConfig.ConfigFile)
		cfg := file.Effective(base)

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
		if fileErr != nil {
			l.Error().Err(fileErr).Msg("config file invalid; starting from env and defaults")
		}
		roomDefs, err := liveconfig.StaticDefs(base, file)
		if err != nil {
			return err
		}
		channels, settingsService, ergoService, err := buildChannels(ctx, cfg, roomDefs, m)
		if err != nil {
			return err
		}
		// Video for voice channels: tickets, and the link that keeps
		// quicvideod's rooms in step with voice channels.
		var tickets *videoticket.VideoTicketService
		var link *videolink.VideoLinkService
		var roomService room.RoomServiceImpl
		roomOpts := []room.Option{room.WithDirectory(channels, cfg.RoomConfig.Strict), room.WithJoinPolicy(channels)}
		if vc := cfg.VideoConfig; vc.QUICURL != "" {
			if tickets, err = videoticket.New(videoticket.Options{KeyFile: vc.TicketKeyFile, Issuer: vc.TicketIssuer, TTL: vc.TicketTTL}); err != nil {
				return err
			}
			if vc.TicketKeyFile == "" {
				l.Warn().Msg("VIDEO_TICKET_KEY_FILE unset: video tickets use an in-memory key")
			}
			if vc.ControlURL != "" {
				link = videolink.New(videolink.Options{URL: vc.ControlURL, Token: vc.ControlToken,
					Snapshot: func() map[string][]string { return roomService.Snapshot() }})
				roomOpts = append(roomOpts, room.WithObserver(link))
			}
			l.Info().Str("quic", vc.QUICURL).Bool("control_link", link != nil).Msg("video for voice channels on")
		}
		roomService = room.NewRoomService(cfg.RoomConfig.MaxParticipants, m, roomOpts...)
		l.Info().Int("static", len(roomDefs)).Int("total", len(channels.Defs())).Bool("strict", cfg.RoomConfig.Strict).
			Str("store", cfg.ChannelConfig.StorePath).Bool("ergo_api", cfg.ChannelConfig.ErgoAPIURL != "").
			Bool("auth_settings", cfg.ChannelConfig.AuthInternalURL != "").Msg("voice rooms configured")

		applier := liveconfig.New(liveconfig.Options{
			Base: base, Running: cfg, RunningRooms: roomDefs,
			Rooms: roomService, Channels: channels, Settings: settingsService, Ergo: ergoService,
		})
		watcher := configfile.New(base.AppConfig.ConfigFile, applier.Load, configfile.WithLogger(l))
		_ = watcher.Reload(ctx) // status for the file as started (logged on error)
		if p := base.AppConfig.ConfigFile; p != "" {
			l.Info().Str("path", p).Msg("watching the config file")
		}

		// 2. Transport
		if q := cfg.QUICConfig; q.CertFile != "" {
			err := certs.WaitForFiles(ctx, q.CertFile, q.KeyFile, 2*time.Second, func(reason error) {
				l.Warn().Err(reason).Str("cert", q.CertFile).Msg("waiting for the TLS certificate files (certsync exports them once the certificate exists)")
			})
			if err != nil {
				return err
			}
		}
		tlsConf, err := buildTLS(cfg)
		if err != nil {
			return err
		}
		webOrigins, wtOrigins, corsOrigins, err := buildOrigins(ctx, cfg)
		if err != nil {
			return err
		}
		quicServer := transport.NewServer(cfg, tlsConf, authService, roomService, m)
		quicServer.SetOrigins(wtOrigins)
		if tickets != nil {
			quicServer.SetVideoTickets(tickets)
		}

		g, gctx := errgroup.WithContext(ctx)
		g.Go(func() error { return quicServer.Serve(gctx) })
		g.Go(func() error { watcher.Run(gctx); return nil })
		if link != nil {
			g.Go(func() error { link.Run(gctx); return nil })
		}
		if webOrigins != nil {
			g.Go(func() error { webOrigins.Run(gctx); return nil })
		}
		g.Go(func() error {
			return http.StartServerWithContext(gctx, cfg.HTTPConfig.Addr, cfg.AppConfig.Version, provider.Handler(), watcher)
		})
		if addr := cfg.HTTPConfig.PublicAddr; addr != "" {
			var jwks videoticket.VideoTicketServiceImpl
			if tickets != nil {
				jwks = tickets
			}
			g.Go(func() error {
				opts := http.PublicOptions{CORSOrigins: cfg.CORSOrigins(), Origins: corsOrigins, QUICPort: quicPort(cfg.QUICConfig.Addr)}
				if cfg.QUICConfig.CertFile == "" {
					opts.DevCertHash = devCertHash(tlsConf)
				}
				return http.StartPublicServerWithContext(gctx, addr, opts, authService, roomService, channels, jwks)
			})
		}
		return g.Wait()
	},
}

// buildOrigins parses WEBTRANSPORT_ORIGINS and the public API's CORS list.
// When either says "directory" they share one fetcher of ORIGINS_URL
// (returned, to be run), which is then required.
func buildOrigins(ctx context.Context, cfg config.Config) (*origins.Fetcher, *origins.Policy, *origins.Policy, error) {
	q := cfg.QUICConfig
	var f *origins.Fetcher
	if origins.UsesDirectory(q.WebTransportOrigins) || origins.UsesDirectory(cfg.CORSOrigins()) {
		if strings.TrimSpace(q.OriginsURL) == "" {
			return nil, nil, nil, errors.New("WEBTRANSPORT_ORIGINS / CORS_ALLOWED_ORIGINS say directory: set ORIGINS_URL (the auth service's /v1/web-origins, e.g. http://auth:8080/v1/web-origins)")
		}
		f = origins.NewFetcher(strings.TrimSpace(q.OriginsURL), q.OriginsRefresh, nil)
		logger.FromCtx(ctx).Info().Str("url", q.OriginsURL).Dur("refresh", q.OriginsRefresh).
			Msg("web origins: following the directory's Boson web clients")
	}
	parse := func(name, list string) *origins.Policy {
		p, err := origins.Parse(list, f)
		if err != nil {
			logger.FromCtx(ctx).Warn().Err(err).Str("list", list).Msgf("%s: entry ignored", name)
			p = origins.MustParse(list, f)
		}
		return p
	}
	return f, parse("WEBTRANSPORT_ORIGINS", q.WebTransportOrigins), parse("CORS_ALLOWED_ORIGINS", cfg.CORSOrigins()), nil
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

// buildChannels returns the room registry and the settings and Ergo services
// it uses (their cache TTLs follow the config file).
func buildChannels(ctx context.Context, cfg config.Config, static []room.Def, m *metrics.Metrics) (
	*voicechannel.VoiceChannelService, liveconfig.TTLSetter, liveconfig.TTLSetter, error) {
	c := cfg.ChannelConfig
	fallback, err := settings.ParseVoiceCreation(c.CreationFallback)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("VOICE_CHANNEL_CREATION: %w", err)
	}
	if c.ErgoAPIURL != "" && c.ErgoAPIToken == "" {
		// e.g. a stack without the irc profile: run without IRC roles.
		logger.FromCtx(ctx).Warn().Msg("ERGO_API_URL is set but ERGO_API_TOKEN is empty: IRC founder/role lookups are off")
		c.ErgoAPIURL = ""
	}
	repo, err := voiceroom.NewRoomRepository(c.StorePath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("VOICE_ROOMS_DB: %w", err)
	}
	st := settings.NewSettingsService(settings.Options{
		URL: c.AuthInternalURL, Token: c.InternalAPIToken, Fallback: fallback, TTL: c.SettingsTTL,
	})
	er := ergo.NewErgoService(ergo.Options{URL: c.ErgoAPIURL, Token: c.ErgoAPIToken, TTL: c.ErgoCacheTTL})
	vc, err := voicechannel.NewVoiceChannelService(ctx, voicechannel.Options{
		Repo:       repo,
		Settings:   st,
		Ergo:       er,
		Metrics:    m,
		Static:     static,
		MaxPerUser: c.MaxPerUser,
		MaxRooms:   c.MaxRooms,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	sTTL, _ := st.(liveconfig.TTLSetter)
	eTTL, _ := er.(liveconfig.TTLSetter)
	return vc, sTTL, eTTL, nil
}

// devCertHash is the hex SHA-256 of the self-signed dev certificate.
func devCertHash(c *tls.Config) string {
	if c == nil || len(c.Certificates) == 0 || len(c.Certificates[0].Certificate) == 0 {
		return ""
	}
	sum := sha256.Sum256(c.Certificates[0].Certificate[0])
	return hex.EncodeToString(sum[:])
}

func quicPort(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
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
