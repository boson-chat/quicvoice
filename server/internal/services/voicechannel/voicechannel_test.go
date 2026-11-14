package voicechannel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/boson-chat/quicvoice/server/internal/db/repositories/voiceroom"
	"github.com/boson-chat/quicvoice/server/internal/roles"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/services/settings"
)

func newSvc(t *testing.T, repo voiceroom.RoomRepositoryImpl, static []room.Def, policy settings.VoiceCreation) *VoiceChannelService {
	t.Helper()
	if repo == nil {
		repo, _ = voiceroom.NewRoomRepository("")
	}
	s, err := NewVoiceChannelService(context.Background(), Options{
		Repo: repo, Static: static, Settings: settings.NewSettingsService(settings.Options{Fallback: policy}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIDs(t *testing.T) {
	s := newSvc(t, nil, nil, settings.CreateRegistered)
	long := strings.Repeat("abcdefghij", 5)
	cases := []struct{ parent, name, want string }{
		{"", "Game Night", "game-night"},
		{"#boson", "Game Night!", "boson-game-night"},
		{"#c++", "x", "c-x"},
		{"", "---", "room"},
		{"", long, long[:32]},
	}
	for _, c := range cases {
		if got := s.newID(c.parent, c.name); got != c.want {
			t.Errorf("newID(%q,%q) = %q, want %q", c.parent, c.name, got, c.want)
		}
	}
	// Collisions get a numeric suffix within 32 chars.
	s.rooms[long[:32]] = voiceroom.Record{ID: long[:32]}
	if got := s.newID("", long); got != long[:30]+"-2" || len(got) > 32 {
		t.Errorf("suffix id %q", got)
	}
}

func TestCleanChannel(t *testing.T) {
	for in, want := range map[string]string{"": "", "#Boson": "#boson", " #a-b ": "#a-b", "#日本": "#日本"} {
		if got, err := cleanChannel(in); err != nil || got != want {
			t.Errorf("%q → %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"boson", "#", "#a b", "#a,b", "#" + strings.Repeat("x", 64), "#a\x01"} {
		if _, err := cleanChannel(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestStaticReconcile(t *testing.T) {
	ctx := context.Background()
	repo, _ := voiceroom.NewRoomRepository("")
	t0 := time.Unix(10, 0)
	_ = repo.Put(ctx, voiceroom.Record{ID: "old", Name: "Old", Static: true, CreatedAt: t0})
	_ = repo.Put(ctx, voiceroom.Record{ID: "music", Name: "mine", OwnerUserID: "u1", CreatedAt: t0, Position: 7})
	_ = repo.Put(ctx, voiceroom.Record{ID: "keep", Name: "Keep", OwnerUserID: "u1", CreatedAt: t0, Position: 9})

	s := newSvc(t, repo, []room.Def{{ID: "general", Name: "General"}, {ID: "music", Name: "Music"}}, settings.CreateAdmins)
	var ids []string
	for _, d := range s.Defs() {
		ids = append(ids, d.ID+":"+d.Name)
	}
	if strings.Join(ids, ",") != "general:General,music:Music,keep:Keep" {
		t.Fatalf("defs = %v", ids)
	}
	if !s.Has("music") || s.Has("old") {
		t.Fatal("Has")
	}
	if r, _ := s.get("music"); !r.Static {
		t.Fatal("dynamic room with a VOICE_ROOMS id not taken over")
	}
	stored, _ := repo.List(ctx)
	if len(stored) != 3 {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestAccessMatrix(t *testing.T) {
	s := newSvc(t, nil, nil, settings.CreateRegistered)
	speaker := &auth.Identity{UserID: "u", Account: "", Role: controlpb.Role_SPEAKER}
	listener := &auth.Identity{UserID: "l", Role: controlpb.Role_LISTENER}
	mod := &auth.Identity{UserID: "m", Role: controlpb.Role_MODERATOR}
	ctx := context.Background()
	for _, c := range []struct {
		rec         voiceroom.Record
		id          *auth.Identity
		join, speak bool
	}{
		{voiceroom.Record{ID: "a"}, speaker, true, true},
		{voiceroom.Record{ID: "a"}, listener, true, false},
		{voiceroom.Record{ID: "a", SpeakRole: roles.Oper}, speaker, true, false},
		{voiceroom.Record{ID: "a", MinRole: roles.Oper}, speaker, false, false},
		{voiceroom.Record{ID: "a", MinRole: roles.Oper}, mod, true, true},
		// No Ergo configured + a role lock: fails closed (err), admins pass.
		{voiceroom.Record{ID: "a", Parent: "#x"}, speaker, true, true},
		{voiceroom.Record{ID: "a", Parent: "#x", MinRole: roles.Op}, mod, true, true},
		{voiceroom.Record{ID: "b", Parent: "#x"}, &auth.Identity{UserID: "u", Rooms: []string{"a"}}, false, false},
	} {
		join, speak, _ := s.caller(ctx, c.id).access(c.rec)
		if join != c.join || speak != c.speak {
			t.Errorf("%+v %+v: join=%v speak=%v, want %v %v", c.rec, c.id, join, speak, c.join, c.speak)
		}
	}
	// No Account: Ergo isn't asked (empty standing), so locks deny without error.
	join, _, err := s.caller(ctx, speaker).access(voiceroom.Record{ID: "a", Parent: "#x", MinRole: roles.Voice})
	if join || err != nil {
		t.Fatalf("no account: %v %v", join, err)
	}
	// With an account and no Ergo: ErrUnavailable-style error.
	_, _, err = s.caller(ctx, &auth.Identity{UserID: "u", Account: "u", Role: controlpb.Role_SPEAKER}).
		access(voiceroom.Record{ID: "a", Parent: "#x", MinRole: roles.Voice})
	if err == nil {
		t.Fatal("no error without Ergo")
	}
}
