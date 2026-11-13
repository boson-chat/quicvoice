// Package logger wraps zerolog with context helpers.
package logger

import (
	"context"
	"os"

	"github.com/rs/zerolog"
)

type options struct {
	serverName  string
	version     string
	environment string
}

type Option func(*options)

func WithServerName(s string) Option  { return func(o *options) { o.serverName = s } }
func WithVersion(s string) Option     { return func(o *options) { o.version = s } }
func WithEnvironment(s string) Option { return func(o *options) { o.environment = s } }

var base = zerolog.New(os.Stdout).With().Timestamp().Logger()

// Logger configures the global base logger.
func Logger(opts ...Option) zerolog.Logger {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}
	var out = zerolog.New(os.Stdout)
	if o.environment == "development" {
		out = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr})
	}
	base = out.With().Timestamp().
		Str("service", o.serverName).
		Str("version", o.version).
		Str("env", o.environment).
		Logger()
	return base
}

type ctxKey struct{}

// WithLogger returns ctx carrying l.
func WithLogger(ctx context.Context, l zerolog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromCtx returns the logger in ctx, or the base logger.
func FromCtx(ctx context.Context) *zerolog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(zerolog.Logger); ok {
		return &l
	}
	return &base
}
