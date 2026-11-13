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
}

type AppConfig struct {
	APPName string `default:"quicvoiced"`
	Env     string `default:"development" env:"ENV"`
	Version string `default:"dev" env:"VERSION"`
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
}

type HTTPConfig struct {
	// Health + metrics. Cluster-internal only.
	Addr string `default:":8080" env:"HTTP_ADDR"`
}

type AuthConfig struct {
	// HS256 secret shared with the app backend that mints room tokens.
	JWTSecret string `env:"JWT_SECRET"`
	// Accept any token as the user id. Dev/testing only.
	Insecure bool `default:"false" env:"AUTH_INSECURE"`
}

type RoomConfig struct {
	MaxParticipants int `default:"50" env:"ROOM_MAX_PARTICIPANTS"`
	// Uplink datagrams per second per sender before dropping (20ms frames = 50/s).
	MaxDatagramsPerSec int `default:"60" env:"MAX_DATAGRAMS_PER_SEC"`
	MaxPayload         int `default:"1000" env:"MAX_DATAGRAM_PAYLOAD"`
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
