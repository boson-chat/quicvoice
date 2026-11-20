package liveconfig

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/boson-chat/quicvoice/server/config"
	"github.com/boson-chat/quicvoice/server/internal/db/repositories/voiceroom"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/services/voicechannel"
	"github.com/boson-chat/quicvoice/server/metrics"
)

type ttl struct{ got []time.Duration }

func (t *ttl) SetTTL(d time.Duration) { t.got = append(t.got, d) }

func TestApply(t *testing.T) {
	ctx := context.Background()
	var base config.Config
	base.RoomConfig = config.RoomConfig{Rooms: "general:General", Strict: true, MaxParticipants: 50, MaxDatagramsPerSec: 60, MaxPayload: 1000}
	base.ChannelConfig = config.ChannelConfig{MaxPerUser: 10, MaxRooms: 500, SettingsTTL: 30 * time.Second, ErgoCacheTTL: 15 * time.Second}
	base.QUICConfig.Addr = ":7700"
	defs, err := StaticDefs(base, config.VoiceFile{})
	if err != nil || !slices.Equal(defs, []room.Def{{ID: "general", Name: "General"}}) {
		t.Fatalf("env defs %v %v", defs, err)
	}
	repo, _ := voiceroom.NewRoomRepository("")
	vc, err := voicechannel.NewVoiceChannelService(ctx, voicechannel.Options{Repo: repo, Static: defs})
	if err != nil {
		t.Fatal(err)
	}
	rooms := room.NewRoomService(50, metrics.Noop(), room.WithDirectory(vc, true))
	st, er := &ttl{}, &ttl{}
	a := New(Options{Base: base, Running: base, RunningRooms: defs, Rooms: rooms, Channels: vc, Settings: st, Ergo: er})

	restart, err := a.Load(ctx, []byte("voice:\n  rooms: [a, b]\n  rooms_strict: false\n  settings_ttl: 1m\n  quic_addr: ':7701'\n  max_datagram_payload: 900\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(restart, []string{config.KeyQUICAddr, config.KeyMaxDatagramPayload}) {
		t.Fatalf("restart_required %v", restart)
	}
	if got := vc.Defs(); len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("defs %v", got)
	}
	if !slices.Equal(st.got, []time.Duration{time.Minute}) || len(er.got) != 0 {
		t.Fatalf("ttls %v %v", st.got, er.got)
	}
	// Strict off: an ad-hoc room shows up in the list once someone is in it.
	if l := rooms.List(ctx, &auth.Identity{UserID: "x"}); len(l) != 2 {
		t.Fatalf("list %v", l)
	}

	// Invalid: nothing changes.
	if _, err := a.Load(ctx, []byte("voice:\n  rooms: [Bad]\n")); err == nil {
		t.Fatal("no error")
	}
	if len(vc.Defs()) != 2 {
		t.Fatal("invalid file applied")
	}
	// Keys removed: back to env, nothing to restart.
	restart, err = a.Load(ctx, nil)
	if err != nil || len(restart) != 0 {
		t.Fatalf("reset: %v %v", restart, err)
	}
	if got := vc.Defs(); len(got) != 1 || got[0].ID != "general" {
		t.Fatalf("defs after reset %v", got)
	}
	if !slices.Equal(st.got, []time.Duration{time.Minute, 30 * time.Second}) {
		t.Fatalf("ttls %v", st.got)
	}
}

type peer struct{ ctrl []*controlpb.ControlMessage }

func (p *peer) SendDatagram([]byte) error { return nil }
func (p *peer) SendControl(m *controlpb.ControlMessage) error {
	p.ctrl = append(p.ctrl, m)
	return nil
}

func TestApplyBitrateCaps(t *testing.T) {
	ctx := context.Background()
	var base config.Config
	base.RoomConfig = config.RoomConfig{Rooms: "general:General", MaxParticipants: 50}
	defs, _ := StaticDefs(base, config.VoiceFile{})
	repo, _ := voiceroom.NewRoomRepository("")
	vc, err := voicechannel.NewVoiceChannelService(ctx, voicechannel.Options{Repo: repo, Static: defs})
	if err != nil {
		t.Fatal(err)
	}
	rooms := room.NewRoomService(50, metrics.Noop(), room.WithDirectory(vc, false))
	a := New(Options{Base: base, Running: base, RunningRooms: defs, Rooms: rooms, Channels: vc})
	if _, err := a.Load(ctx, []byte("voice:\n  max_kbps: 64\n  rooms:\n    - {id: general, name: General, max_kbps: 128}\n")); err != nil {
		t.Fatal(err)
	}
	id := &auth.Identity{UserID: "x", Role: controlpb.Role_SPEAKER}
	p := &peer{}
	_, st, err := rooms.Join(ctx, "general", id, p)
	if err != nil || st.MaxBitrate != 128000 {
		t.Fatalf("general: %v %v", st, err)
	}
	if _, st, _ := rooms.Join(ctx, "adhoc", id, &peer{}); st.MaxBitrate != 64000 {
		t.Fatalf("adhoc cap %d", st.MaxBitrate)
	}
	// Removing the keys lifts the caps, live.
	if _, err := a.Load(ctx, []byte("voice:\n  rooms: [{id: general, name: General}]\n")); err != nil {
		t.Fatal(err)
	}
	if len(p.ctrl) == 0 || p.ctrl[len(p.ctrl)-1].GetRoomSettings().GetMaxBitrate() != 0 || p.ctrl[len(p.ctrl)-1].GetRoomSettings() == nil {
		t.Fatalf("no RoomSettings: %v", p.ctrl)
	}
}
