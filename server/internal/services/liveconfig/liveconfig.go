// Package liveconfig applies the stack config file's voice section to a
// running quicvoiced: static rooms (added, renamed, reordered, removed:
// people in a removed room are kicked, as when a dynamic room is deleted),
// strict mode, room limits and cache TTLs change live; listener addresses
// and datagram limits are reported as restart_required.
package liveconfig

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/boson-chat/quicvoice/server/config"
	"github.com/boson-chat/quicvoice/server/internal/logger"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/services/voicechannel"
)

// TTLSetter is implemented by the settings and ergo services.
type TTLSetter interface{ SetTTL(time.Duration) }

type Options struct {
	// Base is the env + defaults config (before any file).
	Base config.Config
	// Running is the effective config the process started with (restart-only
	// keys are compared against it).
	Running config.Config
	// RunningRooms: the static rooms in use at start.
	RunningRooms []room.Def
	Rooms        room.RoomServiceImpl
	Channels     *voicechannel.VoiceChannelService
	Settings     TTLSetter // may be nil
	Ergo         TTLSetter // may be nil
}

type Applier struct {
	o     Options
	mu    sync.Mutex
	cur   config.Config
	rooms []room.Def
}

func New(o Options) *Applier {
	return &Applier{o: o, cur: o.Running, rooms: slices.Clone(o.RunningRooms)}
}

// StaticDefs: the file's rooms, or VOICE_ROOMS when the file has none.
func StaticDefs(base config.Config, f config.VoiceFile) ([]room.Def, error) {
	if rs, ok := f.StaticRooms(); ok {
		defs := make([]room.Def, len(rs))
		for i, r := range rs {
			defs[i] = room.Def{ID: r.ID, Name: r.Name}
		}
		return defs, nil
	}
	defs, err := room.ParseDefs(base.RoomConfig.Rooms)
	if err != nil {
		return nil, fmt.Errorf("VOICE_ROOMS: %w", err)
	}
	return defs, nil
}

// Load is a configfile.Loader: parse, validate, apply.
func (a *Applier) Load(ctx context.Context, data []byte) ([]string, error) {
	f, err := config.ParseVoiceFile(data)
	if err != nil {
		return nil, err
	}
	return a.Apply(ctx, f)
}

// Apply makes f the voice config. It returns the restart-only keys that
// differ from what the process runs with.
func (a *Applier) Apply(ctx context.Context, f config.VoiceFile) ([]string, error) {
	defs, err := StaticDefs(a.o.Base, f)
	if err != nil {
		return nil, err
	}
	next := f.Effective(a.o.Base)
	log := logger.FromCtx(ctx)

	a.mu.Lock()
	defer a.mu.Unlock()
	if !slices.Equal(defs, a.rooms) {
		removed, err := a.o.Channels.SetStatic(ctx, defs)
		for _, r := range removed {
			n := a.o.Rooms.CloseRoom(ctx, r.ID, fmt.Sprintf("voice channel %q was removed from the server config", r.Name))
			log.Info().Str("room", r.ID).Int("kicked", n).Msg("static voice room removed")
		}
		if err != nil {
			return nil, fmt.Errorf("voice.rooms: %w", err)
		}
		a.rooms = defs
		log.Info().Int("static", len(defs)).Msg("static voice rooms updated")
	}
	if next.RoomConfig.Strict != a.cur.RoomConfig.Strict {
		a.o.Rooms.SetStrict(next.RoomConfig.Strict)
	}
	if next.RoomConfig.MaxParticipants != a.cur.RoomConfig.MaxParticipants {
		a.o.Rooms.SetMaxParticipants(next.RoomConfig.MaxParticipants)
	}
	if next.ChannelConfig.MaxPerUser != a.cur.ChannelConfig.MaxPerUser || next.ChannelConfig.MaxRooms != a.cur.ChannelConfig.MaxRooms {
		a.o.Channels.SetLimits(next.ChannelConfig.MaxPerUser, next.ChannelConfig.MaxRooms)
	}
	if a.o.Settings != nil && next.ChannelConfig.SettingsTTL != a.cur.ChannelConfig.SettingsTTL {
		a.o.Settings.SetTTL(next.ChannelConfig.SettingsTTL)
	}
	if a.o.Ergo != nil && next.ChannelConfig.ErgoCacheTTL != a.cur.ChannelConfig.ErgoCacheTTL {
		a.o.Ergo.SetTTL(next.ChannelConfig.ErgoCacheTTL)
	}
	a.cur = next
	return config.RestartRequired(a.o.Running, next), nil
}
