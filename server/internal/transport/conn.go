package transport

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

// vconn is a client connection carrying the quicvoice protocol: native QUIC
// (ALPN quicvoice/1) or a WebTransport session (browsers, HTTP/3 at /wt).
// Both carry the same things: one bidirectional control stream opened by
// the client, and audio/ping datagrams both ways.
type vconn interface {
	AcceptControl(ctx context.Context) (ctrlStream, error)
	SendDatagram(b []byte) error
	ReceiveDatagram(ctx context.Context) ([]byte, error)
	CloseWithError(code quic.ApplicationErrorCode, msg string)
	Context() context.Context
	RemoteAddr() net.Addr
	// Stats returns the underlying QUIC connection's statistics, when known.
	Stats() (quic.ConnectionStats, bool)
	// Kind is "quic" or "webtransport" (logs, metrics).
	Kind() string
	// MaxPayload caps the Opus payload a client may send on this transport
	// (WebTransport datagrams carry an extra stream-id prefix).
	MaxPayload(configured int) int
}

type ctrlStream interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// ---- native QUIC -----------------------------------------------------------

type quicConn struct{ c *quic.Conn }

func (q quicConn) Kind() string { return "quic" }

func (q quicConn) AcceptControl(ctx context.Context) (ctrlStream, error) {
	return q.c.AcceptStream(ctx)
}

func (q quicConn) SendDatagram(b []byte) error { return q.c.SendDatagram(b) }

func (q quicConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	return q.c.ReceiveDatagram(ctx)
}

func (q quicConn) CloseWithError(code quic.ApplicationErrorCode, msg string) {
	_ = q.c.CloseWithError(code, msg)
}
func (q quicConn) Context() context.Context { return q.c.Context() }
func (q quicConn) RemoteAddr() net.Addr     { return q.c.RemoteAddr() }
func (q quicConn) Stats() (quic.ConnectionStats, bool) {
	return q.c.ConnectionStats(), true
}
func (q quicConn) MaxPayload(configured int) int { return configured }

// ---- WebTransport ----------------------------------------------------------

// wtMaxPayload bounds the Opus payload on WebTransport sessions. An HTTP/3
// datagram is the QUIC datagram frame plus a quarter-stream-id varint (up to
// 8 bytes), and browsers size datagrams for a ~1200-byte path MTU; 1000
// bytes of Opus (with the 12-byte downlink header) always fits. A 20 ms
// Opus frame at 510 kbit/s is ~1,275 bytes, so the top Opus bitrates are
// out of reach on the web; Boson's "music" quality (128 kbit/s) is ~320.
const wtMaxPayload = 1000

type wtConn struct {
	s *webtransport.Session
	// q is the QUIC connection the session runs on (for RTT/loss); nil if
	// it couldn't be found.
	q *quic.Conn
}

func (w wtConn) Kind() string { return "webtransport" }

func (w wtConn) AcceptControl(ctx context.Context) (ctrlStream, error) {
	return w.s.AcceptStream(ctx)
}

func (w wtConn) SendDatagram(b []byte) error { return w.s.SendDatagram(b) }

func (w wtConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	return w.s.ReceiveDatagram(ctx)
}

func (w wtConn) CloseWithError(code quic.ApplicationErrorCode, msg string) {
	_ = w.s.CloseWithError(webtransport.SessionErrorCode(code), msg)
}
func (w wtConn) Context() context.Context { return w.s.Context() }
func (w wtConn) RemoteAddr() net.Addr     { return w.s.RemoteAddr() }
func (w wtConn) Stats() (quic.ConnectionStats, bool) {
	if w.q == nil {
		return quic.ConnectionStats{}, false
	}
	return w.q.ConnectionStats(), true
}
func (w wtConn) MaxPayload(configured int) int { return min(configured, wtMaxPayload) }
