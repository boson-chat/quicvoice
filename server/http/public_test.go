package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	qvhttp "github.com/boson-chat/quicvoice/server/http"
	"github.com/boson-chat/quicvoice/server/internal/db/repositories/voiceroom"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/bridge"
	"github.com/boson-chat/quicvoice/server/internal/services/bridge/bridgetest"
	"github.com/boson-chat/quicvoice/server/internal/services/ergo"
	"github.com/boson-chat/quicvoice/server/internal/services/ergo/ergotest"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/services/settings"
	"github.com/boson-chat/quicvoice/server/internal/services/voicechannel"
	"github.com/boson-chat/quicvoice/server/metrics"
	"github.com/golang-jwt/jwt/v5"
)

const secret = "test-secret"

type recPeer struct {
	mu   sync.Mutex
	msgs []*controlpb.ControlMessage
}

func (*recPeer) SendDatagram([]byte) error { return nil }
func (p *recPeer) SendControl(m *controlpb.ControlMessage) error {
	p.mu.Lock()
	p.msgs = append(p.msgs, m)
	p.mu.Unlock()
	return nil
}

func (p *recPeer) kicked() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.msgs {
		if e := m.GetError(); e != nil && e.Code == controlpb.Error_KICKED {
			return e.Message
		}
	}
	return ""
}

// token mints an HS256 voice token: name = sub, role as given ("" = speaker).
func token(t *testing.T, sub, role string, ttl time.Duration, rooms ...string) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, auth.Claims{
		Name: sub, Role: role, Rooms: rooms,
		RegisteredClaims: jwt.RegisteredClaims{Subject: "usr_" + sub, ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl))},
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// netToken mints a token like the auth service does once it has network
// roles: roles and perms claims (nil = claim absent).
func netToken(t *testing.T, sub, role string, netRoles, perms []string) string {
	t.Helper()
	c := auth.Claims{
		Name: sub, Role: role,
		RegisteredClaims: jwt.RegisteredClaims{Subject: "usr_" + sub, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))},
	}
	if netRoles != nil {
		c.Roles = &netRoles
	}
	if perms != nil {
		c.Perms = &perms
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// policyServer fakes the auth service's internal settings endpoint.
type policyServer struct {
	mu     sync.Mutex
	policy string
	srv    *httptest.Server
}

func newPolicyServer(t *testing.T, policy string) *policyServer {
	p := &policyServer{policy: policy}
	p.srv = httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		if r.Header.Get("X-Internal-Token") != "itok" {
			nethttp.Error(w, "no", nethttp.StatusUnauthorized)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		_, _ = w.Write([]byte(`{"registration":"open","channel_creation":"registered","voice_channel_creation":"` + p.policy + `"}`))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *policyServer) set(v string) {
	p.mu.Lock()
	p.policy = v
	p.mu.Unlock()
}

type api struct {
	url    string
	rooms  room.RoomServiceImpl
	ergo   *ergotest.Fake
	policy *policyServer
}

type apiOpts struct {
	static  []room.Def
	path    string // "" = in-memory registry
	policy  string
	ergo    *ergotest.Fake
	policy2 *policyServer
	// noErgo: no Ergo API (an add-on install); bridge: evrd's managed API
	// for guests' membership instead.
	noErgo bool
	bridge *bridgetest.Fake
}

func newAPI(t *testing.T, o apiOpts) *api {
	t.Helper()
	ctx := context.Background()
	if o.policy == "" {
		o.policy = "admins"
	}
	ps := o.policy2
	if ps == nil {
		ps = newPolicyServer(t, o.policy)
	}
	fe := o.ergo
	if fe == nil {
		fe = ergotest.New(t, "etok")
	}
	repo, err := voiceroom.NewRoomRepository(o.path)
	if err != nil {
		t.Fatal(err)
	}
	ergoOpts := ergo.Options{URL: fe.URL(), Token: "etok", TTL: time.Nanosecond}
	if o.noErgo {
		ergoOpts = ergo.Options{}
	}
	var presence voicechannel.Presence
	if o.bridge != nil {
		presence = bridge.NewBridgeService(bridge.Options{URL: o.bridge.URL(), Token: o.bridge.Token, TTL: time.Nanosecond})
	}
	vc, err := voicechannel.NewVoiceChannelService(ctx, voicechannel.Options{
		Repo: repo,
		// TTLs of 1ns: every request sees the current fake state.
		Settings:   settings.NewSettingsService(settings.Options{URL: ps.srv.URL, Token: "itok", TTL: time.Nanosecond}),
		Ergo:       ergo.NewErgoService(ergoOpts),
		Presence:   presence,
		Static:     o.static,
		MaxPerUser: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	rs := room.NewRoomService(10, metrics.Noop(), room.WithDirectory(vc, true), room.WithJoinPolicy(vc))
	srv := httptest.NewServer(qvhttp.NewPublicRouter(auth.NewJWTAuthService(secret), rs, vc))
	t.Cleanup(srv.Close)
	return &api{url: srv.URL, rooms: rs, ergo: fe, policy: ps}
}

func do(t *testing.T, method, url, authz, body string) (*nethttp.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = bytes.NewBufferString(body)
	}
	req, _ := nethttp.NewRequest(method, url, rd)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := nethttp.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func get(t *testing.T, url, authz string) (*nethttp.Response, string) {
	return do(t, nethttp.MethodGet, url, authz, "")
}

type roomOut struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Kind   string  `json:"kind"`
	Parent *string `json:"parent"`
	Owner  *struct {
		UserID string `json:"user_id"`
		Name   string `json:"name"`
	} `json:"owner"`
	Position                 int        `json:"position"`
	CreatedAt                *time.Time `json:"created_at"`
	MinRole                  string     `json:"min_role"`
	SpeakRole                string     `json:"speak_role"`
	RequireChannelMembership bool       `json:"require_channel_membership"`
	AllowedRoles             []string   `json:"allowed_roles"`
	CanManage                bool       `json:"can_manage"`
	CanJoin                  bool       `json:"can_join"`
	CanSpeak                 bool       `json:"can_speak"`
	LockReason               string     `json:"lock_reason"`
	Participants             []struct {
		UserID string `json:"user_id"`
	} `json:"participants"`
}

type listOut struct {
	Permissions struct {
		Policy          string   `json:"policy"`
		CanCreate       bool     `json:"can_create"`
		CreateScope     string   `json:"create_scope"`
		FoundedChannels []string `json:"founded_channels"`
		CanManageAny    bool     `json:"can_manage_any"`
		BypassLocks     bool     `json:"bypass_locks"`
	} `json:"permissions"`
	Rooms []roomOut `json:"rooms"`
}

func (a *api) list(t *testing.T, tok string) listOut {
	t.Helper()
	resp, body := get(t, a.url+"/v1/rooms", "Bearer "+tok)
	if resp.StatusCode != nethttp.StatusOK {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}
	var out listOut
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (l listOut) room(id string) *roomOut {
	for i := range l.Rooms {
		if l.Rooms[i].ID == id {
			return &l.Rooms[i]
		}
	}
	return nil
}

func (a *api) create(t *testing.T, tok, body string, want int) roomOut {
	t.Helper()
	resp, b := do(t, nethttp.MethodPost, a.url+"/v1/rooms", "Bearer "+tok, body)
	if resp.StatusCode != want {
		t.Fatalf("POST %s: %d %s, want %d", body, resp.StatusCode, b, want)
	}
	var r roomOut
	if want == nethttp.StatusCreated {
		if err := json.Unmarshal([]byte(b), &r); err != nil {
			t.Fatal(err)
		}
		if resp.Header.Get("Location") != "/v1/rooms/"+r.ID {
			t.Fatalf("Location %q", resp.Header.Get("Location"))
		}
	} else if !strings.Contains(b, `"code"`) {
		t.Fatalf("error body without code: %s", b)
	}
	return r
}

func TestPublicRooms(t *testing.T) {
	ctx := context.Background()
	a := newAPI(t, apiOpts{static: []room.Def{{ID: "general", Name: "General"}, {ID: "gaming", Name: "Gaming"}}})
	_, _, _ = a.rooms.Join(ctx, "general", &auth.Identity{UserID: "usr_1", DisplayName: "alice", Role: controlpb.Role_SPEAKER}, &recPeer{})

	t.Run("unauthorized", func(t *testing.T) {
		for name, authz := range map[string]string{
			"missing": "",
			"scheme":  "Basic " + token(t, "u", "", time.Minute),
			"empty":   "Bearer ",
			"invalid": "Bearer not-a-jwt",
			"expired": "Bearer " + token(t, "u", "", -time.Minute),
		} {
			for _, m := range []string{"GET", "POST", "PATCH", "DELETE"} {
				url := a.url + "/v1/rooms"
				if m == "PATCH" || m == "DELETE" {
					url += "/general"
				}
				resp, body := do(t, m, url, authz, "")
				if resp.StatusCode != nethttp.StatusUnauthorized {
					t.Errorf("%s %s: status %d, want 401", name, m, resp.StatusCode)
				}
				if resp.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
					t.Errorf("%s: headers %v", name, resp.Header)
				}
				if !strings.Contains(body, `"error"`) {
					t.Errorf("%s: body %q", name, body)
				}
			}
		}
	})

	t.Run("ok", func(t *testing.T) {
		resp, body := get(t, a.url+"/v1/rooms", "Bearer "+token(t, "u", "", time.Minute))
		if resp.StatusCode != nethttp.StatusOK {
			t.Fatalf("status %d: %s", resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type %q", ct)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control %q", cc)
		}
		var l listOut
		_ = json.Unmarshal([]byte(body), &l)
		g := l.room("general")
		if len(l.Rooms) != 2 || l.Rooms[0].ID != "general" || l.Rooms[1].ID != "gaming" || g.Kind != "static" ||
			g.Parent != nil || g.Owner != nil || g.CanManage || !g.CanJoin || !g.CanSpeak || g.MinRole != "member" ||
			len(g.Participants) != 1 || g.Participants[0].UserID != "usr_1" || g.CreatedAt == nil {
			t.Errorf("body %s", body)
		}
		if l.Permissions.Policy != "admins" || l.Permissions.CanCreate || l.Permissions.CreateScope != "none" {
			t.Errorf("permissions %+v", l.Permissions)
		}
		// Exact shape of one room for the client contract.
		if !strings.Contains(body, `"participants":[{"session_id":1,"user_id":"usr_1","name":"alice","muted":false,"deafened":false,"speaking":false}]`) ||
			!strings.Contains(body, `"permissions":{"policy":"admins","can_create":false,"create_scope":"none","can_manage_any":false,"bypass_locks":false}`) ||
			!strings.Contains(body, `"require_channel_membership":false,"allowed_roles":[],"can_manage"`) {
			t.Errorf("shape: %s", body)
		}
	})

	t.Run("rooms claim filters", func(t *testing.T) {
		l := a.list(t, token(t, "u", "", time.Minute, "gaming"))
		if len(l.Rooms) != 1 || l.Rooms[0].ID != "gaming" {
			t.Fatalf("rooms = %+v", l.Rooms)
		}
	})

	t.Run("static rooms are read-only", func(t *testing.T) {
		adm := "Bearer " + token(t, "root", "admin", time.Minute)
		if resp, b := do(t, "DELETE", a.url+"/v1/rooms/general", adm, ""); resp.StatusCode != 403 || !strings.Contains(b, "static_room") {
			t.Fatalf("delete static: %d %s", resp.StatusCode, b)
		}
		if resp, _ := do(t, "PATCH", a.url+"/v1/rooms/general", adm, `{"name":"x"}`); resp.StatusCode != 403 {
			t.Fatalf("patch static: %d", resp.StatusCode)
		}
	})

	t.Run("no metrics on public listener", func(t *testing.T) {
		if resp, _ := get(t, a.url+"/metrics", ""); resp.StatusCode != nethttp.StatusNotFound {
			t.Fatalf("/metrics status %d, want 404", resp.StatusCode)
		}
		if resp, body := get(t, a.url+"/healthz", ""); resp.StatusCode != nethttp.StatusOK || body != "ok" {
			t.Fatalf("/healthz %d %q", resp.StatusCode, body)
		}
	})
}

func TestCreatePolicies(t *testing.T) {
	a := newAPI(t, apiOpts{policy: "admins"})
	alice := token(t, "alice", "", time.Minute)
	admin := token(t, "root", "moderator", time.Minute)
	a.ergo.Register("#boson", "alice")
	a.ergo.Register("#other", "bob")

	// admins: only moderator/admin.
	a.create(t, alice, `{"name":"Lounge"}`, 403)
	r := a.create(t, admin, `{"name":"Staff Room"}`, 201)
	if r.ID != "staff-room" || r.Kind != "dynamic" || r.Owner == nil || r.Owner.UserID != "usr_root" || !r.CanManage || r.CreatedAt == nil {
		t.Fatalf("admin room %+v", r)
	}
	if p := a.list(t, alice).Permissions; p.CanCreate || p.CreateScope != "none" {
		t.Fatalf("alice perms under admins %+v", p)
	}
	if p := a.list(t, admin).Permissions; !p.CanCreate || p.CreateScope != "any" {
		t.Fatalf("admin perms %+v", p)
	}

	// founders: admins anywhere; others only under channels they founded.
	a.policy.set("founders")
	if p := a.list(t, alice).Permissions; p.Policy != "founders" || !p.CanCreate || p.CreateScope != "founded_channels" ||
		len(p.FoundedChannels) != 1 || p.FoundedChannels[0] != "#boson" {
		t.Fatalf("alice perms under founders %+v", p)
	}
	if p := a.list(t, token(t, "carol", "", time.Minute)).Permissions; p.CanCreate || p.CreateScope != "founded_channels" || p.FoundedChannels == nil {
		t.Fatalf("carol perms %+v", p)
	}
	a.create(t, alice, `{"name":"Lounge"}`, 403)                   // no parent
	a.create(t, alice, `{"name":"Lounge","parent":"#other"}`, 403) // bob's channel
	a.create(t, alice, `{"name":"Lounge","parent":"#nope"}`, 403)  // unregistered
	r = a.create(t, alice, `{"name":"Game Night","parent":"#Boson"}`, 201)
	if r.ID != "boson-game-night" || r.Parent == nil || *r.Parent != "#boson" || !r.CanManage {
		t.Fatalf("founder room %+v", r)
	}
	a.create(t, alice, `{"name":"game night","parent":"#boson"}`, 409) // unique per parent, case-insensitive
	a.create(t, admin, `{"name":"Game Night"}`, 201)                   // same name, other parent: fine
	a.create(t, admin, `{"name":"Mods","parent":"#other"}`, 201)       // admins anywhere

	// Ergo down: founder check fails closed with 503.
	a.ergo.SetDown(true)
	resp, b := do(t, "POST", a.url+"/v1/rooms", "Bearer "+alice, `{"name":"X","parent":"#boson"}`)
	if resp.StatusCode != 503 || !strings.Contains(b, "unavailable") || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("ergo down: %d %s", resp.StatusCode, b)
	}
	a.ergo.SetDown(false)

	// registered: anyone, any parent or none; per-user limit (3).
	a.policy.set("registered")
	carol := token(t, "carol", "", time.Minute)
	a.create(t, carol, `{"name":"One"}`, 201)
	a.create(t, carol, `{"name":"Two","parent":"#other"}`, 201)
	a.create(t, carol, `{"name":"Three"}`, 201)
	if _, b := do(t, "POST", a.url+"/v1/rooms", "Bearer "+carol, `{"name":"Four"}`); !strings.Contains(b, `"code":"limit"`) {
		t.Fatalf("limit: %s", b)
	}

	// Validation.
	for _, body := range []string{
		`{"name":""}`, `{"name":"   "}`, `{"name":"` + strings.Repeat("x", 33) + `"}`, `{"name":"a\u0007b"}`,
		`{"name":"x","parent":"boson"}`, `{"name":"x","parent":"#a b"}`, `{"name":"x","min_role":"op"}`,
		`{"name":"x","min_role":"king"}`, `{"name":"x","require_channel_membership":true}`, `not json`,
	} {
		a.create(t, admin, body, 400)
	}
	a.create(t, admin, `{"name":"`+strings.Repeat("é", 32)+`"}`, 201) // 32 runes ok
	// Names without ASCII letters/digits get "room", then "room-2", ….
	if r := a.create(t, admin, `{"name":"🎮🎮"}`, 201); r.ID != "room-2" {
		t.Fatalf("emoji id %q", r.ID)
	}
	if r := a.create(t, admin, `{"name":"🎲"}`, 201); r.ID != "room-3" {
		t.Fatalf("second emoji id %q", r.ID)
	}
}

func TestManageAndDelete(t *testing.T) {
	ctx := context.Background()
	a := newAPI(t, apiOpts{policy: "registered"})
	a.ergo.Register("#boson", "fred")
	alice, bob, fred := token(t, "alice", "", time.Minute), token(t, "bob", "", time.Minute), token(t, "fred", "", time.Minute)
	admin := token(t, "root", "admin", time.Minute)

	r := a.create(t, alice, `{"name":"Hangout","parent":"#boson"}`, 201)
	url := a.url + "/v1/rooms/" + r.ID

	// can_manage per caller: owner, parent founder, admin; not bob.
	for tok, want := range map[string]bool{alice: true, fred: true, admin: true, bob: false} {
		if got := a.list(t, tok).room(r.ID).CanManage; got != want {
			t.Errorf("can_manage = %v, want %v", got, want)
		}
	}

	// Rename keeps the id; position moves it.
	if resp, b := do(t, "PATCH", url, "Bearer "+bob, `{"name":"Mine"}`); resp.StatusCode != 403 {
		t.Fatalf("bob patch: %d %s", resp.StatusCode, b)
	}
	resp, b := do(t, "PATCH", url, "Bearer "+fred, `{"name":"Chill Zone","position":0}`)
	if resp.StatusCode != 200 || !strings.Contains(b, `"id":"boson-hangout"`) || !strings.Contains(b, `"name":"Chill Zone"`) || !strings.Contains(b, `"position":0`) {
		t.Fatalf("fred patch: %d %s", resp.StatusCode, b)
	}
	for _, body := range []string{`{"name":""}`, `{"position":-1}`, `{"parent":"#x"}`, `{"min_role":"oper"}`} {
		if resp, b := do(t, "PATCH", url, "Bearer "+alice, body); resp.StatusCode != 400 && resp.StatusCode != 403 {
			t.Fatalf("patch %s: %d %s", body, resp.StatusCode, b)
		}
	}
	if resp, _ := do(t, "PATCH", a.url+"/v1/rooms/nope", "Bearer "+admin, `{"name":"x"}`); resp.StatusCode != 404 {
		t.Fatalf("patch missing: %d", resp.StatusCode)
	}
	a.create(t, bob, `{"name":"Other","parent":"#boson"}`, 201)
	if resp, _ := do(t, "PATCH", url, "Bearer "+alice, `{"name":"other"}`); resp.StatusCode != 409 {
		t.Fatalf("rename onto taken name: %d", resp.StatusCode)
	}

	// Delete kicks everyone in the room.
	p1, p2 := &recPeer{}, &recPeer{}
	m1, _, err := a.rooms.Join(ctx, r.ID, &auth.Identity{UserID: "u1", DisplayName: "u1", Role: controlpb.Role_SPEAKER}, p1)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = a.rooms.Join(ctx, r.ID, &auth.Identity{UserID: "u2", DisplayName: "u2", Role: controlpb.Role_SPEAKER}, p2)
	if resp, _ := do(t, "DELETE", url, "Bearer "+bob, ""); resp.StatusCode != 403 {
		t.Fatalf("bob delete: %d", resp.StatusCode)
	}
	if resp, b := do(t, "DELETE", url, "Bearer "+alice, ""); resp.StatusCode != 204 || b != "" {
		t.Fatalf("delete: %d %q", resp.StatusCode, b)
	}
	if !strings.Contains(p1.kicked(), "deleted") || p2.kicked() == "" || !m1.Closed() {
		t.Fatalf("not kicked: %q %q", p1.kicked(), p2.kicked())
	}
	if a.list(t, alice).room(r.ID) != nil || len(a.rooms.Participants(r.ID)) != 0 {
		t.Fatal("room still listed")
	}
	if _, _, err := a.rooms.Join(ctx, r.ID, &auth.Identity{UserID: "u1", Role: controlpb.Role_SPEAKER}, p1); err == nil {
		t.Fatal("joined a deleted room in strict mode")
	}
	if resp, _ := do(t, "DELETE", url, "Bearer "+alice, ""); resp.StatusCode != 404 {
		t.Fatalf("second delete: %d", resp.StatusCode)
	}
}

func TestRoleLocks(t *testing.T) {
	ctx := context.Background()
	a := newAPI(t, apiOpts{policy: "registered"})
	a.ergo.Register("#boson", "fred")
	a.ergo.Join("olive", "#boson", "o")
	a.ergo.Join("vic", "#boson", "v")
	a.ergo.Join("mia", "#boson", "")
	admin := token(t, "root", "admin", time.Minute)
	fred := token(t, "fred", "", time.Minute)

	r := a.create(t, fred, `{"name":"Stage","parent":"#boson","min_role":"voice","speak_role":"op"}`, 201)
	if r.MinRole != "voice" || r.SpeakRole != "op" {
		t.Fatalf("locks %+v", r)
	}
	type want struct{ join, speak bool }
	cases := map[string]want{
		"fred":   {true, true},   // founder (offline: from ChanServ registration)
		"olive":  {true, true},   // @
		"vic":    {true, false},  // + → listener
		"mia":    {false, false}, // in channel, no prefix
		"nobody": {false, false},
	}
	for user, w := range cases {
		got := a.list(t, token(t, user, "", time.Minute)).room(r.ID)
		if got.CanJoin != w.join || got.CanSpeak != w.speak {
			t.Errorf("%s: join=%v speak=%v, want %+v", user, got.CanJoin, got.CanSpeak, w)
		}
		// Enforced at Join too.
		id := &auth.Identity{UserID: "usr_" + user, DisplayName: user, Account: user, Role: controlpb.Role_SPEAKER}
		m, _, err := a.rooms.Join(ctx, r.ID, id, &recPeer{})
		if (err == nil) != w.join {
			t.Errorf("%s: Join err = %v, want join=%v", user, err, w.join)
		}
		if err == nil {
			if m.Identity.CanSpeak() != w.speak {
				t.Errorf("%s: member role %v", user, m.Identity.Role)
			}
			a.rooms.Leave(ctx, m, "test")
		}
	}
	if got := a.list(t, admin).room(r.ID); !got.CanJoin || !got.CanSpeak {
		t.Error("admin locked out")
	}

	// require_channel_membership: founder fred isn't on IRC → can't join.
	resp, b := do(t, "PATCH", a.url+"/v1/rooms/"+r.ID, "Bearer "+fred, `{"min_role":"member","speak_role":"member","require_channel_membership":true}`)
	if resp.StatusCode != 200 {
		t.Fatalf("patch: %d %s", resp.StatusCode, b)
	}
	for user, join := range map[string]bool{"fred": false, "mia": true, "nobody": false} {
		if got := a.list(t, token(t, user, "", time.Minute)).room(r.ID); got.CanJoin != join {
			t.Errorf("membership %s: %v", user, got.CanJoin)
		}
	}

	// Ergo down: locked rooms fail closed, unlocked ones still work.
	open := a.create(t, admin, `{"name":"Open","parent":"#boson"}`, 201)
	a.ergo.SetDown(true)
	l := a.list(t, token(t, "mia", "", time.Minute))
	if l.room(r.ID).CanJoin || !l.room(open.ID).CanJoin {
		t.Fatalf("ergo down: %+v", l.Rooms)
	}
	_, _, err := a.rooms.Join(ctx, r.ID, &auth.Identity{UserID: "usr_mia", Account: "mia", Role: controlpb.Role_SPEAKER}, &recPeer{})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("join with ergo down: %v", err)
	}
	a.ergo.SetDown(false)

	// Rooms without a parent: member or oper only.
	adminOnly := a.create(t, admin, `{"name":"Admins","min_role":"oper"}`, 201)
	if a.list(t, fred).room(adminOnly.ID).CanJoin || !a.list(t, admin).room(adminOnly.ID).CanJoin {
		t.Fatal("oper lock")
	}
	a.create(t, fred, `{"name":"Mine","min_role":"oper"}`, 403) // only admins lock to oper
}

func TestPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rooms.json")
	static := []room.Def{{ID: "general", Name: "General"}, {ID: "music", Name: "Music"}}
	fe := ergotest.New(t, "etok")
	ps := newPolicyServer(t, "registered")
	a := newAPI(t, apiOpts{static: static, path: path, ergo: fe, policy2: ps})
	alice := token(t, "alice", "", time.Minute)
	r := a.create(t, alice, `{"name":"Late Night","parent":"#boson","min_role":"voice"}`, 201)
	_, _ = do(t, "PATCH", a.url+"/v1/rooms/"+r.ID, "Bearer "+alice, `{"name":"Later Night"}`)

	// Restart with a changed VOICE_ROOMS: music gone, news added, general renamed.
	b := newAPI(t, apiOpts{static: []room.Def{{ID: "general", Name: "Lobby"}, {ID: "news", Name: "News"}}, path: path, ergo: fe, policy2: ps})
	l := b.list(t, alice)
	var ids []string
	for _, x := range l.Rooms {
		ids = append(ids, x.ID)
	}
	if strings.Join(ids, ",") != "general,news,"+r.ID {
		t.Fatalf("rooms after restart = %v", ids)
	}
	got := l.room(r.ID)
	if got.Name != "Later Night" || got.MinRole != "voice" || got.Owner.UserID != "usr_alice" || !got.CanManage || got.Kind != "dynamic" {
		t.Fatalf("restored %+v", got)
	}
	if l.room("general").Name != "Lobby" {
		t.Fatal("static rename not applied")
	}
}
