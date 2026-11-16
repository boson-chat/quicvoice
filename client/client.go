// Package client is the quicvoice Go SDK: connect, authenticate, join a room,
// send and receive audio datagrams. It has no audio I/O of its own yet; the
// caller hands it encoded Opus frames and gets them back per sender.
package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/boson-chat/quicvoice/wire"
	"github.com/quic-go/quic-go"
)

const Version = "quicvoice-go/0.1"

type Config struct {
	// host:port of the voice server, e.g. "voice.boson.chat:7700".
	Addr  string
	Token string
	// Skip TLS verification (self-signed dev server).
	InsecureSkipVerify bool

	// Called from the datagram goroutine for every downlink audio frame.
	// Payload aliases an internal buffer only for the duration of the call.
	OnAudio func(d wire.Downlink)
	// Called from the control goroutine for every control message after Welcome
	// (RoomState, ParticipantJoined/Left/Updated, Error, ...).
	OnControl func(m *controlpb.ControlMessage)
}

// Stats are cumulative counters plus the last datagram-path RTT.
type Stats struct {
	Sent, Received uint64
	RTT            time.Duration
}

type Client struct {
	cfg    Config
	conn   *quic.Conn
	stream *quic.Stream

	writeMu sync.Mutex
	welcome *controlpb.Welcome

	reqMu     sync.Mutex // one request (Join, VideoTicket) at a time
	pendingMu sync.Mutex
	pending   chan *controlpb.ControlMessage // set while a request waits

	sent, received atomic.Uint64
	rttNanos       atomic.Int64
	pingID         atomic.Uint32
	epoch          time.Time
}

// Dial connects, sends Hello and waits for Welcome.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	tlsConf := &tls.Config{
		NextProtos:         []string{wire.ALPN},
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // opt-in for dev servers
	}
	conn, err := quic.DialAddr(ctx, cfg.Addr, tlsConf, &quic.Config{
		EnableDatagrams: true,
		KeepAlivePeriod: 10 * time.Second,
		MaxIdleTimeout:  30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.Addr, err)
	}
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, err
	}
	c := &Client{cfg: cfg, conn: conn, stream: stream, epoch: time.Now()}

	if err := c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Hello{
		Hello: &controlpb.Hello{Token: cfg.Token, ClientVersion: Version},
	}}); err != nil {
		c.Close()
		return nil, err
	}

	rd := wire.NewControlReader(stream)
	if dl, ok := ctx.Deadline(); ok {
		_ = stream.SetReadDeadline(dl)
	}
	first, err := rd.Read()
	_ = stream.SetReadDeadline(time.Time{})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("waiting for Welcome: %w", err)
	}
	if e := first.GetError(); e != nil {
		c.Close()
		return nil, fmt.Errorf("server: %s: %s", e.GetCode(), e.GetMessage())
	}
	if c.welcome = first.GetWelcome(); c.welcome == nil {
		c.Close()
		return nil, errors.New("expected Welcome")
	}

	go c.controlLoop(rd)
	go c.datagramLoop()
	return c, nil
}

func (c *Client) Welcome() *controlpb.Welcome { return c.welcome }

// Join enters a room and returns its state, including our session id.
func (c *Client) Join(ctx context.Context, room string) (*controlpb.RoomState, error) {
	m, err := c.request(ctx, &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Join{
		Join: &controlpb.Join{Room: room},
	}})
	if err != nil {
		return nil, err
	}
	if e := m.GetError(); e != nil {
		return nil, fmt.Errorf("join %q: %s: %s", room, e.GetCode(), e.GetMessage())
	}
	return m.GetRoomState(), nil
}

// VideoTicket asks for a ticket to the video room of the voice channel
// we're in (quicvideod verifies it; see docs/video.md).
func (c *Client) VideoTicket(ctx context.Context) (*controlpb.VideoTicket, error) {
	m, err := c.request(ctx, &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_VideoTicketRequest{
		VideoTicketRequest: &controlpb.VideoTicketRequest{},
	}})
	if err != nil {
		return nil, err
	}
	if e := m.GetError(); e != nil {
		return nil, fmt.Errorf("video ticket: %s: %s", e.GetCode(), e.GetMessage())
	}
	if m.GetVideoTicket() == nil {
		return nil, fmt.Errorf("video ticket: unexpected reply")
	}
	return m.GetVideoTicket(), nil
}

// request sends m and waits for the reply (RoomState, VideoTicket or Error).
func (c *Client) request(ctx context.Context, m *controlpb.ControlMessage) (*controlpb.ControlMessage, error) {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()
	ch := make(chan *controlpb.ControlMessage, 1)
	c.pendingMu.Lock()
	c.pending = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		c.pending = nil
		c.pendingMu.Unlock()
	}()

	if err := c.send(m); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.conn.Context().Done():
		return nil, context.Cause(c.conn.Context())
	}
}

func (c *Client) Leave() error {
	return c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Leave{Leave: &controlpb.Leave{}}})
}

// SendAudioClock sends a lip-sync report: the audio timestamp ts was
// captured at captureUS (Unix µs). The voice engine calls it (ClockSender).
func (c *Client) SendAudioClock(ts uint32, captureUS int64) error {
	return c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_AudioClock{
		AudioClock: &controlpb.AudioClock{Timestamp: ts, CaptureUs: captureUS, Version: 1},
	}})
}

func (c *Client) SetMute(muted, deafened bool) error {
	return c.send(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_SetMute{
		SetMute: &controlpb.SetMute{Muted: muted, Deafened: deafened},
	}})
}

// SendAudio sends one encoded frame as an uplink datagram.
func (c *Client) SendAudio(u *wire.Uplink) error {
	if err := c.conn.SendDatagram(wire.AppendUplink(nil, u)); err != nil {
		return err
	}
	c.sent.Add(1)
	return nil
}

// Ping sends a datagram RTT probe; the result shows up in Stats().RTT.
func (c *Client) Ping() error {
	p := wire.Ping{ID: uint16(c.pingID.Add(1)), SentUS: uint32(time.Since(c.epoch).Microseconds())}
	return c.conn.SendDatagram(wire.AppendPing(nil, &p))
}

func (c *Client) Stats() Stats {
	return Stats{Sent: c.sent.Load(), Received: c.received.Load(), RTT: time.Duration(c.rttNanos.Load())}
}

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.conn.Context().Done() }

func (c *Client) Close() error { return c.conn.CloseWithError(0, "bye") }

func (c *Client) send(m *controlpb.ControlMessage) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return wire.WriteControl(c.stream, m)
}

func (c *Client) controlLoop(rd *wire.ControlReader) {
	for {
		m, err := rd.Read()
		if err != nil {
			return
		}
		if m.GetRoomState() != nil || m.GetError() != nil || m.GetVideoTicket() != nil {
			c.pendingMu.Lock()
			if c.pending != nil {
				select {
				case c.pending <- m:
				default:
				}
			}
			c.pendingMu.Unlock()
		}
		if c.cfg.OnControl != nil {
			c.cfg.OnControl(m)
		}
	}
}

func (c *Client) datagramLoop() {
	ctx := c.conn.Context()
	for {
		b, err := c.conn.ReceiveDatagram(ctx)
		if err != nil {
			return
		}
		_, kind, err := wire.Peek(b)
		if err != nil {
			continue
		}
		switch kind {
		case wire.KindDownlink:
			d, err := wire.ParseDownlink(b)
			if err != nil {
				continue
			}
			c.received.Add(1)
			if c.cfg.OnAudio != nil {
				c.cfg.OnAudio(d)
			}
		case wire.KindPing:
			p, err := wire.ParsePing(b)
			if err != nil {
				continue
			}
			if !p.Reply {
				p.Reply = true
				_ = c.conn.SendDatagram(wire.AppendPing(nil, &p))
				continue
			}
			now := uint32(time.Since(c.epoch).Microseconds())
			c.rttNanos.Store(int64(now-p.SentUS) * int64(time.Microsecond))
		}
	}
}
