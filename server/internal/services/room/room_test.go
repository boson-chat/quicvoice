package room

import (
	"context"
	"sync"
	"testing"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/metrics"
	"github.com/boson-chat/quicvoice/wire"
)

type fakePeer struct {
	mu    sync.Mutex
	dgram [][]byte
	ctrl  []*controlpb.ControlMessage
}

func (p *fakePeer) SendDatagram(b []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dgram = append(p.dgram, b)
	return nil
}

func (p *fakePeer) SendControl(m *controlpb.ControlMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ctrl = append(p.ctrl, m)
	return nil
}

func ident(name string, role controlpb.Role) *auth.Identity {
	return &auth.Identity{UserID: name, DisplayName: name, Role: role}
}

func uplink(seq uint16) []byte {
	return wire.AppendUplink(nil, &wire.Uplink{Audio: wire.Audio{Seq: seq, Timestamp: 960, Payload: []byte("opus")}})
}

func TestForwardReachesOthersOnly(t *testing.T) {
	ctx := context.Background()
	s := NewRoomService(10, metrics.Noop())
	pa, pb, pc := &fakePeer{}, &fakePeer{}, &fakePeer{}
	a, _, err := s.Join(ctx, "r", ident("a", controlpb.Role_SPEAKER), pa)
	if err != nil {
		t.Fatal(err)
	}
	b, state, _ := s.Join(ctx, "r", ident("b", controlpb.Role_SPEAKER), pb)
	if len(state.Participants) != 2 || state.YourSessionId != uint32(b.SessionID) {
		t.Fatalf("bad room state: %v", state)
	}
	_, _, _ = s.Join(ctx, "other", ident("c", controlpb.Role_SPEAKER), pc)

	s.Forward(a, uplink(7))

	if len(pa.dgram) != 0 || len(pc.dgram) != 0 {
		t.Fatalf("echoed to sender or leaked to other room: a=%d c=%d", len(pa.dgram), len(pc.dgram))
	}
	if len(pb.dgram) != 1 {
		t.Fatalf("b got %d datagrams, want 1", len(pb.dgram))
	}
	d, err := wire.ParseDownlink(pb.dgram[0])
	if err != nil {
		t.Fatal(err)
	}
	if d.SessionID != a.SessionID || d.Seq != 7 || string(d.Payload) != "opus" {
		t.Fatalf("bad downlink: %+v", d)
	}
}

func TestMutedListenerDeafened(t *testing.T) {
	ctx := context.Background()
	s := NewRoomService(10, metrics.Noop())
	pa, pb := &fakePeer{}, &fakePeer{}
	listener, _, _ := s.Join(ctx, "r", ident("l", controlpb.Role_LISTENER), pa)
	speaker, _, _ := s.Join(ctx, "r", ident("s", controlpb.Role_SPEAKER), pb)

	s.Forward(listener, uplink(1))
	if len(pb.dgram) != 0 {
		t.Fatal("listener audio was forwarded")
	}

	s.SetMute(ctx, speaker, true, false)
	s.Forward(speaker, uplink(2))
	if len(pa.dgram) != 0 {
		t.Fatal("muted speaker audio was forwarded")
	}

	s.SetMute(ctx, speaker, false, false)
	s.SetMute(ctx, listener, false, true)
	s.Forward(speaker, uplink(3))
	if len(pa.dgram) != 0 {
		t.Fatal("deafened member received audio")
	}
}

func TestLeaveAndRoomCleanup(t *testing.T) {
	ctx := context.Background()
	s := NewRoomService(1, metrics.Noop()).(*RoomService)
	pa := &fakePeer{}
	a, _, _ := s.Join(ctx, "r", ident("a", controlpb.Role_SPEAKER), pa)
	if _, _, err := s.Join(ctx, "r", ident("b", controlpb.Role_SPEAKER), &fakePeer{}); err != ErrRoomFull {
		t.Fatalf("want ErrRoomFull, got %v", err)
	}
	s.Leave(ctx, a, "bye")
	s.Leave(ctx, a, "bye") // idempotent
	if len(s.rooms) != 0 {
		t.Fatalf("room not cleaned up: %d rooms", len(s.rooms))
	}
	// A rejected join into a fresh room must not leave an empty room behind.
	if _, _, err := s.Join(ctx, "", ident("a", controlpb.Role_SPEAKER), pa); err != ErrEmptyRoom {
		t.Fatalf("want ErrEmptyRoom, got %v", err)
	}
	if len(s.rooms) != 0 {
		t.Fatalf("empty room left behind")
	}
}

func TestSessionIDsNotReusedImmediately(t *testing.T) {
	ctx := context.Background()
	s := NewRoomService(10, metrics.Noop())
	a, _, _ := s.Join(ctx, "r", ident("a", controlpb.Role_SPEAKER), &fakePeer{})
	keep, _, _ := s.Join(ctx, "r", ident("k", controlpb.Role_SPEAKER), &fakePeer{})
	s.Leave(ctx, a, "")
	b, _, _ := s.Join(ctx, "r", ident("b", controlpb.Role_SPEAKER), &fakePeer{})
	if b.SessionID == a.SessionID || b.SessionID == keep.SessionID {
		t.Fatalf("session id reused: %d", b.SessionID)
	}
}
