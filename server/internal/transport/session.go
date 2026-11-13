package transport

import (
	"context"
	"errors"
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

type session struct {
	srv  *Server
	conn *quic.Conn
	out  chan *controlpb.ControlMessage

	identity *auth.Identity
	member   atomic.Pointer[room.Member]
}

func newSession(srv *Server, conn *quic.Conn) *session {
	return &session{srv: srv, conn: conn, out: make(chan *controlpb.ControlMessage, controlQueue)}
}

// SendDatagram implements room.Peer.
func (s *session) SendDatagram(b []byte) error { return s.conn.SendDatagram(b) }

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

func (s *session) run(ctx context.Context) error {
	log := logger.FromCtx(ctx)
	defer s.leave(ctx, "disconnected")

	actx, cancel := context.WithDeadline(ctx, s.srv.helloDeadline())
	stream, err := s.conn.AcceptStream(actx)
	cancel()
	if err != nil {
		_ = s.conn.CloseWithError(CodeProtocol, "no control stream")
		return err
	}

	_ = stream.SetReadDeadline(s.srv.helloDeadline())
	rd := wire.NewControlReader(stream)
	first, err := rd.Read()
	if err != nil {
		_ = s.conn.CloseWithError(CodeProtocol, "no hello")
		return err
	}
	_ = stream.SetReadDeadline(time.Time{})

	hello := first.GetHello()
	if hello == nil {
		return s.fatal(stream, controlpb.Error_PROTOCOL, "expected Hello")
	}
	id, err := s.srv.auth.Verify(ctx, hello.GetToken())
	if err != nil {
		code := controlpb.Error_UNAUTHENTICATED
		if errors.Is(err, auth.ErrTokenExpired) {
			code = controlpb.Error_TOKEN_EXPIRED
		}
		metrics.AuthFailures.WithLabelValues(code.String()).Inc()
		return s.fatal(stream, code, err.Error())
	}
	s.identity = id
	l := log.With().Str("user", id.UserID).Logger()
	ctx = logger.WithLogger(ctx, l)
	l.Info().Str("client", hello.GetClientVersion()).Msg("authenticated")

	go s.writeLoop(stream)
	_ = s.SendControl(welcome(s.srv.cfg.RoomConfig.MaxPayload))
	go s.datagramLoop(ctx)

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
	switch msg := m.Msg.(type) {
	case *controlpb.ControlMessage_Join:
		if s.member.Load() != nil {
			s.sendError(controlpb.Error_PROTOCOL, room.ErrInRoom.Error(), false)
			return nil
		}
		mem, state, err := s.srv.rooms.Join(ctx, msg.Join.GetRoom(), s.identity, s)
		if err != nil {
			s.sendError(joinErrorCode(err), err.Error(), false)
			return nil
		}
		s.member.Store(mem)
		logger.FromCtx(ctx).Info().Str("room", mem.Room()).Uint16("session", mem.SessionID).Msg("joined")
		_ = s.SendControl(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_RoomState{RoomState: state}})

	case *controlpb.ControlMessage_Leave:
		s.leave(ctx, "left")

	case *controlpb.ControlMessage_SetMute:
		if mem := s.member.Load(); mem != nil {
			s.srv.rooms.SetMute(ctx, mem, msg.SetMute.GetMuted(), msg.SetMute.GetDeafened())
		}

	case *controlpb.ControlMessage_Ping:
		_ = s.SendControl(&controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Pong{
			Pong: &controlpb.Pong{Id: msg.Ping.GetId()},
		}})

	case *controlpb.ControlMessage_Hello:
		s.sendError(controlpb.Error_PROTOCOL, "duplicate Hello", false)

	default:
		// Known-but-unimplemented and future messages are ignored so newer
		// clients keep working against this server.
	}
	return nil
}

func (s *session) leave(ctx context.Context, reason string) {
	if mem := s.member.Swap(nil); mem != nil {
		s.srv.rooms.Leave(ctx, mem, reason)
		logger.FromCtx(ctx).Info().Str("room", mem.Room()).Str("reason", reason).Msg("left")
	}
}

func (s *session) datagramLoop(ctx context.Context) {
	maxPerSec := s.srv.cfg.RoomConfig.MaxDatagramsPerSec
	maxLen := s.srv.cfg.RoomConfig.MaxPayload + wire.UplinkHeaderLen
	var window time.Time
	count := 0

	for {
		b, err := s.conn.ReceiveDatagram(ctx)
		if err != nil {
			return
		}
		_, kind, err := wire.Peek(b)
		if err != nil {
			metrics.Dropped.WithLabelValues("malformed").Inc()
			continue
		}

		if now := time.Now(); now.Sub(window) >= time.Second {
			window, count = now, 0
		}
		count++
		if count > maxPerSec {
			metrics.Dropped.WithLabelValues("rate_limited").Inc()
			continue
		}

		switch kind {
		case wire.KindUplink:
			metrics.DatagramsIn.WithLabelValues("uplink").Inc()
			if len(b) > maxLen {
				metrics.Dropped.WithLabelValues("too_large").Inc()
				continue
			}
			mem := s.member.Load()
			if mem == nil {
				metrics.Dropped.WithLabelValues("not_in_room").Inc()
				continue
			}
			s.srv.rooms.Forward(mem, b)

		case wire.KindPing:
			metrics.DatagramsIn.WithLabelValues("ping").Inc()
			p, err := wire.ParsePing(b)
			if err != nil || p.Reply {
				continue
			}
			p.Reply = true
			_ = s.conn.SendDatagram(wire.AppendPing(nil, &p))

		default:
			metrics.Dropped.WithLabelValues("unknown_kind").Inc()
		}
	}
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

func joinErrorCode(err error) controlpb.Error_Code {
	switch {
	case errors.Is(err, room.ErrRoomFull):
		return controlpb.Error_ROOM_FULL
	case errors.Is(err, room.ErrForbidden):
		return controlpb.Error_FORBIDDEN
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
