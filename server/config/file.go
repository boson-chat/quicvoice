package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/boson-chat/quicvoice/server/internal/configfile"
	"gopkg.in/yaml.v3"
)

// The stack config file (BOSON_CONFIG, e.g. /config/boson.yaml) holds the
// non-secret settings of every boson service; quicvoiced reads only its
// `voice:` section and ignores everything else. A key in the file beats the
// env var, and the env var beats the built-in default; an absent key means
// "use env / default", so removing a key resets it.

// RoomEntry is one static room in voice.rooms.
type RoomEntry struct {
	ID   string
	Name string
	// MaxKbps caps the voice bitrate in this room (kbit/s); 0 = the
	// section's max_kbps.
	MaxKbps int
}

// VoiceFile is the `voice:` section. nil = key absent.
type VoiceFile struct {
	// Applied live.
	Rooms           *[]RoomEntry   // VOICE_ROOMS
	RoomsStrict     *bool          // VOICE_ROOMS_STRICT
	MaxRoomsPerUser *int           // VOICE_ROOMS_MAX_PER_USER
	MaxRooms        *int           // VOICE_ROOMS_MAX
	MaxParticipants *int           // ROOM_MAX_PARTICIPANTS
	SettingsTTL     *time.Duration // SETTINGS_TTL
	ErgoCacheTTL    *time.Duration // ERGO_CACHE_TTL
	// MaxKbps caps the voice bitrate in every room without its own
	// max_kbps (kbit/s). Absent = no cap.
	MaxKbps *int

	// Take effect after a restart.
	QUICAddr           *string // QUIC_ADDR
	HTTPAddr           *string // HTTP_ADDR
	PublicHTTPAddr     *string // PUBLIC_HTTP_ADDR
	MaxDatagramsPerSec *int    // MAX_DATAGRAMS_PER_SEC
	MaxDatagramPayload *int    // MAX_DATAGRAM_PAYLOAD
	// Web pages (origins) that may use voice: WebTransport sessions and
	// the public API's CORS. Sets WEBTRANSPORT_ORIGINS and
	// CORS_ALLOWED_ORIGINS; an empty list = any page.
	WebOrigins *[]string // WEBTRANSPORT_ORIGINS, CORS_ALLOWED_ORIGINS
}

// Voice section keys.
const (
	KeyRooms              = "voice.rooms"
	KeyRoomsStrict        = "voice.rooms_strict"
	KeyMaxRoomsPerUser    = "voice.max_rooms_per_user"
	KeyMaxRooms           = "voice.max_rooms"
	KeyMaxParticipants    = "voice.max_participants"
	KeySettingsTTL        = "voice.settings_ttl"
	KeyErgoCacheTTL       = "voice.ergo_cache_ttl"
	KeyMaxKbps            = "voice.max_kbps"
	KeyQUICAddr           = "voice.quic_addr"
	KeyHTTPAddr           = "voice.http_addr"
	KeyPublicHTTPAddr     = "voice.public_http_addr"
	KeyMaxDatagramsPerSec = "voice.max_datagrams_per_sec"
	KeyMaxDatagramPayload = "voice.max_datagram_payload"
	KeyWebOrigins         = "voice.web_origins"
)

var (
	roomIDPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	maxRoomName   = 64
)

// Voice bitrate caps (max_kbps): Opus's useful range.
const (
	MinKbps = 6
	MaxKbps = 510
)

// ParseVoiceFile parses a whole config file and returns its voice section.
// Empty or nil data (no file), a missing or null section = no keys.
// Errors carry the line and column (configfile.PosError or yaml's own).
func ParseVoiceFile(data []byte) (VoiceFile, error) {
	var f VoiceFile
	if len(bytes.TrimSpace(data)) == 0 {
		return f, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return f, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return f, nil
	}
	root := doc.Content[0]
	if isNull(root) {
		return f, nil
	}
	if root.Kind != yaml.MappingNode {
		return f, configfile.Errorf(root.Line, root.Column, "the config file must be a mapping (version: 1, network:, voice:, ...)")
	}
	if v := lookup(root, "version"); v != nil {
		var n int
		if err := v.Decode(&n); err != nil || n != 1 {
			return f, configfile.Errorf(v.Line, v.Column, "version: unsupported config file version %q (want 1)", v.Value)
		}
	}
	sec := lookup(root, "voice")
	if sec == nil || isNull(sec) {
		return f, nil
	}
	if sec.Kind != yaml.MappingNode {
		return f, configfile.Errorf(sec.Line, sec.Column, "voice: must be a mapping")
	}
	for i := 0; i+1 < len(sec.Content); i += 2 {
		k, v := sec.Content[i], sec.Content[i+1]
		if isNull(v) {
			continue // `key:` with no value = absent
		}
		var err error
		switch k.Value {
		case "rooms":
			var rooms []RoomEntry
			rooms, err = parseRooms(v)
			f.Rooms = &rooms
		case "rooms_strict":
			f.RoomsStrict, err = decode[bool](k.Value, v)
		case "max_rooms_per_user":
			f.MaxRoomsPerUser, err = positive(k.Value, v, 1_000_000)
		case "max_rooms":
			f.MaxRooms, err = positive(k.Value, v, 1_000_000)
		case "max_participants":
			f.MaxParticipants, err = positive(k.Value, v, 10_000)
		case "settings_ttl":
			f.SettingsTTL, err = duration(k.Value, v)
		case "ergo_cache_ttl":
			f.ErgoCacheTTL, err = duration(k.Value, v)
		case "max_kbps":
			f.MaxKbps, err = kbps("voice.max_kbps", v)
		case "quic_addr":
			f.QUICAddr, err = addr(k.Value, v, false)
		case "http_addr":
			f.HTTPAddr, err = addr(k.Value, v, false)
		case "public_http_addr":
			f.PublicHTTPAddr, err = addr(k.Value, v, true)
		case "max_datagrams_per_sec":
			f.MaxDatagramsPerSec, err = positive(k.Value, v, 100_000)
		case "max_datagram_payload":
			f.MaxDatagramPayload, err = positive(k.Value, v, 65_000)
		case "web_origins":
			f.WebOrigins, err = origins(v)
		}
		if err != nil {
			return VoiceFile{}, err
		}
	}
	return f, nil
}

// origins: a list of page origins like https://chat.example.com (or "*").
func origins(v *yaml.Node) (*[]string, error) {
	if v.Kind != yaml.SequenceNode {
		return nil, configfile.Errorf(v.Line, v.Column, "voice.web_origins: want a list like [https://chat.example.com]")
	}
	out := []string{}
	for _, it := range v.Content {
		o := strings.TrimSuffix(strings.TrimSpace(it.Value), "/")
		if it.Kind != yaml.ScalarNode || (o != "*" && !originPattern.MatchString(o)) {
			return nil, configfile.Errorf(it.Line, it.Column, "voice.web_origins: %q is not an origin like https://chat.example.com", it.Value)
		}
		out = append(out, o)
	}
	return &out, nil
}

var originPattern = regexp.MustCompile(`^https?://[A-Za-z0-9.\-\[\]:]+$`)

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

// lookup returns the value of key in a mapping node (nil = absent).
func lookup(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func decode[T any](key string, v *yaml.Node) (*T, error) {
	var x T
	if err := v.Decode(&x); err != nil {
		return nil, configfile.Errorf(v.Line, v.Column, "voice.%s: %s", key, typeMsg(err))
	}
	return &x, nil
}

// typeMsg strips yaml's "yaml: unmarshal errors:\n  line N: " prefix (the
// position is reported separately).
func typeMsg(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": cannot"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

func positive(key string, v *yaml.Node, maxV int) (*int, error) {
	n, err := decode[int](key, v)
	if err != nil {
		return nil, err
	}
	if *n < 1 || *n > maxV {
		return nil, configfile.Errorf(v.Line, v.Column, "voice.%s: must be 1..%d, got %d", key, maxV, *n)
	}
	return n, nil
}

// kbps: a bitrate cap in kbit/s, MinKbps..MaxKbps; 0 = no cap.
func kbps(key string, v *yaml.Node) (*int, error) {
	var n int
	if v.Kind != yaml.ScalarNode || v.Decode(&n) != nil {
		return nil, configfile.Errorf(v.Line, v.Column, "%s: want a whole number of kbit/s like 64", key)
	}
	if n != 0 && (n < MinKbps || n > MaxKbps) {
		return nil, configfile.Errorf(v.Line, v.Column, "%s: must be %d..%d kbit/s (0 = no cap), got %d", key, MinKbps, MaxKbps, n)
	}
	return &n, nil
}

func duration(key string, v *yaml.Node) (*time.Duration, error) {
	var s string
	if err := v.Decode(&s); err != nil {
		return nil, configfile.Errorf(v.Line, v.Column, "voice.%s: want a duration like 30s", key)
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return nil, configfile.Errorf(v.Line, v.Column, "voice.%s: %q is not a positive duration like 30s", key, s)
	}
	return &d, nil
}

func addr(key string, v *yaml.Node, allowEmpty bool) (*string, error) {
	s, err := decode[string](key, v)
	if err != nil {
		return nil, err
	}
	*s = strings.TrimSpace(*s)
	if (*s == "" && !allowEmpty) || (*s != "" && !strings.Contains(*s, ":")) {
		return nil, configfile.Errorf(v.Line, v.Column, "voice.%s: want a listen address like :7700", key)
	}
	return s, nil
}

// parseRooms: a list of {id, name} (name optional) or bare ids, ids
// lowercase [a-z0-9-]{1,32}, unique. An empty list = no static rooms.
func parseRooms(v *yaml.Node) ([]RoomEntry, error) {
	if v.Kind != yaml.SequenceNode {
		return nil, configfile.Errorf(v.Line, v.Column, "voice.rooms: want a list like [{id: general, name: General}]")
	}
	rooms := []RoomEntry{}
	seen := map[string]bool{}
	for _, it := range v.Content {
		var e RoomEntry
		idNode := it
		switch it.Kind {
		case yaml.ScalarNode:
			e.ID = strings.TrimSpace(it.Value)
		case yaml.MappingNode:
			idNode = lookup(it, "id")
			if idNode == nil {
				return nil, configfile.Errorf(it.Line, it.Column, "voice.rooms: entry has no id")
			}
			e.ID = strings.TrimSpace(idNode.Value)
			if n := lookup(it, "name"); n != nil && !isNull(n) {
				if n.Kind != yaml.ScalarNode {
					return nil, configfile.Errorf(n.Line, n.Column, "voice.rooms: name must be a string")
				}
				e.Name = strings.TrimSpace(n.Value)
			}
			if n := lookup(it, "max_kbps"); n != nil && !isNull(n) {
				k, err := kbps("voice.rooms: max_kbps", n)
				if err != nil {
					return nil, err
				}
				e.MaxKbps = *k
			}
		default:
			return nil, configfile.Errorf(it.Line, it.Column, "voice.rooms: entry must be {id: ..., name: ...}")
		}
		if !roomIDPattern.MatchString(e.ID) {
			return nil, configfile.Errorf(idNode.Line, idNode.Column, "voice.rooms: invalid room id %q (want lowercase [a-z0-9-]{1,32})", e.ID)
		}
		if seen[e.ID] {
			return nil, configfile.Errorf(idNode.Line, idNode.Column, "voice.rooms: duplicate room id %q", e.ID)
		}
		seen[e.ID] = true
		if e.Name == "" {
			e.Name = e.ID
		}
		if len(e.Name) > maxRoomName {
			return nil, configfile.Errorf(it.Line, it.Column, "voice.rooms: name for %q is longer than %d bytes", e.ID, maxRoomName)
		}
		rooms = append(rooms, e)
	}
	return rooms, nil
}

// StaticRooms returns the file's rooms (an empty list is allowed, unlike
// VOICE_ROOMS), or ok=false when the file has no rooms key (use VOICE_ROOMS).
func (f VoiceFile) StaticRooms() (rooms []RoomEntry, ok bool) {
	if f.Rooms == nil {
		return nil, false
	}
	return slices.Clone(*f.Rooms), true
}

// BitrateCaps are the voice bitrate caps in bit/s: def for every room,
// rooms for the static rooms with their own max_kbps. 0 = no cap.
type BitrateCaps struct {
	Default int
	Rooms   map[string]int
}

// Of is room's cap in bit/s (0 = none).
func (c BitrateCaps) Of(room string) int {
	if b, ok := c.Rooms[room]; ok {
		return b
	}
	return c.Default
}

// Equal reports whether c and o cap every room the same.
func (c BitrateCaps) Equal(o BitrateCaps) bool {
	if c.Default != o.Default || len(c.Rooms) != len(o.Rooms) {
		return false
	}
	for k, v := range c.Rooms {
		if w, ok := o.Rooms[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// Caps is the file's voice bitrate caps (max_kbps and the rooms' own).
func (f VoiceFile) Caps() BitrateCaps {
	var c BitrateCaps
	if f.MaxKbps != nil {
		c.Default = *f.MaxKbps * 1000
	}
	if f.Rooms != nil {
		for _, r := range *f.Rooms {
			if r.MaxKbps > 0 {
				if c.Rooms == nil {
					c.Rooms = map[string]int{}
				}
				c.Rooms[r.ID] = r.MaxKbps * 1000
			}
		}
	}
	return c
}

// Effective is base (env + defaults) with the file's keys on top, except
// rooms: see StaticRooms.
func (f VoiceFile) Effective(base Config) Config {
	c := base
	set(&c.RoomConfig.Strict, f.RoomsStrict)
	set(&c.ChannelConfig.MaxPerUser, f.MaxRoomsPerUser)
	set(&c.ChannelConfig.MaxRooms, f.MaxRooms)
	set(&c.RoomConfig.MaxParticipants, f.MaxParticipants)
	set(&c.ChannelConfig.SettingsTTL, f.SettingsTTL)
	set(&c.ChannelConfig.ErgoCacheTTL, f.ErgoCacheTTL)
	set(&c.QUICConfig.Addr, f.QUICAddr)
	set(&c.HTTPConfig.Addr, f.HTTPAddr)
	set(&c.HTTPConfig.PublicAddr, f.PublicHTTPAddr)
	set(&c.RoomConfig.MaxDatagramsPerSec, f.MaxDatagramsPerSec)
	set(&c.RoomConfig.MaxPayload, f.MaxDatagramPayload)
	if f.WebOrigins != nil {
		c.QUICConfig.WebTransportOrigins = strings.Join(*f.WebOrigins, ",")
		c.HTTPConfig.CORSAllowedOrigins = c.QUICConfig.WebTransportOrigins
		if c.HTTPConfig.CORSAllowedOrigins == "" {
			c.HTTPConfig.CORSAllowedOrigins = "*"
		}
	}
	return c
}

func set[T any](dst *T, v *T) {
	if v != nil {
		*dst = *v
	}
}

// RestartRequired lists the restart-only keys whose effective value in c
// differs from running (the config the process started with).
func RestartRequired(running, c Config) []string {
	var keys []string
	add := func(changed bool, key string) {
		if changed {
			keys = append(keys, key)
		}
	}
	add(running.QUICConfig.Addr != c.QUICConfig.Addr, KeyQUICAddr)
	add(running.HTTPConfig.Addr != c.HTTPConfig.Addr, KeyHTTPAddr)
	add(running.HTTPConfig.PublicAddr != c.HTTPConfig.PublicAddr, KeyPublicHTTPAddr)
	add(running.RoomConfig.MaxDatagramsPerSec != c.RoomConfig.MaxDatagramsPerSec, KeyMaxDatagramsPerSec)
	add(running.RoomConfig.MaxPayload != c.RoomConfig.MaxPayload, KeyMaxDatagramPayload)
	add(running.QUICConfig.WebTransportOrigins != c.QUICConfig.WebTransportOrigins ||
		running.CORSOrigins() != c.CORSOrigins(), KeyWebOrigins)
	return keys
}

// ExportVoice renders a `voice:` section from cfg (env + defaults, plus a
// config file's keys if one was loaded), for merging into the stack config
// file on an existing install. It always writes the rooms, rooms_strict and
// max_rooms_per_user keys; the others only when their env var is set
// (lookupEnv), so the file doesn't pin defaults.
func ExportVoice(cfg Config, f VoiceFile, rooms []RoomEntry, lookupEnv func(string) (string, bool)) ([]byte, error) {
	has := func(env string, file bool) bool {
		_, ok := lookupEnv(env)
		return ok || file
	}
	type room struct {
		ID      string `yaml:"id"`
		Name    string `yaml:"name"`
		MaxKbps int    `yaml:"max_kbps,omitempty"`
	}
	sec := &yaml.Node{Kind: yaml.MappingNode}
	add := func(key string, v any) error {
		var n yaml.Node
		if err := n.Encode(v); err != nil {
			return err
		}
		sec.Content = append(sec.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, &n)
		return nil
	}
	rs := make([]room, len(rooms))
	for i, r := range rooms {
		rs[i] = room(r)
	}
	type kv struct {
		key  string
		on   bool
		vals any
	}
	for _, e := range []kv{
		{"rooms", true, rs},
		{"rooms_strict", true, cfg.RoomConfig.Strict},
		{"max_rooms_per_user", true, cfg.ChannelConfig.MaxPerUser},
		{"max_rooms", has("VOICE_ROOMS_MAX", f.MaxRooms != nil), cfg.ChannelConfig.MaxRooms},
		{"max_participants", has("ROOM_MAX_PARTICIPANTS", f.MaxParticipants != nil), cfg.RoomConfig.MaxParticipants},
		{"settings_ttl", has("SETTINGS_TTL", f.SettingsTTL != nil), cfg.ChannelConfig.SettingsTTL.String()},
		{"ergo_cache_ttl", has("ERGO_CACHE_TTL", f.ErgoCacheTTL != nil), cfg.ChannelConfig.ErgoCacheTTL.String()},
		{"max_kbps", f.MaxKbps != nil, deref(f.MaxKbps)},
		{"quic_addr", has("QUIC_ADDR", f.QUICAddr != nil), cfg.QUICConfig.Addr},
		{"http_addr", has("HTTP_ADDR", f.HTTPAddr != nil), cfg.HTTPConfig.Addr},
		{"public_http_addr", has("PUBLIC_HTTP_ADDR", f.PublicHTTPAddr != nil), cfg.HTTPConfig.PublicAddr},
		{"max_datagrams_per_sec", has("MAX_DATAGRAMS_PER_SEC", f.MaxDatagramsPerSec != nil), cfg.RoomConfig.MaxDatagramsPerSec},
		{"max_datagram_payload", has("MAX_DATAGRAM_PAYLOAD", f.MaxDatagramPayload != nil), cfg.RoomConfig.MaxPayload},
		{"web_origins", has("WEBTRANSPORT_ORIGINS", f.WebOrigins != nil), splitOrigins(cfg.QUICConfig.WebTransportOrigins)},
	} {
		if !e.on {
			continue
		}
		if err := add(e.key, e.vals); err != nil {
			return nil, err
		}
	}
	doc := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: "voice"}, sec}}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// ReadVoiceFile reads path ("" or missing = no keys).
func ReadVoiceFile(path string) (VoiceFile, error) {
	if path == "" {
		return VoiceFile{}, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return VoiceFile{}, nil
	}
	if err != nil {
		return VoiceFile{}, err
	}
	f, err := ParseVoiceFile(b)
	if err != nil {
		return VoiceFile{}, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// splitOrigins turns a comma-separated origin list into a slice.
func splitOrigins(list string) []string {
	out := []string{}
	for _, o := range strings.Split(list, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out
}
