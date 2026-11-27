package config

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/server/internal/configfile"
)

func TestParseVoiceFile(t *testing.T) {
	f, err := ParseVoiceFile([]byte(`version: 1
network: {name: Boson}
auth: {registration: open}
voice:
  rooms:
    - {id: general, name: General}
    - music
  rooms_strict: false
  max_rooms_per_user: 3
  max_rooms: 40
  max_participants: 25
  settings_ttl: 1m
  ergo_cache_ttl: 5s
  public_http_addr: ""
  unknown_key: whatever
`))
	if err != nil {
		t.Fatal(err)
	}
	rooms, ok := f.StaticRooms()
	if !ok || !slices.Equal(rooms, []RoomEntry{{ID: "general", Name: "General"}, {ID: "music", Name: "music"}}) {
		t.Fatalf("rooms = %v %v", rooms, ok)
	}
	var base Config
	base.RoomConfig.Strict = true
	base.QUICConfig.Addr = ":7700"
	base.HTTPConfig.PublicAddr = ":8090"
	c := f.Effective(base)
	if c.RoomConfig.Strict || c.ChannelConfig.MaxPerUser != 3 || c.ChannelConfig.MaxRooms != 40 ||
		c.RoomConfig.MaxParticipants != 25 || c.ChannelConfig.SettingsTTL != time.Minute ||
		c.ChannelConfig.ErgoCacheTTL != 5*time.Second || c.QUICConfig.Addr != ":7700" || c.HTTPConfig.PublicAddr != "" {
		t.Fatalf("effective = %+v", c)
	}
	if got := RestartRequired(base, c); !slices.Equal(got, []string{KeyPublicHTTPAddr}) {
		t.Fatalf("restart_required = %v", got)
	}
}

func TestParseVoiceFileEmpty(t *testing.T) {
	for _, in := range []string{"", "   \n", "version: 1\n", "voice:\n", "version: 1\nvoice: ~\nirc: {plaintext: true}\n", "voice:\n  rooms_strict:\n"} {
		f, err := ParseVoiceFile([]byte(in))
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if _, ok := f.StaticRooms(); ok || f.RoomsStrict != nil {
			t.Fatalf("%q: keys %+v", in, f)
		}
	}
	f, err := ParseVoiceFile([]byte("voice:\n  rooms: []\n"))
	if rooms, ok := f.StaticRooms(); err != nil || !ok || len(rooms) != 0 {
		t.Fatalf("rooms: [] = %v %v %v", rooms, ok, err)
	}
}

func TestParseVoiceFileErrors(t *testing.T) {
	for _, c := range []struct {
		in        string
		line, col int
		msg       string
	}{
		{"voice:\n  rooms: [{id: general}, {id: General}]\n", 2, 31, "invalid room id"},
		{"voice:\n  rooms:\n    - general\n    - general\n", 4, 7, "duplicate room id"},
		{"voice:\n  rooms_strict: maybe\n", 2, 17, "rooms_strict"},
		{"voice:\n  max_participants: 0\n", 2, 21, "must be 1..10000"},
		{"voice:\n  settings_ttl: 30\n", 2, 17, "not a positive duration"},
		{"voice:\n  quic_addr: nope\n", 2, 14, "listen address"},
		{"voice: [1, 2]\n", 1, 8, "must be a mapping"},
		{"version: 2\nvoice: {}\n", 1, 10, "unsupported config file version"},
		{"voice:\n  rooms: [\n", 0, 0, "yaml"},
		{"voice:\n\trooms: []\n", 2, 0, "yaml"},
		{"voice:\n  max_kbps: 4\n", 2, 13, "must be 6..510"},
		{"voice:\n  max_kbps: lots\n", 2, 13, "whole number"},
		{"voice:\n  rooms: [{id: a, max_kbps: 600}]\n", 2, 29, "max_kbps: must be 6..510"},
	} {
		_, err := ParseVoiceFile([]byte(c.in))
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Fatalf("%q: err = %v, want %q", c.in, err, c.msg)
		}
		if c.line == 0 {
			continue
		}
		line, col := configfile.Position(err)
		if line != c.line || (c.col != 0 && col != c.col) {
			t.Fatalf("%q: at %d:%d, want %d:%d (%v)", c.in, line, col, c.line, c.col, err)
		}
	}
}

func TestExportVoice(t *testing.T) {
	var cfg Config
	cfg.RoomConfig.Strict = true
	cfg.ChannelConfig.MaxPerUser = 10
	cfg.ChannelConfig.MaxRooms = 77
	env := map[string]string{"VOICE_ROOMS_MAX": "77"}
	out, err := ExportVoice(cfg, VoiceFile{}, []RoomEntry{{ID: "general", Name: "General"}}, func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "voice:\n  rooms:\n    - id: general\n      name: General\n  rooms_strict: true\n  max_rooms_per_user: 10\n  max_rooms: 77\n"
	if string(out) != want {
		t.Fatalf("export:\n%s\nwant:\n%s", out, want)
	}
	// What it prints parses back to the same values.
	f, err := ParseVoiceFile(out)
	if err != nil || *f.MaxRooms != 77 || !*f.RoomsStrict {
		t.Fatalf("round trip: %+v %v", f, err)
	}
}

func TestReadVoiceFile(t *testing.T) {
	if f, err := ReadVoiceFile(t.TempDir() + "/missing.yaml"); err != nil || f.Rooms != nil {
		t.Fatalf("missing file: %+v %v", f, err)
	}
	p := t.TempDir() + "/boson.yaml"
	_ = os.WriteFile(p, []byte("voice:\n  max_rooms: x\n"), 0o644)
	if _, err := ReadVoiceFile(p); err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("invalid file: %v", err)
	}
}

func TestBitrateCaps(t *testing.T) {
	f, err := ParseVoiceFile([]byte(`voice:
  max_kbps: 64
  rooms:
    - {id: general, name: General}
    - {id: music, name: Music, max_kbps: 128}
    - {id: lobby, max_kbps: 0}
`))
	if err != nil {
		t.Fatal(err)
	}
	c := f.Caps()
	for room, want := range map[string]int{"general": 64000, "music": 128000, "lobby": 64000, "adhoc": 64000} {
		if got := c.Of(room); got != want {
			t.Errorf("Of(%s) = %d, want %d", room, got, want)
		}
	}
	if !c.Equal(f.Caps()) || c.Equal(BitrateCaps{Default: 64000}) || c.Equal(BitrateCaps{Default: 64000, Rooms: map[string]int{"music": 96000}}) {
		t.Fatal("Equal")
	}
	// No keys: no cap anywhere.
	none, _ := ParseVoiceFile([]byte("voice:\n  rooms: [general]\n"))
	if c := none.Caps(); c.Of("general") != 0 || !c.Equal(BitrateCaps{}) {
		t.Fatalf("no caps = %+v", c)
	}
	// Export keeps the caps.
	var ec Config
	ec.ChannelConfig.MaxPerUser = 3
	out, err := ExportVoice(ec, f, *f.Rooms, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseVoiceFile(out)
	if err != nil || !back.Caps().Equal(c) {
		t.Fatalf("round trip:\n%s\n%+v %v", out, back.Caps(), err)
	}
}

func TestWebOrigins(t *testing.T) {
	f, err := ParseVoiceFile([]byte("voice:\n  web_origins: [https://chat.example.com/, \"http://localhost:5173\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	var base Config
	c := f.Effective(base)
	if c.QUICConfig.WebTransportOrigins != "https://chat.example.com,http://localhost:5173" || c.CORSOrigins() != c.QUICConfig.WebTransportOrigins {
		t.Fatalf("origins %q / %q", c.QUICConfig.WebTransportOrigins, c.CORSOrigins())
	}
	if got := RestartRequired(base, c); !slices.Equal(got, []string{KeyWebOrigins}) {
		t.Fatalf("restart_required = %v", got)
	}
	// An empty list = any page.
	f, err = ParseVoiceFile([]byte("voice:\n  web_origins: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c := f.Effective(base); c.QUICConfig.WebTransportOrigins != "" || c.CORSOrigins() != "*" {
		t.Fatalf("empty list: %q / %q", c.QUICConfig.WebTransportOrigins, c.CORSOrigins())
	}
	for _, bad := range []string{"web_origins: https://x.test", "web_origins: [chat.example.com]", "web_origins: [\"https://x.test/path\"]"} {
		if _, err := ParseVoiceFile([]byte("voice:\n  " + bad + "\n")); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	// directory (the auth service's web origins), mixable with origins.
	f, err = ParseVoiceFile([]byte("voice:\n  web_origins: [Directory, https://chat.example.com]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c := f.Effective(base); c.QUICConfig.WebTransportOrigins != "directory,https://chat.example.com" || c.CORSOrigins() != c.QUICConfig.WebTransportOrigins {
		t.Fatalf("directory: %q / %q", c.QUICConfig.WebTransportOrigins, c.CORSOrigins())
	}
	// Env: CORS falls back to WEBTRANSPORT_ORIGINS.
	base.QUICConfig.WebTransportOrigins = "https://a.test"
	if base.CORSOrigins() != "https://a.test" {
		t.Fatal(base.CORSOrigins())
	}
	base.HTTPConfig.CORSAllowedOrigins = "*"
	if base.CORSOrigins() != "*" {
		t.Fatal(base.CORSOrigins())
	}
}
