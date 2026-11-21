// Package config holds quicvoiced configuration, loaded from env vars.
package config

import (
	"time"

	"github.com/jinzhu/configor"
)

type Config struct {
	AppConfig  AppConfig
	QUICConfig QUICConfig
	HTTPConfig HTTPConfig
	AuthConfig AuthConfig
	RoomConfig RoomConfig
	// Dynamic voice rooms: auth settings + Ergo role lookups.
	ChannelConfig ChannelConfig
	// Video for voice channels (quicvideod): tickets + the control link.
	VideoConfig VideoConfig

	TelemetryConfig TelemetryConfig
}

// TelemetryConfig controls metrics export. Prometheus on HTTP /metrics is
// always on; OTLP push is on when an OTLP endpoint is set. The OTLP exporter
// also reads the other standard OTEL_EXPORTER_OTLP_* vars (headers, timeout,
// TLS, temporality) and OTEL_METRIC_EXPORT_INTERVAL itself.
type TelemetryConfig struct {
	OTLPEndpoint        string `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	OTLPMetricsEndpoint string `env:"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"`
	// http/protobuf (default) or grpc.
	OTLPProtocol        string `env:"OTEL_EXPORTER_OTLP_PROTOCOL"`
	OTLPMetricsProtocol string `env:"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"`
	// "none" turns OTLP push off even when an endpoint is set.
	MetricsExporter string `env:"OTEL_METRICS_EXPORTER"`
	// How often each connection's QUIC stats (RTT, loss) are sampled. 0 = only at close.
	QUICStatsInterval time.Duration `default:"15s" env:"QUIC_STATS_INTERVAL"`
}

type AppConfig struct {
	APPName string `default:"quicvoiced"`
	Env     string `default:"development" env:"ENV"`
	Version string `default:"dev" env:"VERSION"`
	// The boson stack config file (e.g. /config/boson.yaml); its voice
	// section beats env and is reloaded live. Empty or missing file = env
	// and defaults only.
	ConfigFile string `env:"BOSON_CONFIG"`
}

type QUICConfig struct {
	// UDP listen address for voice traffic.
	Addr string `default:":7700" env:"QUIC_ADDR"`
	// TLS cert/key (cert-manager secret mount). Empty = self-signed (dev only).
	CertFile string `env:"TLS_CERT_FILE"`
	KeyFile  string `env:"TLS_KEY_FILE"`
	// Hostname used for the self-signed dev cert.
	DevHostname    string        `default:"localhost" env:"DEV_HOSTNAME"`
	MaxIdleTimeout time.Duration `default:"30s" env:"QUIC_MAX_IDLE_TIMEOUT"`
	KeepAlive      time.Duration `default:"10s" env:"QUIC_KEEPALIVE"`
	// How long a new connection has to send Hello.
	HelloTimeout time.Duration `default:"5s" env:"HELLO_TIMEOUT"`
	// Pages allowed to open WebTransport sessions (https://host:port/wt on
	// the QUIC port): comma-separated origins like https://chat.boson.chat.
	// Empty or "*" = any page; the token in Hello is what authorizes.
	WebTransportOrigins string `env:"WEBTRANSPORT_ORIGINS"`
}

// VideoConfig turns on video in voice channels (docs: quicvideo
// docs/voice-merge.md). Without VIDEO_QUIC_URL, ticket requests get an error.
type VideoConfig struct {
	// Where clients reach quicvideod: native (host:port) and browsers.
	QUICURL         string `env:"VIDEO_QUIC_URL"`
	WebTransportURL string `env:"VIDEO_WEBTRANSPORT_URL"`
	// Ed25519 key for tickets (PEM seed; created on first start). Empty =
	// in-memory key (dev only: tickets stop verifying after a restart).
	TicketKeyFile string        `env:"VIDEO_TICKET_KEY_FILE"`
	TicketIssuer  string        `default:"voice" env:"VIDEO_TICKET_ISSUER"`
	TicketTTL     time.Duration `default:"5m" env:"VIDEO_TICKET_TTL"`
	// quicvideod's control listener and its bearer token. Empty = no link
	// (leaving voice then only stops video when the ticket expires).
	ControlURL   string `env:"VIDEO_CONTROL_URL"`
	ControlToken string `env:"VIDEO_CONTROL_TOKEN"`
}

type HTTPConfig struct {
	// Health + metrics. Cluster-internal only.
	Addr string `default:":8080" env:"HTTP_ADDR"`
	// Public API (GET /v1/rooms, /healthz), meant to sit behind a TLS
	// reverse proxy. Never serves /metrics. Empty = off.
	PublicAddr string `default:":8090" env:"PUBLIC_HTTP_ADDR"`
	// Origins whose pages may call the public API (CORS): comma-separated,
	// "*" = any. Empty = WEBTRANSPORT_ORIGINS (and so any, when that is
	// empty too). Requests carry a bearer token, never cookies, so CORS
	// never allows credentials.
	CORSAllowedOrigins string `env:"CORS_ALLOWED_ORIGINS"`
}

// CORSOrigins is the effective CORS allow-list of the public API.
func (c Config) CORSOrigins() string {
	if c.HTTPConfig.CORSAllowedOrigins != "" {
		return c.HTTPConfig.CORSAllowedOrigins
	}
	return c.QUICConfig.WebTransportOrigins
}

type AuthConfig struct {
	// HS256 secret shared with the app backend that mints room tokens.
	JWTSecret string `env:"JWT_SECRET"`
	// Accept any token as the user id. Dev/testing only.
	Insecure bool `default:"false" env:"AUTH_INSECURE"`
	// boson auth service: verify its EdDSA tokens via JWKS, e.g.
	// https://auth.example.com/.well-known/jwks.json. Can be combined with
	// JWT_SECRET during a migration (routed by the token's alg).
	JWKSURL  string `env:"AUTH_JWKS_URL"`
	Issuer   string `env:"AUTH_ISSUER"`
	Audience string `default:"quicvoice" env:"AUTH_AUDIENCE"`
}

type RoomConfig struct {
	MaxParticipants int `default:"50" env:"ROOM_MAX_PARTICIPANTS"`
	// Uplink datagrams per second per sender before dropping (20ms frames = 50/s).
	MaxDatagramsPerSec int `default:"60" env:"MAX_DATAGRAMS_PER_SEC"`
	MaxPayload         int `default:"1000" env:"MAX_DATAGRAM_PAYLOAD"`
	// Configured rooms, in display order: comma-separated `id:Display Name`.
	// Ids are lowercase [a-z0-9-]{1,32}.
	Rooms string `default:"general:General" env:"VOICE_ROOMS"`
	// Only configured rooms can be joined. false = any other name creates an
	// ad-hoc room (listed after the configured ones).
	Strict bool `default:"true" env:"VOICE_ROOMS_STRICT"`
}

// ChannelConfig is the dynamic voice room registry and its permission sources.
type ChannelConfig struct {
	// Room registry file (JSON, rewritten atomically). Empty = in memory only:
	// rooms created through the API are lost on restart.
	StorePath string `env:"VOICE_ROOMS_DB"`
	// Auth service internal listener, for GET /internal/v1/settings
	// (voice_channel_creation). Empty = VOICE_CHANNEL_CREATION only.
	AuthInternalURL  string `env:"AUTH_INTERNAL_URL"`
	InternalAPIToken string `env:"INTERNAL_API_TOKEN"`
	// Used before the first successful settings fetch (and always without
	// AUTH_INTERNAL_URL): admins | founders | registered.
	CreationFallback string        `default:"admins" env:"VOICE_CHANNEL_CREATION"`
	SettingsTTL      time.Duration `default:"30s" env:"SETTINGS_TTL"`
	// Ergo HTTP API (api section of ircd.yaml), for founder checks and
	// channel roles. Empty = no IRC roles: only admins pass role locks and
	// the founders policy.
	ErgoAPIURL   string        `env:"ERGO_API_URL"`
	ErgoAPIToken string        `env:"ERGO_API_TOKEN"`
	ErgoCacheTTL time.Duration `default:"15s" env:"ERGO_CACHE_TTL"`
	// Dynamic rooms per (non-admin) owner, and in total.
	MaxPerUser int `default:"10" env:"VOICE_ROOMS_MAX_PER_USER"`
	MaxRooms   int `default:"500" env:"VOICE_ROOMS_MAX"`
}

func LoadConfig() (Config, error) {
	var cfg Config
	err := configor.New(&configor.Config{ENVPrefix: "-", Silent: true}).Load(&cfg)
	return cfg, err
}

func LoadConfigOrPanic() Config {
	cfg, err := LoadConfig()
	if err != nil {
		panic(err)
	}
	return cfg
}
