package room

import (
	"context"
	"errors"
	"testing"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/metrics"
)

func callIdent(name string, rooms ...string) *auth.Identity {
	return &auth.Identity{UserID: name, DisplayName: name, Role: controlpb.Role_SPEAKER, Rooms: rooms}
}

func TestIsPrivateCall(t *testing.T) {
	for room, want := range map[string]bool{
		"call:01JABC": true, "call:": false, "general": false, "call-x": false, "": false, "xcall:1": false,
	} {
		if got := IsPrivateCall(room); got != want {
			t.Errorf("IsPrivateCall(%q) = %v, want %v", room, got, want)
		}
	}
}

func TestPrivateCallJoinNeedsTheRoomInTheToken(t *testing.T) {
	ctx := context.Background()
	const room = "call:01JCALL"
	// Strict, with a registry that doesn't (and can't) hold the room.
	s := NewRoomService(10, metrics.Noop(), WithRooms([]Def{{ID: "general", Name: "General"}}, true))

	// Any-room token (no rooms claim): refused.
	if _, _, err := s.Join(ctx, room, callIdent("mallory"), &fakePeer{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("any-room token: %v, want ErrForbidden", err)
	}
	// A token for other rooms: refused.
	if _, _, err := s.Join(ctx, room, callIdent("mallory", "general", "call:other"), &fakePeer{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other rooms: %v, want ErrForbidden", err)
	}
	// A guest token naming the room: refused.
	g := callIdent("guest:x", room)
	g.Guest = true
	if _, _, err := s.Join(ctx, room, g, &fakePeer{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("guest: %v, want ErrForbidden", err)
	}
	// The two people on the call: in, although strict and unregistered.
	a, _, err := s.Join(ctx, room, callIdent("alice", room), &fakePeer{})
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	b, state, err := s.Join(ctx, room, callIdent("bob", room), &fakePeer{})
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	if len(state.Participants) != 2 || state.Room != room {
		t.Fatalf("room state %v", state)
	}
	// Gone once empty.
	s.Leave(ctx, a, "bye")
	s.Leave(ctx, b, "bye")
	if snap := s.Snapshot(); len(snap) != 0 {
		t.Fatalf("room left behind: %v", snap)
	}
	// Non-private rooms still follow strict.
	if _, _, err := s.Join(ctx, "nope", callIdent("alice"), &fakePeer{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("strict: %v, want ErrNotFound", err)
	}
}

func TestPrivateCallsAreNeverListed(t *testing.T) {
	ctx := context.Background()
	const room = "call:01JCALL"
	// Not strict: ad-hoc rooms are listed, private calls aren't.
	s := NewRoomService(10, metrics.Noop(), WithRooms(nil, false))
	if _, _, err := s.Join(ctx, room, callIdent("alice", room), &fakePeer{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Join(ctx, "hangout", callIdent("carol"), &fakePeer{}); err != nil {
		t.Fatal(err)
	}
	for _, who := range []*auth.Identity{callIdent("carol"), callIdent("alice", room), callIdent("bob", room, "hangout")} {
		for _, info := range s.List(ctx, who) {
			if IsPrivateCall(info.ID) {
				t.Fatalf("%s sees private room %q in the list", who.UserID, info.ID)
			}
		}
	}
	if got := s.List(ctx, callIdent("carol")); len(got) != 1 || got[0].ID != "hangout" {
		t.Fatalf("list %v", got)
	}
}

func TestPrivateCallSkipsRoomLocks(t *testing.T) {
	ctx := context.Background()
	const room = "call:01JCALL"
	pol := denyAll{}
	s := NewRoomService(10, metrics.Noop(), WithRooms(nil, true), WithJoinPolicy(pol))
	if _, _, err := s.Join(ctx, room, callIdent("alice", room), &fakePeer{}); err != nil {
		t.Fatalf("locks applied to a private call: %v", err)
	}
}

type denyAll struct{}

func (denyAll) AuthorizeJoin(context.Context, string, *auth.Identity) (bool, error) {
	return false, ErrForbidden
}
