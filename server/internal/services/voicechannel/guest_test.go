package voicechannel

import (
	"context"
	"errors"
	"testing"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/boson-chat/quicvoice/server/internal/db/repositories/voiceroom"
	"github.com/boson-chat/quicvoice/server/internal/roles"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/settings"
)

// guests as the verifier would build them, plus forged ones that a buggy
// verifier might produce (staff role, perms, no perms claim, an account).
func guestIDs() map[string]*auth.Identity {
	return map[string]*auth.Identity{
		"guest": {UserID: "guest:g1", DisplayName: "vic", Role: controlpb.Role_SPEAKER, Guest: true,
			HasRoles: true, NetworkRoles: []string{}, HasPerms: true, Perms: []string{}},
		"legacy-shaped moderator": {UserID: "guest:g2", DisplayName: "vic", Role: controlpb.Role_MODERATOR, Guest: true},
		"with perms": {UserID: "guest:g3", DisplayName: "vic", Role: controlpb.Role_ADMIN, Guest: true, HasPerms: true,
			Perms: []string{auth.PermVoiceManageAny, auth.PermVoiceBypassLocks, auth.PermVoiceCreate}},
	}
}

// TestGuestNeverPrivileged: guests are never admin/bypass/create, not even
// on the legacy role path, under every creation policy.
func TestGuestNeverPrivileged(t *testing.T) {
	ctx := context.Background()
	for _, policy := range []settings.VoiceCreation{settings.CreateAdmins, settings.CreateFounders, settings.CreateRegistered} {
		s, fe := newErgoSvc(t, policy, 0)
		fe.Register("#boson", "vic") // an account named like the guest's nick
		for name, id := range guestIDs() {
			c := s.caller(ctx, id)
			if c.admin() || c.bypass() || c.createPerm() || c.legacyAdmin() {
				t.Errorf("%s %s: admin=%v bypass=%v create=%v legacy=%v", policy, name, c.admin(), c.bypass(), c.createPerm(), c.legacyAdmin())
			}
			if r, _ := c.role("#boson"); r != roles.Member {
				t.Errorf("%s %s: role %v", policy, name, r)
			}
			p := s.permissions(c)
			if p.CanCreate || p.ManageAny || p.BypassLocks || p.Scope != ScopeNone {
				t.Errorf("%s %s: permissions %+v", policy, name, p)
			}
			for _, parent := range []string{"", "#boson"} {
				if _, err := s.Create(ctx, id, CreateInput{Name: "Guest room", Parent: parent}); !errors.Is(err, ErrForbidden) {
					t.Errorf("%s %s parent %q: create err = %v", policy, name, parent, err)
				}
			}
		}
	}
}

// TestGuestAccess: guests get into open rooms and are kept out of rooms
// locked by channel membership, min_role or allowed_roles, even when their
// nick matches an account that would pass (no Ergo lookup for guests).
func TestGuestAccess(t *testing.T) {
	ctx := context.Background()
	s, fe := newErgoSvc(t, settings.CreateRegistered, 0)
	fe.Register("#boson", "vic")
	fe.Join("vic", "#boson", "q")
	for _, c := range []struct {
		name        string
		rec         voiceroom.Record
		join, speak bool
	}{
		{"open, no parent", voiceroom.Record{ID: "a"}, true, true},
		{"open, parent", voiceroom.Record{ID: "a", Parent: "#boson"}, true, true},
		{"require_channel_membership", voiceroom.Record{ID: "a", Parent: "#boson", RequireChannelMembership: true}, false, false},
		{"min_role voice", voiceroom.Record{ID: "a", Parent: "#boson", MinRole: roles.Voice}, false, false},
		{"min_role oper, no parent", voiceroom.Record{ID: "a", MinRole: roles.Oper}, false, false},
		{"allowed_roles", voiceroom.Record{ID: "a", AllowedRoles: []string{"Member"}}, false, false},
		{"speak_role op", voiceroom.Record{ID: "a", Parent: "#boson", SpeakRole: roles.Op}, true, false},
		{"speak_role oper, no parent", voiceroom.Record{ID: "a", SpeakRole: roles.Oper}, true, false},
	} {
		for name, id := range guestIDs() {
			join, speak, err := s.caller(ctx, id).access(c.rec)
			if err != nil || join != c.join || speak != c.speak {
				t.Errorf("%s / %s: join=%v speak=%v err=%v, want %v %v", c.name, name, join, speak, err, c.join, c.speak)
			}
		}
	}
	// The account itself does pass the locks (sanity check of the fixture).
	if join, _, err := s.caller(ctx, tok("vic", "", none, none)).access(voiceroom.Record{ID: "a", Parent: "#boson", MinRole: roles.Voice, RequireChannelMembership: true}); !join || err != nil {
		t.Fatalf("account vic: join=%v err=%v", join, err)
	}
}
