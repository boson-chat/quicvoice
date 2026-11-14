package transport_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/client"
	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/boson-chat/quicvoice/server/config"
	qvhttp "github.com/boson-chat/quicvoice/server/http"
	"github.com/boson-chat/quicvoice/server/internal/certs"
	"github.com/boson-chat/quicvoice/server/internal/configfile"
	"github.com/boson-chat/quicvoice/server/internal/db/repositories/voiceroom"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/liveconfig"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/services/voicechannel"
	"github.com/boson-chat/quicvoice/server/internal/transport"
	"github.com/boson-chat/quicvoice/server/metrics"
)

// writeAtomic replaces path the way the admin panel does: tmp file + rename.
func writeAtomic(t *testing.T, path, content string) {
	t.Helper()
	tmp := filepath.Join(filepath.Dir(path), ".boson.yaml.tmp")
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

type reloadServer struct {
	addr, publicURL, internalURL string
}

// startWithConfigFile wires quicvoiced the way `serve` does with
// BOSON_CONFIG=path: effective config from env + file, the live applier and
// the watcher (fast debounce/poll), the internal and public routers.
func startWithConfigFile(t *testing.T, path string) reloadServer {
	t.Helper()
	base, err := config.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	base.QUICConfig.Addr = freeUDPAddr(t)
	base.RoomConfig.Rooms = "general:General"
	file, err := config.ReadVoiceFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := file.Effective(base)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := metrics.Noop()
	defs, err := liveconfig.StaticDefs(base, file)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := voiceroom.NewRoomRepository("")
	vc, err := voicechannel.NewVoiceChannelService(ctx, voicechannel.Options{Repo: repo, Metrics: m, Static: defs,
		MaxPerUser: cfg.ChannelConfig.MaxPerUser, MaxRooms: cfg.ChannelConfig.MaxRooms})
	if err != nil {
		t.Fatal(err)
	}
	rooms := room.NewRoomService(cfg.RoomConfig.MaxParticipants, m, room.WithDirectory(vc, cfg.RoomConfig.Strict), room.WithJoinPolicy(vc))
	applier := liveconfig.New(liveconfig.Options{Base: base, Running: cfg, RunningRooms: defs, Rooms: rooms, Channels: vc})
	w := configfile.New(path, applier.Load, configfile.WithDebounce(20*time.Millisecond), configfile.WithPollInterval(200*time.Millisecond))
	_ = w.Reload(ctx)
	go w.Run(ctx)

	a := auth.NewInsecureAuthService()
	internal := httptest.NewServer(qvhttp.NewRouter("test", http.NotFoundHandler(), w))
	t.Cleanup(internal.Close)
	public := httptest.NewServer(qvhttp.NewPublicRouter(a, rooms, vc))
	t.Cleanup(public.Close)
	cert, err := certs.SelfSigned("localhost")
	if err != nil {
		t.Fatal(err)
	}
	srv := transport.NewServer(cfg, &tls.Config{Certificates: []tls.Certificate{cert}}, a, rooms, m)
	go func() { _ = srv.Serve(ctx) }()
	time.Sleep(100 * time.Millisecond)
	return reloadServer{addr: cfg.QUICConfig.Addr, publicURL: public.URL, internalURL: internal.URL}
}

func configStatus(t *testing.T, base string) configfile.Status {
	t.Helper()
	resp, err := http.Get(base + "/internal/config/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st configfile.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func roomIDsAndNames(t *testing.T, publicURL string) string {
	t.Helper()
	code, body := api(t, "GET", publicURL+"/v1/rooms", "alice", "")
	if code != 200 {
		t.Fatalf("GET /v1/rooms: %d %s", code, body)
	}
	var l struct {
		Rooms []struct{ ID, Name string } `json:"rooms"`
	}
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, r := range l.Rooms {
		parts = append(parts, r.ID+":"+r.Name)
	}
	return strings.Join(parts, ",")
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestConfigFileReloadOverQUIC: static rooms from the config file beat
// VOICE_ROOMS; an atomic rewrite adds, renames and removes rooms live (the
// people in the removed room are KICKED), toggles strict mode, and an
// invalid file keeps the last good config with the error's position on
// /internal/config/status; a listener change is restart_required.
func TestConfigFileReloadOverQUIC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boson.yaml")
	writeAtomic(t, path, `version: 1
network: {name: Test}
voice:
  rooms:
    - {id: lobby, name: Lobby}
    - {id: music, name: Music}
  rooms_strict: true
`)
	ts := startWithConfigFile(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if got := roomIDsAndNames(t, ts.publicURL); got != "lobby:Lobby,music:Music" {
		t.Fatalf("rooms at start = %q (the file beats VOICE_ROOMS=general)", got)
	}
	st := configStatus(t, ts.internalURL)
	if !st.OK || st.LoadedAt == nil || st.FileMtime == nil || len(st.RestartRequired) != 0 || st.Path != path {
		t.Fatalf("status at start: %+v", st)
	}

	kicked := make(chan string, 4)
	dial := func(name string) *client.Client {
		c, err := client.Dial(ctx, client.Config{Addr: ts.addr, Token: name, InsecureSkipVerify: true,
			OnControl: func(m *controlpb.ControlMessage) {
				if e := m.GetError(); e != nil && e.Code == controlpb.Error_KICKED {
					kicked <- name + ": " + e.Message
				}
			}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	alice, bob := dial("alice"), dial("bob")
	if _, err := alice.Join(ctx, "music"); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Join(ctx, "lobby"); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Join(ctx, "adhoc"); err == nil {
		t.Fatal("strict: ad-hoc room joined")
	}

	// music removed, lobby renamed, stage added, strict off.
	writeAtomic(t, path, `version: 1
network: {name: Test}
voice:
  rooms:
    - {id: stage, name: Stage}
    - {id: lobby, name: The Lobby}
  rooms_strict: false
`)
	select {
	case k := <-kicked:
		if !strings.HasPrefix(k, "alice: ") || !strings.Contains(k, `"Music" was removed from the server config`) {
			t.Fatalf("kick = %q", k)
		}
	case <-ctx.Done():
		t.Fatal("alice was not kicked from the removed room")
	}
	if got := roomIDsAndNames(t, ts.publicURL); got != "stage:Stage,lobby:The Lobby" {
		t.Fatalf("rooms after reload = %q", got)
	}
	select {
	case k := <-kicked:
		t.Fatalf("bob kicked from a renamed room: %q", k)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := alice.Join(ctx, "adhoc"); err != nil {
		t.Fatalf("strict off: ad-hoc join: %v", err)
	}

	// Broken YAML: last good config stays, the status says where.
	writeAtomic(t, path, "version: 1\nvoice:\n  rooms: [{id: Bad Id}]\n")
	eventually(t, "status ok=false", func() bool { return !configStatus(t, ts.internalURL).OK })
	st = configStatus(t, ts.internalURL)
	if st.ErrorLine != 3 || !strings.Contains(st.Error, "invalid room id") {
		t.Fatalf("invalid status: %+v", st)
	}
	if got := roomIDsAndNames(t, ts.publicURL); !strings.HasPrefix(got, "stage:Stage,lobby:The Lobby") {
		t.Fatalf("rooms after an invalid file = %q, want the last good ones", got)
	}

	// A listener address can't change live.
	writeAtomic(t, path, "version: 1\nvoice:\n  rooms: [stage]\n  quic_addr: \":7701\"\n")
	eventually(t, "restart_required", func() bool {
		st := configStatus(t, ts.internalURL)
		return st.OK && len(st.RestartRequired) == 1 && st.RestartRequired[0] == config.KeyQUICAddr
	})

	// File removed: back to env (VOICE_ROOMS=general), nothing to restart.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	eventually(t, "env rooms", func() bool { return strings.HasPrefix(roomIDsAndNames(t, ts.publicURL), "general:General") })
	if st := configStatus(t, ts.internalURL); !st.OK || st.FileMtime != nil || len(st.RestartRequired) != 0 {
		t.Fatalf("status without a file: %+v", st)
	}
}
