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

type fakeObserver struct {
	mu     sync.Mutex
	left   []string
	closed []string
}

func (o *fakeObserver) MemberLeft(room, user string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.left = append(o.left, room+"/"+user)
}

func (o *fakeObserver) RoomClosed(room string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = append(o.closed, room)
}

func TestObserverAndSnapshot(t *testing.T) {
	ctx := context.Background()
	obs := &fakeObserver{}
	s := NewRoomService(10, metrics.Noop(), WithObserver(obs))
	phone, _, _ := s.Join(ctx, "r", ident("a", controlpb.Role_SPEAKER), &fakePeer{})
	desk, _, _ := s.Join(ctx, "r", ident("a", controlpb.Role_SPEAKER), &fakePeer{})
	b, _, _ := s.Join(ctx, "r", ident("b", controlpb.Role_LISTENER), &fakePeer{})
	_, _, _ = s.Join(ctx, "other", ident("c", controlpb.Role_SPEAKER), &fakePeer{})

	snap := s.Snapshot()
	if len(snap["r"]) != 2 || len(snap["other"]) != 1 {
		t.Fatalf("snapshot %v (one entry per user, not per device)", snap)
	}

	// a still has the desktop in the room: no leave yet.
	s.Leave(ctx, phone, "bye")
	if len(obs.left) != 0 {
		t.Fatalf("left %v after one of two devices left", obs.left)
	}
	s.Leave(ctx, desk, "bye")
	s.Leave(ctx, b, "bye")
	s.Leave(ctx, b, "bye") // twice: reported once
	if len(obs.left) != 2 || obs.left[0] != "r/a" || obs.left[1] != "r/b" {
		t.Fatalf("left %v", obs.left)
	}

	if n := s.CloseRoom(ctx, "other", "closed"); n != 1 || len(obs.closed) != 1 || obs.closed[0] != "other" {
		t.Fatalf("closed %v (kicked %d)", obs.closed, n)
	}
	if s.CloseRoom(ctx, "missing", "x") != 0 || len(obs.closed) != 1 {
		t.Fatal("closing a missing room reported")
	}
	if len(s.Snapshot()) != 0 {
		t.Fatalf("snapshot after everyone left: %v", s.Snapshot())
	}
}

func TestBitrateCaps(t *testing.T) {
	ctx := context.Background()
	s := NewRoomService(10, metrics.Noop())
	pa, pb := &fakePeer{}, &fakePeer{}
	// No caps by default.
	_, st, _ := s.Join(ctx, "music", ident("a", controlpb.Role_SPEAKER), pa)
	if st.MaxBitrate != 0 {
		t.Fatalf("default cap = %d", st.MaxBitrate)
	}
	_, _, _ = s.Join(ctx, "general", ident("b", controlpb.Role_SPEAKER), pb)
	settings := func(p *fakePeer) []*controlpb.RoomSettings {
		p.mu.Lock()
		defer p.mu.Unlock()
		var out []*controlpb.RoomSettings
		for _, m := range p.ctrl {
			if rs := m.GetRoomSettings(); rs != nil {
				out = append(out, rs)
			}
		}
		return out
	}

	s.SetBitrateCaps(64000, map[string]int{"music": 128000})
	if got := settings(pa); len(got) != 1 || got[0].Room != "music" || got[0].MaxBitrate != 128000 {
		t.Fatalf("music members got %v", got)
	}
	if got := settings(pb); len(got) != 1 || got[0].MaxBitrate != 64000 {
		t.Fatalf("general members got %v", got)
	}
	_, st, _ = s.Join(ctx, "general", ident("c", controlpb.Role_SPEAKER), &fakePeer{})
	if st.MaxBitrate != 64000 {
		t.Fatalf("join cap = %d", st.MaxBitrate)
	}

	// Only rooms whose cap changed hear about it.
	s.SetBitrateCaps(48000, map[string]int{"music": 128000})
	if len(settings(pa)) != 1 || len(settings(pb)) != 2 {
		t.Fatalf("unchanged room notified: a=%v b=%v", settings(pa), settings(pb))
	}
	s.SetBitrateCaps(0, nil)
	if got := settings(pa); len(got) != 2 || got[1].MaxBitrate != 0 {
		t.Fatalf("cap removed: %v", got)
	}
}
