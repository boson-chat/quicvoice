// Package transport runs the QUIC listener and one session per connection:
// the control stream state machine plus the datagram loop.
package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"github.com/boson-chat/quicvoice/server/config"
	"github.com/boson-chat/quicvoice/server/internal/logger"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/metrics"
	"github.com/boson-chat/quicvoice/wire"
	"github.com/quic-go/quic-go"
)

// Application error codes used when closing a connection.
const (
	CodeOK       quic.ApplicationErrorCode = 0
	CodeProtocol quic.ApplicationErrorCode = 1
	CodeAuth     quic.ApplicationErrorCode = 2
	CodeSlow     quic.ApplicationErrorCode = 3
	CodeShutdown quic.ApplicationErrorCode = 4
)

type Server struct {
	cfg   config.Config
	tls   *tls.Config
	auth  auth.AuthServiceImpl
	rooms room.RoomServiceImpl
}

func NewServer(cfg config.Config, tlsConf *tls.Config, a auth.AuthServiceImpl, r room.RoomServiceImpl) *Server {
	tlsConf = tlsConf.Clone()
	tlsConf.NextProtos = []string{wire.ALPN}
	tlsConf.MinVersion = tls.VersionTLS13
	return &Server{cfg: cfg, tls: tlsConf, auth: a, rooms: r}
}

// Serve listens until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := quic.ListenAddr(s.cfg.QUICConfig.Addr, s.tls, &quic.Config{
		EnableDatagrams: true,
		MaxIdleTimeout:  s.cfg.QUICConfig.MaxIdleTimeout,
		KeepAlivePeriod: s.cfg.QUICConfig.KeepAlive,
		// One control stream per connection; leave a little headroom.
		MaxIncomingStreams:    4,
		MaxIncomingUniStreams: -1,
	})
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.QUICConfig.Addr, err)
	}
	log := logger.FromCtx(ctx)
	log.Info().Str("addr", s.cfg.QUICConfig.Addr).Msg("QUIC listening")

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, quic.ErrServerClosed) {
				return nil
			}
			return err
		}
		go s.handle(ctx, conn)
	}
}

func (s *Server) handle(ctx context.Context, conn *quic.Conn) {
	metrics.Connections.Inc()
	defer metrics.Connections.Dec()

	l := logger.FromCtx(ctx).With().Str("remote", conn.RemoteAddr().String()).Logger()
	ctx = logger.WithLogger(conn.Context(), l)

	sess := newSession(s, conn)
	err := sess.run(ctx)
	if err != nil && !isClosed(err) {
		l.Debug().Err(err).Msg("session ended")
	}
}

func isClosed(err error) bool {
	var appErr *quic.ApplicationError
	var idle *quic.IdleTimeoutError
	return errors.As(err, &appErr) || errors.As(err, &idle) || errors.Is(err, context.Canceled)
}

// helloDeadline bounds how long a connection may sit before authenticating.
func (s *Server) helloDeadline() time.Time {
	return time.Now().Add(s.cfg.QUICConfig.HelloTimeout)
}
