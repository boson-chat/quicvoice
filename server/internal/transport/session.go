package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/boson-chat/quicvoice/server/internal/logger"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/metrics"
	"github.com/boson-chat/quicvoice/wire"
	"github.com/quic-go/quic-go"
)

// Control messages queued for a client before we treat it as stuck.
const controlQueue = 256

// Uplink sequence jumps larger than this (1 s of audio) either way are treated
// as a sender restart, not loss or reordering.
const maxSeqGap = 50

// Downlink datagrams queued per receiver before new ones are dropped: about
// 0.3 s of audio with 10 people talking. Audio that late is useless anyway.
const downlinkQueue = 128

type session struct {
	srv   *Server
	conn  *quic.Conn
	out   chan *controlpb.ControlMessage
	dgram chan []byte

	identity *auth.Identity
	member   atomic.Pointer[room.Member]
}

func newSession(srv *Server, conn *quic.Conn) *session {
	return &session{
		srv:   srv,
		conn:  conn,
		out:   make(chan *controlpb.ControlMessage, controlQueue),
		dgram: make(chan []byte, downlinkQueue),
	}
}

// SendDatagram implements room.Peer. It only queues: quic-go's SendDatagram
// blocks while the connection's own send queue is full, so the actual send
// happens on this receiver's datagramWriter goroutine.
func (s *session) SendDatagram(b []byte) error {
	select {
	case <-s.conn.Context().Done():
		return context.Cause(s.conn.Context())
	default:
	}
	select {
	case s.dgram <- b:
		return nil
	default:
		return room.ErrSendQueueFull
	}
}

// datagramWriter sends queued downlinks. Failures here are counted directly,
// since the forwarding loop has already moved on.
func (s *session) datagramWriter() {
	mt := s.srv.metrics
	for {
		select {
		case b := <-s.dgram:
			if err := s.conn.SendDatagram(b); err != nil {
				var tooLarge *quic.DatagramTooLargeError
				if errors.As(err, &tooLarge) {
					mt.Drop(metrics.DropSendTooLarge)
				} else {
					mt.Drop(metrics.DropSendFailed)
				}
			}
		case <-s.conn.Context().Done():
			return
		}
	}
}

// SendControl implements room.Peer. It never blocks: a client that can't keep
// up with control traffic is disconnected.
func (s *session) SendControl(m *controlpb.ControlMessage) error {
	select {
	case s.out <- m:
		return nil
	default:
		_ = s.conn.CloseWithError(CodeSlow, "control queue full")
		return errors.New("control queue full")
	}
}

func (s *session) run(ctx context.Context) (err error) {
	log := logger.FromCtx(ctx)
	mt := s.srv.metrics
	accepted := time.Now()
	defer s.leave(ctx, metrics.LeaveDisconnected)

	actx, cancel := context.WithDeadline(ctx, s.srv.helloDeadline())
	stream, err := s.conn.AcceptStream(actx)
	cancel()
	if err != nil {
		mt.Handshake(ctx, metrics.HandshakeNoStream, 0)
		_ = s.conn.CloseWithError(CodeProtocol, "no control stream")
		return err
	}

	_ = stream.SetReadDeadline(s.srv.helloDeadline())
	rd := wire.NewControlReader(stream)
	first, err := rd.Read()
	if err != nil {
		mt.Handshake(ctx, metrics.HandshakeNoHello, 0)
		_ = s.conn.CloseWithError(CodeProtocol, "no hello")
		return err
	}
	_ = stream.SetReadDeadline(time.Time{})

	hello := first.GetHello()
	if hello == nil {
		mt.Handshake(ctx, metrics.HandshakeProtocol, 0)
		return s.fatal(stream, controlpb.Error_PROTOCOL, "expected Hello")
	}
	mt.ControlMessage(ctx, metrics.ControlHello)
	id, err := s.srv.auth.Verify(ctx, hello.GetToken())
	if err != nil {
		code, outcome := controlpb.Error_UNAUTHENTICATED, metrics.HandshakeBadToken
		if errors.Is(err, auth.ErrTokenExpired) {
			code, outcome = controlpb.Error_TOKEN_EXPIRED, metrics.HandshakeExpired
		}
		mt.Handshake(ctx, outcome, 0)
		return s.fatal(stream, code, err.Error())
	}
	s.identity = id
	l := log.With().Str("user", id.UserID).Logger()
	ctx = logger.WithLogger(ctx, l)
	l.Info().Str("client", hello.GetClientVersion()).Msg("authenticated")

	go s.writeLoop(stream)
	go s.datagramWriter()
	_ = s.SendControl(welcome(s.srv.cfg.RoomConfig.MaxPayload))
	mt.Handshake(ctx, metrics.HandshakeOK, time.Since(accepted))

	tok := mt.SessionStarted(ctx, hello.GetClientVersion())
	defer func() { mt.SessionEnded(context.WithoutCancel(ctx), tok, disconnectReason(err)) }()

	go s.datagramLoop(ctx)
	go s.statsLoop(ctx)

	for {
		m, err := rd.Read()
		if err != nil {
			return err
		}
		if err := s.handleControl(ctx, m); err != nil {
			return err
		}
	}
}

func (s *session) handleControl(ctx context.Context, m *controlpb.ControlMessage) error {
	mt := s.srv.metrics
	switch msg := m.Msg.(type) {
	case *controlpb.ControlMessage_Join:
		mt.ControlMessage(ctx, metrics.ControlJoin)
		if mem := s.member.Load(); mem != nil && mem.Closed() {
			// The server removed us (room deleted); already counted as kicked.
			s.member.CompareAndSwap(mem, nil)
		}
		if s.member.Load() != nil {
			mt.Join(ctx, metrics.JoinAlreadyInRoom)
			s.sendError(controlpb.Error_PROTOCOL, room.ErrInRoom.Error(), false)
			return nil
		}
		mem, state, err := s.srv.rooms.Join(ctx, msg.Join.GetRoom(), s.identity, s)
		if err != nil {
			mt.Join(ctx, joinOutcome(err))
			s.sendError(joinErrorCode(err), err.Error(), false)
			return nil
		}
		mt.Join(ctx, metrics.JoinOK)
		mt.RoomSize(ctx, len(state.GetParticipants()))
		s.member.Store(mem)
		logger.FromCtx(ctx).Info().Str("room", mem.Room()).Uint16("session", mem.SessionID).Msg("joined")
		_ = s.SendControl(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_RoomState{RoomState: state}})

	case *controlpb.ControlMessage_Leave:
		mt.ControlMessage(ctx, metrics.ControlLeave)
		s.leave(ctx, metrics.LeaveRequested)

	case *controlpb.ControlMessage_SetMute:
		mt.ControlMessage(ctx, metrics.ControlSetMute)
		if mem := s.member.Load(); mem != nil {
			s.srv.rooms.SetMute(ctx, mem, msg.SetMute.GetMuted(), msg.SetMute.GetDeafened())
		}

	case *controlpb.ControlMessage_Ping:
		mt.ControlMessage(ctx, metrics.ControlPing)
		_ = s.SendControl(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Pong{
			Pong: &controlpb.Pong{Id: msg.Ping.GetId()},
		}})

	case *controlpb.ControlMessage_Hello:
		mt.ControlMessage(ctx, metrics.ControlHello)
		s.sendError(controlpb.Error_PROTOCOL, "duplicate Hello", false)

	default:
		// Known-but-unimplemented and future messages are ignored so newer
		// clients keep working against this server.
		mt.ControlMessage(ctx, metrics.ControlUnsupported)
	}
	return nil
}

func (s *session) leave(ctx context.Context, reason metrics.LeaveReason) {
	if mem := s.member.Swap(nil); mem != nil {
		if mem.Closed() {
			return // removed by the server; counted as kicked then
		}
		s.srv.rooms.Leave(ctx, mem, reason.String())
		s.srv.metrics.Leave(ctx, reason)
		logger.FromCtx(ctx).Info().Str("room", mem.Room()).Stringer("reason", reason).Msg("left")
	}
}

// datagramLoop is the per-packet hot path: metrics here are atomic adds only.
func (s *session) datagramLoop(ctx context.Context) {
	mt := s.srv.metrics
	maxPerSec := s.srv.cfg.RoomConfig.MaxDatagramsPerSec
	maxLen := s.srv.cfg.RoomConfig.MaxPayload + wire.UplinkHeaderLen
	var window time.Time
	count := 0
	// Uplink loss estimate. Only this goroutine touches these.
	var lastSeq uint16
	haveSeq := false

	for {
		b, err := s.conn.ReceiveDatagram(ctx)
		if err != nil {
			return
		}
		_, kind, err := wire.Peek(b)
		if err != nil {
			mt.DatagramReceived(metrics.DatagramOther, len(b))
			mt.Drop(metrics.DropMalformed)
			continue
		}
		switch kind {
		case wire.KindUplink:
			mt.DatagramReceived(metrics.DatagramUplink, len(b))
			// Before rate limiting, so our own drops don't read as network loss.
			if len(b) >= wire.UplinkHeaderLen {
				lastSeq, haveSeq = trackSeq(mt, b, lastSeq, haveSeq)
			}
		case wire.KindPing:
			mt.DatagramReceived(metrics.DatagramPing, len(b))
		default:
			mt.DatagramReceived(metrics.DatagramOther, len(b))
		}

		if now := time.Now(); now.Sub(window) >= time.Second {
			window, count = now, 0
		}
		count++
		if count > maxPerSec {
			mt.Drop(metrics.DropRateLimited)
			continue
		}

		switch kind {
		case wire.KindUplink:
			if len(b) < wire.UplinkHeaderLen {
				mt.Drop(metrics.DropMalformed)
				continue
			}
			if len(b) > maxLen {
				mt.Drop(metrics.DropTooLarge)
				continue
			}
			mem := s.member.Load()
			if mem == nil {
				mt.Drop(metrics.DropNotInRoom)
				continue
			}
			s.srv.rooms.Forward(mem, b)

		case wire.KindPing:
			p, err := wire.ParsePing(b)
			if err != nil {
				mt.Drop(metrics.DropMalformed)
				continue
			}
			if p.Reply {
				continue
			}
			p.Reply = true
			out := wire.AppendPing(nil, &p)
			if err := s.SendDatagram(out); err != nil {
				if errors.Is(err, room.ErrSendQueueFull) {
					mt.Drop(metrics.DropSendQueueFull)
				} else {
					mt.Drop(metrics.DropSendFailed)
				}
				continue
			}
			mt.DatagramsSent(metrics.SentPong, 1, len(out))

		default:
			mt.Drop(metrics.DropUnknownKind)
		}
	}
}

// trackSeq counts frames missing between the last uplink seen and this one
// (b is a full uplink header). It returns the new cursor. Small backwards
// steps are late/reordered frames and leave the cursor alone; a START flag or
// a big jump either way restarts tracking without counting loss.
func trackSeq(mt *metrics.Metrics, b []byte, last uint16, have bool) (uint16, bool) {
	seq := binary.BigEndian.Uint16(b[4:6])
	if !have || b[1]&wire.FlagStart != 0 {
		return seq, true
	}
	if back := last - seq; back > 0 && back <= maxSeqGap {
		return last, true
	}
	if gap := seq - last - 1; gap > 0 && gap < maxSeqGap {
		mt.UplinkLost(int(gap))
	}
	return seq, true
}

// statsLoop samples the connection's QUIC stats every QUIC_STATS_INTERVAL
// and once more when it closes.
func (s *session) statsLoop(ctx context.Context) {
	var tick <-chan time.Time
	if iv := s.srv.cfg.TelemetryConfig.QUICStatsInterval; iv > 0 {
		t := time.NewTicker(iv)
		defer t.Stop()
		tick = t.C
	}
	var prev quic.ConnectionStats
	for {
		select {
		case <-tick:
			prev = s.sampleStats(ctx, prev)
		case <-s.conn.Context().Done():
			s.sampleStats(context.WithoutCancel(ctx), prev)
			return
		}
	}
}

func (s *session) sampleStats(ctx context.Context, prev quic.ConnectionStats) quic.ConnectionStats {
	cur := s.conn.ConnectionStats()
	s.srv.metrics.QUICSample(ctx, metrics.QUICSample{
		SmoothedRTT:     cur.SmoothedRTT,
		RTTVariation:    cur.MeanDeviation,
		PacketsSent:     delta(cur.PacketsSent, prev.PacketsSent),
		PacketsReceived: delta(cur.PacketsReceived, prev.PacketsReceived),
		// quic-go's lost counters can go down (a "lost" packet acked late).
		PacketsLost:   delta(cur.PacketsLost, prev.PacketsLost),
		BytesSent:     delta(cur.BytesSent, prev.BytesSent),
		BytesReceived: delta(cur.BytesReceived, prev.BytesReceived),
	})
	return cur
}

func delta(cur, prev uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}

func (s *session) writeLoop(stream *quic.Stream) {
	for {
		select {
		case m := <-s.out:
			if err := wire.WriteControl(stream, m); err != nil {
				_ = s.conn.CloseWithError(CodeProtocol, "control write failed")
				return
			}
		case <-s.conn.Context().Done():
			return
		}
	}
}

func (s *session) sendError(code controlpb.Error_Code, msg string, fatal bool) {
	_ = s.SendControl(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Error{
		Error: &controlpb.Error{Code: code, Message: msg, Fatal: fatal},
	}})
}

// fatal writes an Error directly (the write loop may not be running yet) and
// closes the connection.
func (s *session) fatal(stream *quic.Stream, code controlpb.Error_Code, msg string) error {
	_ = stream.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = wire.WriteControl(stream, &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Error{
		Error: &controlpb.Error{Code: code, Message: msg, Fatal: true},
	}})
	_ = stream.Close()
	// Give the peer a moment to read the Error before the close frame lands.
	time.Sleep(100 * time.Millisecond)
	_ = s.conn.CloseWithError(CodeAuth, msg)
	return errors.New(msg)
}

// disconnectReason classifies why an authenticated session's run returned.
func disconnectReason(err error) metrics.DisconnectReason {
	var appErr *quic.ApplicationError
	var idle *quic.IdleTimeoutError
	var trErr *quic.TransportError
	var strErr *quic.StreamError
	switch {
	case errors.As(err, &appErr):
		if appErr.Remote {
			return metrics.DisconnectClientClosed
		}
		switch appErr.ErrorCode {
		case CodeSlow:
			return metrics.DisconnectSlowConsumer
		case CodeProtocol:
			return metrics.DisconnectProtocolError
		case CodeShutdown:
			return metrics.DisconnectServerShutdown
		}
		return metrics.DisconnectError
	case errors.As(err, &idle):
		return metrics.DisconnectIdleTimeout
	case errors.Is(err, io.EOF),
		errors.As(err, &strErr) && strErr.Remote,
		errors.As(err, &trErr) && trErr.Remote:
		return metrics.DisconnectClientClosed
	case errors.Is(err, context.Canceled), errors.Is(err, quic.ErrServerClosed):
		return metrics.DisconnectServerShutdown
	}
	return metrics.DisconnectError
}

func joinOutcome(err error) metrics.JoinOutcome {
	switch {
	case errors.Is(err, room.ErrRoomFull):
		return metrics.JoinFull
	case errors.Is(err, room.ErrForbidden):
		return metrics.JoinForbidden
	case errors.Is(err, room.ErrNotFound):
		return metrics.JoinNotFound
	default:
		return metrics.JoinBadRequest
	}
}

func joinErrorCode(err error) controlpb.Error_Code {
	switch {
	case errors.Is(err, room.ErrRoomFull):
		return controlpb.Error_ROOM_FULL
	case errors.Is(err, room.ErrForbidden):
		return controlpb.Error_FORBIDDEN
	case errors.Is(err, room.ErrNotFound):
		return controlpb.Error_ROOM_NOT_FOUND
	default:
		return controlpb.Error_PROTOCOL
	}
}

func welcome(maxPayload int) *controlpb.ControlMessage {
	return &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Welcome{Welcome: &controlpb.Welcome{
		Codec: &controlpb.CodecConfig{
			Name: "opus", SampleRate: 48000, Channels: 1, FrameMs: 20, Bitrate: 32000, Fec: true, Dtx: true,
		},
		MaxDatagramPayload: uint32(maxPayload),
	}}}
}
