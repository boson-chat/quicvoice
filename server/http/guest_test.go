package http_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/golang-jwt/jwt/v5"
)

// guestToken mints a guest voice token the way the auth service does.
func guestToken(t *testing.T, guestID, nick, role string) string {
	t.Helper()
	none := []string{}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, auth.Claims{
		Name: nick, Role: role, Guest: true, Roles: &none, Perms: &none,
		RegisteredClaims: jwt.RegisteredClaims{Subject: "guest:" + guestID, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))},
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestGuestVoice: a guest token lists rooms without any create/manage
// rights, joins open rooms only, and shows up as guest:true in the
// participant list and in ParticipantJoined.
func TestGuestVoice(t *testing.T) {
	ctx := context.Background()
	a := newAPI(t, apiOpts{policy: "registered", static: []room.Def{{ID: "general", Name: "General"}}})
	a.ergo.Register("#boson", "vic")
	a.ergo.Join("vic", "#boson", "q")
	admin := token(t, "root", "admin", time.Minute)
	locked := map[string]string{
		"membership": a.create(t, admin, `{"name":"Members","parent":"#boson","require_channel_membership":true}`, 201).ID,
		"min_role":   a.create(t, admin, `{"name":"Voiced","parent":"#boson","min_role":"voice"}`, 201).ID,
		"oper":       a.create(t, admin, `{"name":"Admins","min_role":"oper"}`, 201).ID,
		"allowed":    a.create(t, admin, `{"name":"Booth","allowed_roles":["Member"]}`, 201).ID,
	}
	open := a.create(t, admin, `{"name":"Open","parent":"#boson"}`, 201).ID

	// Nick "vic" is the founder account's name; a guest must not inherit it.
	// role "admin" in a guest token is ignored as well.
	gtok := guestToken(t, "abcdef0123456789", "vic", "admin")
	l := a.list(t, gtok)
	if p := l.Permissions; p.CanCreate || p.CanManageAny || p.BypassLocks || p.CreateScope != "none" {
		t.Fatalf("guest permissions %+v", p)
	}
	a.create(t, gtok, `{"name":"Mine"}`, 403)
	a.create(t, gtok, `{"name":"Mine","parent":"#boson"}`, 403)
	for name, id := range locked {
		if r := l.room(id); r.CanJoin || r.CanManage {
			t.Errorf("%s: guest can_join=%v can_manage=%v", name, r.CanJoin, r.CanManage)
		}
	}
	for _, id := range []string{"general", open} {
		if r := l.room(id); !r.CanJoin || !r.CanSpeak || r.CanManage {
			t.Errorf("%s: guest %+v", id, r)
		}
	}
	if resp, _ := do(t, "DELETE", a.url+"/v1/rooms/"+open, "Bearer "+gtok, ""); resp.StatusCode != 403 {
		t.Errorf("guest delete: %d", resp.StatusCode)
	}

	// Same decisions at Join.
	gid, err := auth.NewJWTAuthService(secret).Verify(ctx, gtok)
	if err != nil {
		t.Fatal(err)
	}
	for name, id := range locked {
		if _, _, err := a.rooms.Join(ctx, id, gid, &recPeer{}); err == nil {
			t.Errorf("%s: guest joined", name)
		}
	}
	watcher := &recPeer{}
	_, _, _ = a.rooms.Join(ctx, "general", &auth.Identity{UserID: "usr_1", DisplayName: "alice", Role: controlpb.Role_SPEAKER}, watcher)
	m, state, err := a.rooms.Join(ctx, "general", gid, &recPeer{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.rooms.Leave(ctx, m, "test")
	var joined *controlpb.Participant
	watcher.mu.Lock()
	for _, msg := range watcher.msgs {
		if pj := msg.GetParticipantJoined(); pj != nil {
			joined = pj.Participant
		}
	}
	watcher.mu.Unlock()
	if joined == nil || !joined.Guest || joined.UserId != "guest:abcdef0123456789" || joined.DisplayName != "vic" || joined.Role != controlpb.Role_SPEAKER {
		t.Fatalf("ParticipantJoined %+v", joined)
	}
	for _, p := range state.Participants {
		if p.Guest != (p.UserId == "guest:abcdef0123456789") {
			t.Errorf("RoomState participant %+v", p)
		}
	}

	// Participant JSON: guest:true for the guest, omitted for accounts.
	_, body := get(t, a.url+"/v1/rooms", "Bearer "+admin)
	if !strings.Contains(body, `"participants":[{"session_id":1,"user_id":"usr_1","name":"alice","muted":false,"deafened":false,"speaking":false},`+
		`{"session_id":2,"user_id":"guest:abcdef0123456789","name":"vic","muted":false,"deafened":false,"speaking":false,"guest":true}]`) {
		t.Fatalf("participants: %s", body)
	}

	// A guest-shaped subject without the guest claim is refused outright.
	if resp, _ := get(t, a.url+"/v1/rooms", "Bearer "+token(t, "x", "", time.Minute)); resp.StatusCode != 200 {
		t.Fatalf("account token: %d", resp.StatusCode)
	}
	forged, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, auth.Claims{Name: "vic",
		RegisteredClaims: jwt.RegisteredClaims{Subject: "guest:abc", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))}}).SignedString([]byte(secret))
	if resp, _ := get(t, a.url+"/v1/rooms", "Bearer "+forged); resp.StatusCode != 401 {
		t.Fatalf("guest sub without claim: %d", resp.StatusCode)
	}
}
