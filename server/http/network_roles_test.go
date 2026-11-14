package http_test

import (
	"context"
	"encoding/json"
	nethttp "net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
)

// TestNetworkRolesAPI: tokens with the auth service's roles/perms claims,
// allowed_roles in POST/PATCH/GET, and enforcement at Join.
func TestNetworkRolesAPI(t *testing.T) {
	ctx := context.Background()
	a := newAPI(t, apiOpts{policy: "registered", static: []room.Def{{ID: "general", Name: "General"}}})
	verifier := auth.NewJWTAuthService(secret)
	none := []string{}
	var (
		mgr       = netToken(t, "mgr", "", none, []string{auth.PermVoiceManageAny, "chat.kick"})
		creator   = netToken(t, "cara", "", []string{"Member"}, []string{auth.PermVoiceCreate})
		noCreate  = netToken(t, "nina", "", []string{"DJ"}, none)
		dj        = netToken(t, "dan", "", []string{"dj"}, none)
		bypass    = netToken(t, "bea", "", none, []string{auth.PermVoiceBypassLocks})
		oldUser   = token(t, "olu", "", time.Minute)
		oldAdmin  = token(t, "root", "admin", time.Minute)
		newLegacy = netToken(t, "rad", "admin", none, none) // role claim ignored once perms is present
	)

	// permissions block.
	for name, c := range map[string]struct {
		tok                          string
		canCreate, manageAny, bypass bool
		scope                        string
	}{
		"mgr":       {mgr, true, true, false, "any"},
		"creator":   {creator, true, false, false, "any"},
		"noCreate":  {noCreate, false, false, false, "none"},
		"bypass":    {bypass, false, false, true, "none"},
		"oldUser":   {oldUser, true, false, false, "any"},
		"oldAdmin":  {oldAdmin, true, true, true, "any"},
		"newLegacy": {newLegacy, false, false, false, "none"},
	} {
		p := a.list(t, c.tok).Permissions
		if p.CanCreate != c.canCreate || p.CanManageAny != c.manageAny || p.BypassLocks != c.bypass || p.CreateScope != c.scope {
			t.Errorf("%s: permissions %+v", name, p)
		}
	}

	a.create(t, noCreate, `{"name":"Nope"}`, 403)
	a.create(t, newLegacy, `{"name":"Nope"}`, 403)
	r := a.create(t, creator, `{"name":"Booth","allowed_roles":[" DJ ","Owner","dj"]}`, 201)
	if !slices.Equal(r.AllowedRoles, []string{"DJ", "Owner"}) {
		t.Fatalf("allowed_roles = %q", r.AllowedRoles)
	}
	if g := a.list(t, oldUser).room("general"); g.AllowedRoles == nil || len(g.AllowedRoles) != 0 {
		t.Fatalf("static allowed_roles = %#v", g.AllowedRoles)
	}

	many := make([]string, 21)
	for i := range many {
		many[i] = `"r` + string(rune('a'+i)) + `"`
	}
	for _, body := range []string{
		`{"name":"x","allowed_roles":[""]}`,
		`{"name":"x","allowed_roles":["` + strings.Repeat("x", 65) + `"]}`,
		`{"name":"x","allowed_roles":["a\u0001"]}`,
		`{"name":"x","allowed_roles":"DJ"}`,
		`{"name":"x","allowed_roles":[` + strings.Join(many, ",") + `]}`,
	} {
		resp, b := do(t, "POST", a.url+"/v1/rooms", "Bearer "+mgr, body)
		if resp.StatusCode != 400 || !strings.Contains(b, `"code":"invalid"`) {
			t.Errorf("POST %s: %d %s", body, resp.StatusCode, b)
		}
	}

	type want struct{ join, manage bool }
	check := func(t *testing.T, cases map[string]want, toks map[string]string) {
		t.Helper()
		for name, w := range cases {
			got := a.list(t, toks[name]).room(r.ID)
			if got.CanJoin != w.join || got.CanManage != w.manage {
				t.Errorf("%s: can_join=%v can_manage=%v, want %+v", name, got.CanJoin, got.CanManage, w)
			}
			// Same decision at Join (the QUIC path goes through RoomService.Join).
			id, err := verifier.Verify(ctx, toks[name])
			if err != nil {
				t.Fatal(err)
			}
			m, _, err := a.rooms.Join(ctx, r.ID, id, &recPeer{})
			if (err == nil) != w.join {
				t.Errorf("%s: Join err = %v, want join=%v", name, err, w.join)
			}
			if err == nil {
				a.rooms.Leave(ctx, m, "test")
			} else if !w.join && !strings.Contains(err.Error(), "needs one of these roles") {
				t.Errorf("%s: Join err text %q", name, err)
			}
		}
	}
	toks := map[string]string{"mgr": mgr, "creator": creator, "noCreate": noCreate, "dj": dj,
		"bypass": bypass, "oldUser": oldUser, "oldAdmin": oldAdmin, "newLegacy": newLegacy}
	check(t, map[string]want{
		"mgr":       {false, true}, // manage_any manages but doesn't bypass
		"creator":   {false, true}, // owner, but not a DJ
		"noCreate":  {true, false},
		"dj":        {true, false},
		"bypass":    {true, false},
		"oldUser":   {false, false}, // no roles claim
		"oldAdmin":  {true, true},
		"newLegacy": {false, false},
	}, toks)

	url := a.url + "/v1/rooms/" + r.ID
	patch := func(tok, body string, status int) roomOut {
		t.Helper()
		resp, b := do(t, "PATCH", url, "Bearer "+tok, body)
		if resp.StatusCode != status {
			t.Fatalf("PATCH %s: %d %s, want %d", body, resp.StatusCode, b, status)
		}
		var out roomOut
		_ = json.Unmarshal([]byte(b), &out)
		return out
	}
	patch(newLegacy, `{"allowed_roles":["Owner"]}`, 403)
	patch(dj, `{"allowed_roles":["Owner"]}`, 403)
	if got := patch(mgr, `{"allowed_roles":["Owner"]}`, 200); !slices.Equal(got.AllowedRoles, []string{"Owner"}) {
		t.Fatalf("replace: %q", got.AllowedRoles)
	}
	if a.list(t, dj).room(r.ID).CanJoin {
		t.Fatal("dj can still join after allowed_roles changed")
	}
	if got := patch(creator, `{"allowed_roles":null,"name":"Booth 2"}`, 200); !slices.Equal(got.AllowedRoles, []string{"Owner"}) {
		t.Fatalf("null should leave allowed_roles: %q", got.AllowedRoles)
	}
	patch(creator, `{"allowed_roles":["\u0007"]}`, 400)
	resp, b := do(t, "PATCH", url, "Bearer "+creator, `{"allowed_roles":[]}`)
	if resp.StatusCode != 200 || !strings.Contains(b, `"allowed_roles":[]`) {
		t.Fatalf("clear: %d %s", resp.StatusCode, b)
	}
	if !a.list(t, oldUser).room(r.ID).CanJoin {
		t.Fatal("cleared allowed_roles still locks")
	}
	// Static rooms stay read-only, also for manage_any.
	if resp, b := do(t, "PATCH", a.url+"/v1/rooms/general", "Bearer "+mgr, `{"allowed_roles":["DJ"]}`); resp.StatusCode != nethttp.StatusForbidden || !strings.Contains(b, "static_room") {
		t.Fatalf("static: %d %s", resp.StatusCode, b)
	}
	// manage_any can delete someone else's room.
	if resp, _ := do(t, "DELETE", url, "Bearer "+mgr, ""); resp.StatusCode != nethttp.StatusNoContent {
		t.Fatalf("delete by manage_any: %d", resp.StatusCode)
	}
}

// TestNetworkRolesPersist: allowed_roles survives a restart.
func TestNetworkRolesPersist(t *testing.T) {
	path := t.TempDir() + "/rooms.json"
	ps := newPolicyServer(t, "registered")
	a := newAPI(t, apiOpts{path: path, policy2: ps})
	alice := netToken(t, "alice", "", []string{"DJ"}, []string{auth.PermVoiceCreate})
	r := a.create(t, alice, `{"name":"Booth","allowed_roles":["DJ"]}`, 201)
	b := newAPI(t, apiOpts{path: path, policy2: ps})
	got := b.list(t, alice).room(r.ID)
	if got == nil || !slices.Equal(got.AllowedRoles, []string{"DJ"}) || !got.CanJoin {
		t.Fatalf("after restart: %+v", got)
	}
	if b.list(t, token(t, "bob", "", time.Minute)).room(r.ID).CanJoin {
		t.Fatal("old token joined an allowed_roles room after restart")
	}
}
