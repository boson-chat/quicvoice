package voicechannel

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/boson-chat/quicvoice/server/internal/db/repositories/voiceroom"
	"github.com/boson-chat/quicvoice/server/internal/roles"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/ergo"
	"github.com/boson-chat/quicvoice/server/internal/services/ergo/ergotest"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/services/settings"
)

// tok builds an Identity the way the auth service's verifiers do. perms nil
// = token without the perms claim (pre network roles); same for netRoles.
func tok(user, role string, netRoles, perms []string) *auth.Identity {
	id := &auth.Identity{UserID: "usr_" + user, DisplayName: user, Account: user, Role: controlpb.Role_SPEAKER}
	switch role {
	case "moderator":
		id.Role = controlpb.Role_MODERATOR
	case "listener":
		id.Role = controlpb.Role_LISTENER
	}
	if netRoles != nil {
		id.HasRoles, id.NetworkRoles = true, netRoles
	}
	if perms != nil {
		id.HasPerms, id.Perms = true, perms
	}
	return id
}

var none = []string{}

func newErgoSvc(t *testing.T, policy settings.VoiceCreation, perUser int) (*VoiceChannelService, *ergotest.Fake) {
	t.Helper()
	fe := ergotest.New(t, "etok")
	repo, _ := voiceroom.NewRoomRepository("")
	s, err := NewVoiceChannelService(context.Background(), Options{
		Repo:       repo,
		Settings:   settings.NewSettingsService(settings.Options{Fallback: policy}),
		Ergo:       ergo.NewErgoService(ergo.Options{URL: fe.URL(), Token: "etok", TTL: time.Nanosecond}),
		MaxPerUser: perUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, fe
}

// TestCreatePrecedence: every policy × old/new token × perms, with and
// without a parent channel the caller founded. Also checks that GET's
// permissions (can_create / create_scope) agree with what Create does.
func TestCreatePrecedence(t *testing.T) {
	type want struct{ noParent, founded bool }
	ids := []struct {
		name string
		id   *auth.Identity
		want map[settings.VoiceCreation]want
	}{
		{"old user (founder)", tok("alice", "", nil, nil), map[settings.VoiceCreation]want{
			settings.CreateAdmins: {false, false}, settings.CreateFounders: {false, true}, settings.CreateRegistered: {true, true}}},
		{"old auth admin", tok("root", "moderator", nil, nil), map[settings.VoiceCreation]want{
			settings.CreateAdmins: {true, true}, settings.CreateFounders: {true, true}, settings.CreateRegistered: {true, true}}},
		{"new, no perms (founder)", tok("alice", "", none, none), map[settings.VoiceCreation]want{
			settings.CreateAdmins: {false, false}, settings.CreateFounders: {false, false}, settings.CreateRegistered: {false, false}}},
		{"new, voice.create (founder)", tok("alice", "", none, []string{auth.PermVoiceCreate}), map[settings.VoiceCreation]want{
			settings.CreateAdmins: {false, false}, settings.CreateFounders: {false, true}, settings.CreateRegistered: {true, true}}},
		{"new, voice.create (not founder)", tok("bob", "", none, []string{auth.PermVoiceCreate}), map[settings.VoiceCreation]want{
			settings.CreateAdmins: {false, false}, settings.CreateFounders: {false, false}, settings.CreateRegistered: {true, true}}},
		{"new, voice.manage_any", tok("root", "", none, []string{auth.PermVoiceManageAny}), map[settings.VoiceCreation]want{
			settings.CreateAdmins: {true, true}, settings.CreateFounders: {true, true}, settings.CreateRegistered: {true, true}}},
		{"new, legacy admin role only", tok("root", "moderator", none, none), map[settings.VoiceCreation]want{
			settings.CreateAdmins: {false, false}, settings.CreateFounders: {false, false}, settings.CreateRegistered: {false, false}}},
		{"new, bypass_locks only", tok("root", "", none, []string{auth.PermVoiceBypassLocks}), map[settings.VoiceCreation]want{
			settings.CreateAdmins: {false, false}, settings.CreateFounders: {false, false}, settings.CreateRegistered: {false, false}}},
	}
	ctx := context.Background()
	for _, policy := range []settings.VoiceCreation{settings.CreateAdmins, settings.CreateFounders, settings.CreateRegistered} {
		for i, c := range ids {
			t.Run(fmt.Sprintf("%s/%s", policy, c.name), func(t *testing.T) {
				s, fe := newErgoSvc(t, policy, 0)
				fe.Register("#boson", "alice")
				w := c.want[policy]
				for _, tc := range []struct {
					parent string
					ok     bool
				}{{"", w.noParent}, {"#boson", w.founded}} {
					_, err := s.Create(ctx, c.id, CreateInput{Name: fmt.Sprintf("r%d", i), Parent: tc.parent})
					if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrForbidden)) {
						t.Errorf("parent %q: err = %v, want ok=%v", tc.parent, err, tc.ok)
					}
				}
				p := s.permissions(s.caller(ctx, c.id))
				anyOK := p.Scope == ScopeAny
				foundedOK := anyOK || (p.Scope == ScopeFounded && slices.Contains(p.FoundedChannels, "#boson"))
				if anyOK != w.noParent || foundedOK != w.founded || p.CanCreate != (w.noParent || w.founded) {
					t.Errorf("permissions %+v disagree with %+v", p, w)
				}
			})
		}
	}
}

func TestPerUserLimitAdminExemption(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		id     *auth.Identity
		exempt bool
	}{
		{"old admin", tok("root", "moderator", nil, nil), true},
		{"old user", tok("u", "", nil, nil), false},
		{"new manage_any", tok("m", "", none, []string{auth.PermVoiceManageAny}), true},
		{"new voice.create", tok("c", "", none, []string{auth.PermVoiceCreate}), false},
		{"new voice.create + legacy admin role", tok("x", "moderator", none, []string{auth.PermVoiceCreate}), false},
	} {
		s, _ := newErgoSvc(t, settings.CreateRegistered, 1)
		_, err1 := s.Create(ctx, c.id, CreateInput{Name: "one"})
		_, err2 := s.Create(ctx, c.id, CreateInput{Name: "two"})
		if err1 != nil || (err2 == nil) != c.exempt || (err2 != nil && !errors.Is(err2, ErrLimit)) {
			t.Errorf("%s: %v / %v, exempt=%v", c.name, err1, err2, c.exempt)
		}
	}
}

func TestManagePrecedence(t *testing.T) {
	ctx := context.Background()
	s, fe := newErgoSvc(t, settings.CreateRegistered, 0)
	fe.Register("#boson", "fred")
	owner := tok("owner", "", none, []string{auth.PermVoiceCreate})
	v, err := s.Create(ctx, owner, CreateInput{Name: "Hangout", Parent: "#boson"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		id   *auth.Identity
		want bool
	}{
		{"owner, new token without perms", tok("owner", "", none, none), true},
		{"parent founder, old token", tok("fred", "", nil, nil), true},
		{"parent founder, new token", tok("fred", "", none, none), true},
		{"old auth admin", tok("root", "moderator", nil, nil), true},
		{"new manage_any", tok("m", "", none, []string{auth.PermVoiceManageAny}), true},
		{"new legacy admin role, no perms", tok("root", "moderator", none, none), false},
		{"new bypass_locks only", tok("b", "", none, []string{auth.PermVoiceBypassLocks}), false},
		{"stranger", tok("x", "", nil, nil), false},
	} {
		got, err := s.caller(ctx, c.id).canManage(v.Room)
		if err != nil || got != c.want {
			t.Errorf("%s: canManage = %v %v, want %v", c.name, got, err, c.want)
		}
		_, err = s.Update(ctx, c.id, v.Room.ID, UpdateInput{Position: new(int)})
		if (err == nil) != c.want {
			t.Errorf("%s: Update err = %v, want ok=%v", c.name, err, c.want)
		}
	}
	// Setting an oper lock is for network admins: manage_any, or the
	// legacy role on old tokens.
	oper := roles.Oper
	for _, c := range []struct {
		name string
		id   *auth.Identity
		want bool
	}{
		{"old admin", tok("root", "moderator", nil, nil), true},
		{"new manage_any", tok("m", "", none, []string{auth.PermVoiceManageAny}), true},
		{"founder, new token", tok("fred", "", none, none), false},
	} {
		_, err := s.Update(ctx, c.id, v.Room.ID, UpdateInput{MinRole: &oper})
		if (err == nil) != c.want {
			t.Errorf("oper lock by %s: %v, want ok=%v", c.name, err, c.want)
		}
	}
}

func TestAccessWithNetworkClaims(t *testing.T) {
	s := newSvc(t, nil, nil, settings.CreateRegistered)
	ctx := context.Background()
	dj := voiceroom.Record{ID: "a", AllowedRoles: []string{"DJ", "Owner"}}
	bypass := []string{auth.PermVoiceBypassLocks}
	for _, c := range []struct {
		name        string
		rec         voiceroom.Record
		id          *auth.Identity
		join, speak bool
	}{
		// allowed_roles
		{"old user, allowed_roles", dj, tok("u", "", nil, nil), false, false},
		{"old auth admin, allowed_roles", dj, tok("m", "moderator", nil, nil), true, true},
		{"new, holds dj (case-insensitive)", dj, tok("u", "", []string{"Member", "dj"}, none), true, true},
		{"new, other roles", dj, tok("u", "", []string{"Member"}, none), false, false},
		{"new, no roles claim", dj, tok("u", "", nil, none), false, false},
		{"new, bypass_locks", dj, tok("u", "", none, bypass), true, true},
		{"new, bypass_locks, listener token", dj, tok("u", "listener", none, bypass), true, false},
		{"new, legacy admin role ignored", dj, tok("m", "moderator", none, none), false, false},
		{"new, manage_any doesn't bypass", dj, tok("m", "", none, []string{auth.PermVoiceManageAny}), false, false},
		{"new, holds role, listener token", dj, tok("u", "listener", []string{"Owner"}, none), true, false},
		{"no restriction", voiceroom.Record{ID: "a"}, tok("u", "", nil, none), true, true},
		// rooms claim still wins over bypass
		{"rooms claim", dj, &auth.Identity{UserID: "u", Rooms: []string{"b"}, HasPerms: true, Perms: bypass}, false, false},
		// oper / IRC locks
		{"oper lock, bypass", voiceroom.Record{ID: "a", MinRole: roles.Oper}, tok("u", "", none, bypass), true, true},
		{"oper lock, manage_any", voiceroom.Record{ID: "a", MinRole: roles.Oper}, tok("u", "", none, []string{auth.PermVoiceManageAny}), false, false},
		{"oper lock, new legacy admin", voiceroom.Record{ID: "a", MinRole: roles.Oper}, tok("m", "moderator", none, none), false, false},
		{"speak oper, bypass", voiceroom.Record{ID: "a", SpeakRole: roles.Oper}, tok("u", "", none, bypass), true, true},
		{"irc lock, bypass (no Ergo)", voiceroom.Record{ID: "a", Parent: "#x", MinRole: roles.Op, RequireChannelMembership: true}, tok("u", "", none, bypass), true, true},
	} {
		join, speak, _ := s.caller(ctx, c.id).access(c.rec)
		if join != c.join || speak != c.speak {
			t.Errorf("%s: join=%v speak=%v, want %v %v", c.name, join, speak, c.join, c.speak)
		}
	}
}

func TestAllowedRolesCombineWithIRCLocks(t *testing.T) {
	ctx := context.Background()
	s, fe := newErgoSvc(t, settings.CreateRegistered, 0)
	fe.Join("vic", "#boson", "v")
	rec := voiceroom.Record{ID: "a", Parent: "#boson", MinRole: roles.Voice, AllowedRoles: []string{"DJ"}}
	for _, c := range []struct {
		id   *auth.Identity
		join bool
	}{
		{tok("vic", "", []string{"DJ"}, none), true},
		{tok("vic", "", []string{"Member"}, none), false}, // +v but not DJ
		{tok("mia", "", []string{"DJ"}, none), false},     // DJ but no +v
	} {
		if join, _, err := s.caller(ctx, c.id).access(rec); join != c.join || err != nil {
			t.Errorf("%s %v: join=%v err=%v", c.id.Account, c.id.NetworkRoles, join, err)
		}
	}
}

func TestAuthorizeJoinAllowedRolesText(t *testing.T) {
	ctx := context.Background()
	s := newSvc(t, nil, nil, settings.CreateRegistered)
	mgr := tok("m", "", none, []string{auth.PermVoiceManageAny})
	v, err := s.Create(ctx, mgr, CreateInput{Name: "Booth", AllowedRoles: []string{"DJ", "Owner"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AuthorizeJoin(ctx, v.Room.ID, tok("u", "", []string{"Member"}, none))
	if !errors.Is(err, room.ErrForbidden) || !strings.Contains(err.Error(), "needs one of these roles: DJ, Owner") {
		t.Fatalf("err = %v", err)
	}
	if ok, err := s.AuthorizeJoin(ctx, v.Room.ID, tok("u", "", []string{"owner"}, none)); err != nil || !ok {
		t.Fatalf("owner role: %v %v", ok, err)
	}
}

func TestAllowedRolesValidation(t *testing.T) {
	long := strings.Repeat("é", MaxRoleNameLen)
	many := make([]string, MaxAllowedRoles)
	for i := range many {
		many[i] = fmt.Sprintf("r%d", i)
	}
	for _, c := range []struct {
		in   []string
		want []string
		ok   bool
	}{
		{nil, nil, true},
		{[]string{}, nil, true},
		{[]string{" DJ ", "Owner", "dj", "OWNER"}, []string{"DJ", "Owner"}, true},
		{[]string{long}, []string{long}, true},
		{many, many, true},
		{append(append([]string{}, many...), "R0"), many, true}, // dupes don't count
		{append(append([]string{}, many...), "extra"), nil, false},
		{[]string{""}, nil, false},
		{[]string{"   "}, nil, false},
		{[]string{long + "x"}, nil, false},
		{[]string{"a\u0007b"}, nil, false},
		{[]string{"a\nb"}, nil, false},
		{[]string{"\xff"}, nil, false},
	} {
		got, err := cleanAllowedRoles(c.in)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrInvalid)) || (c.ok && !slices.Equal(got, c.want)) {
			t.Errorf("%q → %q %v, want %q ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}

func TestAllowedRolesUpdate(t *testing.T) {
	ctx := context.Background()
	s := newSvc(t, nil, nil, settings.CreateRegistered)
	owner := tok("o", "", nil, nil)
	v, err := s.Create(ctx, owner, CreateInput{Name: "Booth", AllowedRoles: []string{"DJ"}})
	if err != nil || !slices.Equal(v.Room.AllowedRoles, []string{"DJ"}) {
		t.Fatalf("create: %+v %v", v.Room, err)
	}
	// nil = unchanged
	if v, _ = s.Update(ctx, owner, v.Room.ID, UpdateInput{Name: ptr("Booth 2")}); !slices.Equal(v.Room.AllowedRoles, []string{"DJ"}) {
		t.Fatalf("unchanged: %+v", v.Room)
	}
	if v, _ = s.Update(ctx, owner, v.Room.ID, UpdateInput{AllowedRoles: &[]string{"Owner", "owner"}}); !slices.Equal(v.Room.AllowedRoles, []string{"Owner"}) {
		t.Fatalf("replace: %+v", v.Room)
	}
	if _, err := s.Update(ctx, owner, v.Room.ID, UpdateInput{AllowedRoles: &[]string{""}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid: %v", err)
	}
	if v, _ = s.Update(ctx, owner, v.Room.ID, UpdateInput{AllowedRoles: &[]string{}}); v.Room.AllowedRoles != nil {
		t.Fatalf("clear: %+v", v.Room)
	}
}

func ptr[T any](v T) *T { return &v }
