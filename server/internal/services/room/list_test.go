package room

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/metrics"
	"github.com/boson-chat/quicvoice/wire"
)

var testDefs = []Def{{"general", "General"}, {"gaming", "Gaming"}}

func voiced(seq uint16) []byte {
	return wire.AppendUplink(nil, &wire.Uplink{Audio: wire.Audio{Voice: true, Seq: seq, Timestamp: 960, Payload: []byte("opus")}})
}

func ids(rooms []Info) []string {
	out := make([]string, 0, len(rooms))
	for _, r := range rooms {
		out = append(out, r.ID)
	}
	return out
}

func TestStrictJoin(t *testing.T) {
	ctx := context.Background()
	s := NewRoomService(10, metrics.Noop(), WithRooms(testDefs, true)).(*RoomService)
	if _, _, err := s.Join(ctx, "nope", ident("a", controlpb.Role_SPEAKER), &fakePeer{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if len(s.rooms) != 0 {
		t.Fatal("rejected join left a room behind")
	}
	if _, _, err := s.Join(ctx, "gaming", ident("a", controlpb.Role_SPEAKER), &fakePeer{}); err != nil {
		t.Fatal(err)
	}
	// The token's rooms claim still applies to configured rooms.
	limited := &auth.Identity{UserID: "b", DisplayName: "b", Role: controlpb.Role_SPEAKER, Rooms: []string{"gaming"}}
	if _, _, err := s.Join(ctx, "general", limited, &fakePeer{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}

func TestListConfigOrderAndParticipants(t *testing.T) {
	ctx := context.Background()
	s := NewRoomService(10, metrics.Noop(), WithRooms(testDefs, true))

	got := s.List(ctx, ident("x", controlpb.Role_SPEAKER))
	if len(got) != 2 || got[0].ID != "general" || got[0].Name != "General" || got[1].ID != "gaming" {
		t.Fatalf("empty list = %+v", got)
	}
	if got[0].Participants == nil || len(got[0].Participants) != 0 {
		t.Fatalf("empty room participants = %#v, want empty non-nil", got[0].Participants)
	}

	a, _, _ := s.Join(ctx, "gaming", &auth.Identity{UserID: "usr_a", DisplayName: "alice", Role: controlpb.Role_SPEAKER}, &fakePeer{})
	b, _, _ := s.Join(ctx, "gaming", ident("bob", controlpb.Role_SPEAKER), &fakePeer{})
	s.SetMute(ctx, b, true, true)

	got = s.List(ctx, ident("x", controlpb.Role_SPEAKER))
	ps := got[1].Participants
	if len(ps) != 2 {
		t.Fatalf("gaming participants = %+v", ps)
	}
	want := []ParticipantInfo{
		{SessionID: a.SessionID, UserID: "usr_a", Name: "alice"},
		{SessionID: b.SessionID, UserID: "bob", Name: "bob", Muted: true, Deafened: true},
	}
	for i := range want {
		if ps[i] != want[i] {
			t.Errorf("participant %d = %+v, want %+v", i, ps[i], want[i])
		}
	}

	// Rooms the token can't join are left out.
	limited := &auth.Identity{UserID: "l", Rooms: []string{"gaming", "elsewhere"}}
	if got := ids(s.List(ctx, limited)); len(got) != 1 || got[0] != "gaming" {
		t.Fatalf("limited list = %v", got)
	}
}

func TestListAdhocWhenNotStrict(t *testing.T) {
	ctx := context.Background()
	s := NewRoomService(10, metrics.Noop(), WithRooms(testDefs, false))
	_, _, _ = s.Join(ctx, "zeta", ident("a", controlpb.Role_SPEAKER), &fakePeer{})
	_, _, _ = s.Join(ctx, "alpha", ident("b", controlpb.Role_SPEAKER), &fakePeer{})
	_, _, _ = s.Join(ctx, "general", ident("c", controlpb.Role_SPEAKER), &fakePeer{})
	got := ids(s.List(ctx, ident("x", controlpb.Role_SPEAKER)))
	want := []string{"general", "gaming", "alpha", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("list = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("list = %v, want %v", got, want)
		}
	}
}

func TestSpeakingWindow(t *testing.T) {
	ctx := context.Background()
	var now atomic.Int64
	now.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	clock := func() time.Time { return time.Unix(0, now.Load()) }
	s := NewRoomService(10, metrics.Noop(), WithRooms(testDefs, true), WithClock(clock))
	a, _, _ := s.Join(ctx, "general", ident("a", controlpb.Role_SPEAKER), &fakePeer{})
	_, _, _ = s.Join(ctx, "general", ident("b", controlpb.Role_SPEAKER), &fakePeer{})

	speaking := func() bool {
		for _, p := range s.List(ctx, ident("x", controlpb.Role_SPEAKER))[0].Participants {
			if p.SessionID == a.SessionID {
				return p.Speaking
			}
		}
		t.Fatal("a not listed")
		return false
	}

	if speaking() {
		t.Fatal("speaking before any audio")
	}
	s.Forward(a, uplink(1)) // unvoiced (DTX/silence)
	if speaking() {
		t.Fatal("unvoiced frame counted as speaking")
	}
	s.Forward(a, voiced(2))
	if !speaking() {
		t.Fatal("not speaking after a voiced frame")
	}
	now.Add(int64(SpeakingWindow) - 1)
	if !speaking() {
		t.Fatal("stopped speaking inside the window")
	}
	now.Add(1)
	if speaking() {
		t.Fatal("still speaking after the window")
	}
	// Muted audio is dropped and doesn't count.
	s.SetMute(ctx, a, true, false)
	s.Forward(a, voiced(3))
	if speaking() {
		t.Fatal("muted member counted as speaking")
	}
}

// TestListConcurrent exercises List against joins, leaves and the audio path
// under -race.
func TestListConcurrent(t *testing.T) {
	ctx := context.Background()
	s := NewRoomService(50, metrics.Noop(), WithRooms(testDefs, false))
	stay, _, _ := s.Join(ctx, "general", ident("stay", controlpb.Role_SPEAKER), &fakePeer{})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			m, _, err := s.Join(ctx, []string{"general", "gaming", "adhoc"}[i%3], ident("churn", controlpb.Role_SPEAKER), &fakePeer{})
			if err == nil {
				s.SetMute(ctx, m, i%2 == 0, false)
				s.Leave(ctx, m, "")
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.Forward(stay, voiced(uint16(i)))
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = s.List(ctx, ident("x", controlpb.Role_SPEAKER))
		}
	}()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}
