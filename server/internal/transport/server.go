// Package transport runs the QUIC listener and one session per connection:
// the control stream state machine plus the datagram loop.
package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/boson-chat/quicvoice/server/config"
	"github.com/boson-chat/quicvoice/server/internal/logger"
	"github.com/boson-chat/quicvoice/server/internal/origins"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/services/videoticket"
	"github.com/boson-chat/quicvoice/server/metrics"
	"github.com/boson-chat/quicvoice/wire"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

// Application error codes used when closing a connection.
const (
	CodeOK       quic.ApplicationErrorCode = 0
	CodeProtocol quic.ApplicationErrorCode = 1
	CodeAuth     quic.ApplicationErrorCode = 2
	CodeSlow     quic.ApplicationErrorCode = 3
	CodeShutdown quic.ApplicationErrorCode = 4
)

// WebTransportPath is where browsers open their session
// (https://host:port/wt, on the QUIC port).
const WebTransportPath = "/wt"

type Server struct {
	cfg     config.Config
	tls     *tls.Config
	auth    auth.AuthServiceImpl
	rooms   room.RoomServiceImpl
	metrics *metrics.Metrics
	tickets videoticket.VideoTicketServiceImpl // nil = no video
	origins *origins.Policy                    // nil = cfg's WEBTRANSPORT_ORIGINS, fixed entries

	ready chan struct{}
	addr  atomic.Pointer[net.Addr]
	wt    *webtransport.Server
	// h3 QUIC connections by remote address, so a WebTransport session can
	// report its connection's RTT and loss.
	h3conns sync.Map // string → *quic.Conn
}

// SetVideoTickets turns on video tickets for voice-channel members.
func (s *Server) SetVideoTickets(t videoticket.VideoTicketServiceImpl) { s.tickets = t }

func NewServer(cfg config.Config, tlsConf *tls.Config, a auth.AuthServiceImpl, r room.RoomServiceImpl, m *metrics.Metrics) *Server {
	tlsConf = tlsConf.Clone()
	// One port, two protocols: native quicvoice, and HTTP/3 for browsers'
	// WebTransport sessions.
	tlsConf.NextProtos = []string{wire.ALPN, http3.NextProtoH3}
	tlsConf.MinVersion = tls.VersionTLS13
	return &Server{cfg: cfg, tls: tlsConf, auth: a, rooms: r, metrics: m, ready: make(chan struct{})}
}

// Addr is the bound UDP address once Serve is listening (tests use :0).
func (s *Server) Addr() net.Addr {
	<-s.ready
	return *s.addr.Load()
}

// Serve listens until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := quic.ListenAddr(s.cfg.QUICConfig.Addr, s.tls, &quic.Config{
		EnableDatagrams: true,
		MaxIdleTimeout:  s.cfg.QUICConfig.MaxIdleTimeout,
		KeepAlivePeriod: s.cfg.QUICConfig.KeepAlive,
		// One control stream per connection (WebTransport: plus the
		// CONNECT request stream); leave a little headroom.
		MaxIncomingStreams: 8,
		// Native clients open none; HTTP/3 needs the peer's control and
		// QPACK streams (WebTransport sessions use no others here).
		MaxIncomingUniStreams: 8,
		// Required by WebTransport (reliable stream resets).
		EnableStreamResetPartialDelivery: true,
	})
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.QUICConfig.Addr, err)
	}
	a := ln.Addr()
	s.addr.Store(&a)
	close(s.ready)
	log := logger.FromCtx(ctx)
	log.Info().Str("addr", a.String()).Str("webtransport", WebTransportPath).Msg("QUIC listening")

	mux := http.NewServeMux()
	mux.HandleFunc(WebTransportPath, func(w http.ResponseWriter, r *http.Request) {
		if !s.checkOrigin(r) {
			// Upgrade would refuse it too, but without a status: an
			// unwritten response is a 200, which a client takes as success.
			log.Debug().Str("origin", r.Header.Get("Origin")).Msg("webtransport origin not allowed")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		sess, err := s.wt.Upgrade(w, r)
		if err != nil {
			log.Debug().Err(err).Msg("webtransport upgrade failed")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var qc *quic.Conn
		if v, ok := s.h3conns.Load(r.RemoteAddr); ok {
			qc = v.(*quic.Conn)
		}
		go s.handle(ctx, wtConn{s: sess, q: qc})
	})
	s.wt = &webtransport.Server{H3: &http3.Server{Handler: mux}, CheckOrigin: s.checkOrigin}
	webtransport.ConfigureHTTP3Server(s.wt.H3)

	go func() {
		<-ctx.Done()
		_ = s.wt.Close()
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
		if conn.ConnectionState().TLS.NegotiatedProtocol == http3.NextProtoH3 {
			key := conn.RemoteAddr().String()
			s.h3conns.Store(key, conn)
			go func() {
				defer s.h3conns.Delete(key)
				_ = s.wt.ServeQUICConn(conn)
			}()
			continue
		}
		go s.handle(ctx, quicConn{conn})
	}
}

// SetOrigins sets the parsed WEBTRANSPORT_ORIGINS (which can follow the
// directory's web origins). Unset: the list in the config, fixed entries
// only. Call before Serve.
func (s *Server) SetOrigins(p *origins.Policy) { s.origins = p }

// checkOrigin applies WEBTRANSPORT_ORIGINS (empty or "*" = any). Sessions
// still need a token in Hello; origins only narrow which pages may try.
func (s *Server) checkOrigin(r *http.Request) bool {
	if s.origins != nil {
		return s.origins.Allowed(r.Header.Get("Origin"))
	}
	return OriginAllowed(s.cfg.QUICConfig.WebTransportOrigins, r.Header.Get("Origin"))
}

// OriginAllowed reports whether origin is in the comma-separated list
// (empty or "*" = any origin; "directory" adds nothing without a fetcher).
func OriginAllowed(list, origin string) bool {
	return origins.MustParse(list, nil).Allowed(origin)
}

func (s *Server) handle(ctx context.Context, conn vconn) {
	s.metrics.ConnectionOpened(ctx)
	defer s.metrics.ConnectionClosed(context.WithoutCancel(ctx))

	l := logger.FromCtx(ctx).With().Str("remote", conn.RemoteAddr().String()).Str("transport", conn.Kind()).Logger()
	sctx, cancel := context.WithCancel(logger.WithLogger(conn.Context(), l))
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			conn.CloseWithError(CodeShutdown, "server shutting down")
		case <-sctx.Done():
		}
	}()

	sess := newSession(s, conn)
	err := sess.run(sctx)
	if err != nil && !isClosed(err) {
		l.Debug().Err(err).Msg("session ended")
	}
}

func isClosed(err error) bool {
	var appErr *quic.ApplicationError
	var idle *quic.IdleTimeoutError
	var sessErr *webtransport.SessionError
	return errors.As(err, &appErr) || errors.As(err, &idle) || errors.As(err, &sessErr) || errors.Is(err, context.Canceled)
}

// helloDeadline bounds how long a connection may sit before authenticating.
func (s *Server) helloDeadline() time.Time {
	return time.Now().Add(s.cfg.QUICConfig.HelloTimeout)
}
