package voicechannel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/server/internal/db/repositories/voiceroom"
	"github.com/boson-chat/quicvoice/server/internal/roles"
	"github.com/boson-chat/quicvoice/server/internal/services/bridge"
	"github.com/boson-chat/quicvoice/server/internal/services/bridge/bridgetest"
	"github.com/boson-chat/quicvoice/server/internal/services/ergo"
	"github.com/boson-chat/quicvoice/server/internal/services/ergo/ergotest"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/services/settings"
)

func newPresenceSvc(t *testing.T, o Options) *VoiceChannelService {
	t.Helper()
	repo, _ := voiceroom.NewRoomRepository("")
	o.Repo = repo
	o.Settings = settings.NewSettingsService(settings.Options{Fallback: settings.CreateRegistered})
	s, err := NewVoiceChannelService(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// guestRooms: what a guest named vic, in #boson (as op there) and not in
// #other, gets in each kind of room.
var guestRooms = []struct {
	name        string
	rec         voiceroom.Record
	join, speak bool
	lock        string // AuthorizeJoin's refusal / View.LockReason
}{
	{"open", voiceroom.Record{ID: "a", Parent: "#boson"}, true, true, ""},
	{"membership, in", voiceroom.Record{ID: "a", Parent: "#Boson", RequireChannelMembership: true}, true, true, ""},
	{"membership, not in", voiceroom.Record{ID: "a", Parent: "#other", RequireChannelMembership: true}, false, false, "join #other on IRC first"},
	{"membership, speak op", voiceroom.Record{ID: "a", Parent: "#boson", RequireChannelMembership: true, SpeakRole: roles.Op}, true, false, ""},
	// Guests are members only, whatever prefix their nick has.
	{"min_role voice", voiceroom.Record{ID: "a", Parent: "#boson", MinRole: roles.Voice}, false, false,
		"needs voice or higher in #boson (sign in: guests count as members)"},
	{"min_role op + membership", voiceroom.Record{ID: "a", Parent: "#boson", MinRole: roles.Op, SpeakRole: roles.Op, RequireChannelMembership: true}, false, false,
		"needs op or higher in #boson (sign in: guests count as members)"},
	{"allowed_roles", voiceroom.Record{ID: "a", Parent: "#boson", AllowedRoles: []string{"DJ"}}, false, false,
		"needs one of these roles: DJ (sign in: guests have none)"},
}

func checkGuestRooms(t *testing.T, s *VoiceChannelService) {
	t.Helper()
	ctx := context.Background()
	id := guestIDs()["guest"]
	for _, c := range guestRooms {
		cl := s.caller(ctx, id)
		join, speak, err := cl.access(c.rec)
		if err != nil || join != c.join || speak != c.speak {
			t.Errorf("%s: join=%v speak=%v err=%v, want %v %v", c.name, join, speak, err, c.join, c.speak)
		}
		if v := s.view(s.caller(ctx, id), c.rec); v.LockReason != c.lock {
			t.Errorf("%s: lock reason %q, want %q", c.name, v.LockReason, c.lock)
		}
		if cl.admin() || cl.bypass() || cl.createPerm() {
			t.Errorf("%s: guest got privileges", c.name)
		}
	}
}

// TestGuestMembershipErgo: on an Ergo install a guest's membership comes
// from /v1/whois by nick, only for a client that isn't logged in.
func TestGuestMembershipErgo(t *testing.T) {
	fe := ergotest.New(t, "etok")
	fe.JoinGuest("vic", "#boson", "o")
	fe.Register("#boson", "vic")
	s := newPresenceSvc(t, Options{Ergo: ergo.NewErgoService(ergo.Options{URL: fe.URL(), Token: "etok", TTL: time.Nanosecond})})
	checkGuestRooms(t, s)

	// A logged-in client using the nick is someone else.
	fe2 := ergotest.New(t, "etok")
	fe2.Join("vic", "#boson", "")
	s2 := newPresenceSvc(t, Options{Ergo: ergo.NewErgoService(ergo.Options{URL: fe2.URL(), Token: "etok"})})
	rec := voiceroom.Record{ID: "a", Parent: "#boson", RequireChannelMembership: true}
	if join, _, err := s2.caller(context.Background(), guestIDs()["guest"]).access(rec); join || err != nil {
		t.Fatalf("logged-in vic let the guest in: %v %v", join, err)
	}
}

// TestGuestMembershipBridge: on a bridged install (no Ergo) it comes from
// evrd's managed API.
func TestGuestMembershipBridge(t *testing.T) {
	fb := bridgetest.New(t, "btok")
	fb.Join("vic", "#boson")
	fb.Watch("#other")
	br := bridge.NewBridgeService(bridge.Options{URL: fb.URL(), Token: "btok", TTL: time.Nanosecond})
	s := newPresenceSvc(t, Options{Presence: br})
	checkGuestRooms(t, s)

	// One lookup per request and channel.
	calls := fb.Calls.Load()
	if _, err := s.List(context.Background(), guestIDs()["guest"]); err != nil {
		t.Fatal(err)
	}
	if fb.Calls.Load() != calls {
		t.Fatalf("listing an empty registry asked evrd %d times", fb.Calls.Load()-calls)
	}
	for _, in := range []CreateInput{
		{Name: "One", Parent: "#boson", RequireChannelMembership: true},
		{Name: "Two", Parent: "#boson", RequireChannelMembership: true, SpeakRole: roles.Voice},
	} {
		if _, err := s.Create(context.Background(), tok("root", "moderator", nil, nil), in); err != nil {
			t.Fatal(err)
		}
	}
	calls = fb.Calls.Load()
	if _, err := s.List(context.Background(), guestIDs()["guest"]); err != nil {
		t.Fatal(err)
	}
	if n := fb.Calls.Load() - calls; n != 1 {
		t.Fatalf("two #boson rooms asked evrd %d times, want 1", n)
	}

	// evrd down: fail closed, as "unavailable".
	fb.SetStatus(500)
	rec := voiceroom.Record{ID: "a", Parent: "#boson", RequireChannelMembership: true}
	if _, _, err := s.caller(context.Background(), guestIDs()["guest"]).access(rec); !errors.Is(err, bridge.ErrUnavailable) {
		t.Fatalf("down: %v", err)
	}
	if v := s.view(s.caller(context.Background(), guestIDs()["guest"]), rec); v.CanJoin || v.LockReason != ErrUnavailable.Error() {
		t.Fatalf("down view: %+v", v)
	}
}

// TestGuestOpenChannelRoomNoIRC: a room under a channel with every lock at
// its lowest (who can join/talk: everyone, membership off) is open to a
// guest with no IRC lookup at all: no Ergo, no bridge (an add-on install).
func TestGuestOpenChannelRoomNoIRC(t *testing.T) {
	ctx := context.Background()
	s := newPresenceSvc(t, Options{})
	admin := tok("root", "moderator", nil, nil)
	v, err := s.Create(ctx, admin, CreateInput{Name: "ircforever", Parent: "#ircforever", MinRole: roles.Member, SpeakRole: roles.Member})
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.List(ctx, guestIDs()["guest"])
	if err != nil || len(l.Rooms) != 1 || !l.Rooms[0].CanJoin || !l.Rooms[0].CanSpeak || l.Rooms[0].LockReason != "" {
		t.Fatalf("listing: %+v %v", l, err)
	}
	if speak, err := s.AuthorizeJoin(ctx, v.Room.ID, guestIDs()["guest"]); !speak || err != nil {
		t.Fatalf("join: %v %v", speak, err)
	}
	// With the membership lock on and nothing to ask: unavailable, not
	// "join it first".
	on := true
	if _, err := s.Update(ctx, admin, v.Room.ID, UpdateInput{RequireChannelMembership: &on}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthorizeJoin(ctx, v.Room.ID, guestIDs()["guest"]); !errors.Is(err, room.ErrForbidden) || !strings.Contains(err.Error(), ErrUnavailable.Error()) {
		t.Fatalf("membership, no lookup: %v", err)
	}
}

// AuthorizeJoin refuses with the same words as the listing's lock reason.
func TestGuestAuthorizeJoinText(t *testing.T) {
	ctx := context.Background()
	fb := bridgetest.New(t, "btok")
	fb.Join("vic", "#boson")
	s := newPresenceSvc(t, Options{Presence: bridge.NewBridgeService(bridge.Options{URL: fb.URL(), Token: "btok"})})
	admin := tok("root", "moderator", nil, nil)
	in, _ := s.Create(ctx, admin, CreateInput{Name: "In", Parent: "#boson", RequireChannelMembership: true})
	out, _ := s.Create(ctx, admin, CreateInput{Name: "Out", Parent: "#elsewhere", RequireChannelMembership: true})
	if _, err := s.AuthorizeJoin(ctx, in.Room.ID, guestIDs()["guest"]); err != nil {
		t.Fatalf("in: %v", err)
	}
	_, err := s.AuthorizeJoin(ctx, out.Room.ID, guestIDs()["guest"])
	if !errors.Is(err, room.ErrForbidden) || !strings.HasSuffix(err.Error(), "join #elsewhere on IRC first") {
		t.Fatalf("out: %v", err)
	}
}
