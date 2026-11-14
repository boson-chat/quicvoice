// Package room tracks rooms and participants and fans audio out between them.
package room

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/metrics"
	"github.com/boson-chat/quicvoice/wire"
)

var (
	ErrRoomFull   = errors.New("room: full")
	ErrForbidden  = errors.New("room: not allowed in this room")
	ErrInRoom     = errors.New("room: already in a room")
	ErrEmptyRoom  = errors.New("room: room name required")
	ErrNotFound   = errors.New("room: no such room")
	errNoSessions = errors.New("room: no free session ids")

	// ErrDatagramTooLarge is returned (wrapped) by Peer.SendDatagram when the
	// datagram doesn't fit the receiver's current path MTU.
	ErrDatagramTooLarge = errors.New("room: datagram too large for path")

	// ErrSendQueueFull is returned by Peer.SendDatagram when the receiver's
	// downlink queue is full. Peers must never block the forwarding loop.
	ErrSendQueueFull = errors.New("room: receiver send queue full")
)

// Peer is the transport side of a participant.
type Peer interface {
	// SendDatagram queues b for the receiver. It must not block: Forward
	// calls it for every receiver in turn, so one slow receiver would stall
	// audio for the whole room.
	SendDatagram(b []byte) error
	SendControl(m *controlpb.ControlMessage) error
}

// Member is one participant in one room.
type Member struct {
	SessionID uint16
	Identity  *auth.Identity
	peer      Peer
	room      *Room

	selfMuted   atomic.Bool
	deafened    atomic.Bool
	serverMuted atomic.Bool
	// lastVoice is when the member last had a voiced frame forwarded
	// (UnixNano, 0 = never). Written on the audio hot path, read by List.
	lastVoice atomic.Int64
}

func (m *Member) Room() string { return m.room.name }

func (m *Member) participant() *controlpb.Participant {
	return &controlpb.Participant{
		SessionId:   uint32(m.SessionID),
		UserId:      m.Identity.UserID,
		DisplayName: m.Identity.DisplayName,
		Role:        m.Identity.Role,
		SelfMuted:   m.selfMuted.Load(),
		ServerMuted: m.serverMuted.Load(),
		Deafened:    m.deafened.Load(),
	}
}

// Room is a set of members. Membership changes take mu; the audio fast path
// reads an immutable snapshot of members without locking.
type Room struct {
	name     string
	mu       sync.Mutex
	members  map[uint16]*Member
	nextID   uint16
	snapshot atomic.Pointer[[]*Member]
}

// SpeakingWindow is how long after their last voiced frame a member still
// counts as speaking in List.
const SpeakingWindow = time.Second

// Info is a point-in-time view of one room, for room lists.
type Info struct {
	ID           string
	Name         string
	Participants []ParticipantInfo
}

// ParticipantInfo is a point-in-time view of one member.
type ParticipantInfo struct {
	SessionID uint16
	UserID    string
	Name      string
	// Muted is self- or server-muted.
	Muted    bool
	Deafened bool
	// Speaking: a voiced frame was forwarded within SpeakingWindow.
	Speaking bool
}

type RoomServiceImpl interface {
	Join(ctx context.Context, room string, id *auth.Identity, p Peer) (*Member, *controlpb.RoomState, error)
	// List returns the rooms id may join: configured rooms in config order,
	// then (when not strict) live ad-hoc rooms by name. It never blocks the
	// audio path.
	List(ctx context.Context, id *auth.Identity) []Info
	Leave(ctx context.Context, m *Member, reason string)
	SetMute(ctx context.Context, m *Member, muted, deafened bool)
	// Forward fans a raw uplink datagram out to the rest of m's room. It is
	// the per-packet hot path, so it takes no context and allocates once per
	// receiver.
	Forward(m *Member, uplink []byte)
}

type RoomService struct {
	maxParticipants int
	metrics         *metrics.Metrics
	// defs are the configured rooms, in display order. Read-only after
	// construction.
	defs    []Def
	defined map[string]bool
	// strict: only configured rooms can be joined.
	strict bool
	now    func() time.Time

	mu    sync.Mutex
	rooms map[string]*Room
}

type Option func(*RoomService)

// WithRooms sets the configured rooms. With strict, Join rejects any other
// room with ErrNotFound; without it, other names still create ad-hoc rooms.
// Without this option every room is ad-hoc.
func WithRooms(defs []Def, strict bool) Option {
	return func(s *RoomService) {
		s.defs = slices.Clone(defs)
		s.strict = strict
		s.defined = make(map[string]bool, len(defs))
		for _, d := range defs {
			s.defined[d.ID] = true
		}
	}
}

// WithClock replaces time.Now (tests).
func WithClock(now func() time.Time) Option {
	return func(s *RoomService) { s.now = now }
}

func NewRoomService(maxParticipants int, m *metrics.Metrics, opts ...Option) RoomServiceImpl {
	s := &RoomService{maxParticipants: maxParticipants, metrics: m, rooms: map[string]*Room{}, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	m.ObserveRooms(s.stats)
	return s
}

func (s *RoomService) List(_ context.Context, id *auth.Identity) []Info {
	// Copy the room index under the lock; members are then read from each
	// room's lock-free snapshot.
	s.mu.Lock()
	live := maps.Clone(s.rooms)
	s.mu.Unlock()

	now := s.now().UnixNano()
	out := make([]Info, 0, len(s.defs))
	for _, d := range s.defs {
		if id.CanJoin(d.ID) {
			out = append(out, Info{ID: d.ID, Name: d.Name, Participants: live[d.ID].participants(now)})
		}
	}
	if s.strict {
		return out
	}
	var adhoc []string
	for name := range live {
		if !s.defined[name] && id.CanJoin(name) {
			adhoc = append(adhoc, name)
		}
	}
	slices.Sort(adhoc)
	for _, name := range adhoc {
		out = append(out, Info{ID: name, Name: name, Participants: live[name].participants(now)})
	}
	return out
}

// participants lists r's members by session id. nil r = no members.
func (r *Room) participants(now int64) []ParticipantInfo {
	out := []ParticipantInfo{}
	if r == nil {
		return out
	}
	members := r.snapshot.Load()
	if members == nil {
		return out
	}
	for _, m := range *members {
		last := m.lastVoice.Load()
		out = append(out, ParticipantInfo{
			SessionID: m.SessionID,
			UserID:    m.Identity.UserID,
			Name:      m.Identity.DisplayName,
			Muted:     m.selfMuted.Load() || m.serverMuted.Load(),
			Deafened:  m.deafened.Load(),
			Speaking:  last != 0 && now-last < int64(SpeakingWindow),
		})
	}
	slices.SortFunc(out, func(a, b ParticipantInfo) int { return cmp.Compare(a.SessionID, b.SessionID) })
	return out
}

// stats is read by the metrics collector (every scrape / export interval).
// It walks the lock-free member snapshots, so it never blocks the audio path.
func (s *RoomService) stats() metrics.RoomStats {
	s.mu.Lock()
	rooms := make([]*Room, 0, len(s.rooms))
	for _, r := range s.rooms {
		rooms = append(rooms, r)
	}
	s.mu.Unlock()

	st := metrics.RoomStats{Rooms: int64(len(rooms))}
	for _, r := range rooms {
		members := r.snapshot.Load()
		if members == nil {
			continue
		}
		st.MaxRoomSize = max(st.MaxRoomSize, int64(len(*members)))
		for _, m := range *members {
			st.AddParticipant(m.Identity.Role)
		}
	}
	return st
}

func (s *RoomService) Join(_ context.Context, name string, id *auth.Identity, p Peer) (*Member, *controlpb.RoomState, error) {
	if name == "" {
		return nil, nil, ErrEmptyRoom
	}
	if s.strict && !s.defined[name] {
		return nil, nil, ErrNotFound
	}
	if !id.CanJoin(name) {
		return nil, nil, ErrForbidden
	}

	s.mu.Lock()
	r, ok := s.rooms[name]
	if !ok {
		r = &Room{name: name, members: map[uint16]*Member{}}
		s.rooms[name] = r
	}
	// Hold s.mu while touching r so Leave can't delete r between lookup and join.
	r.mu.Lock()
	fail := func(err error) (*Member, *controlpb.RoomState, error) {
		empty := len(r.members) == 0
		r.mu.Unlock()
		if empty {
			delete(s.rooms, name)
		}
		s.mu.Unlock()
		return nil, nil, err
	}
	if len(r.members) >= s.maxParticipants {
		return fail(ErrRoomFull)
	}
	sid, err := r.allocID()
	if err != nil {
		return fail(err)
	}
	m := &Member{SessionID: sid, Identity: id, peer: p, room: r}
	others := make([]*controlpb.Participant, 0, len(r.members))
	for _, o := range r.members {
		others = append(others, o.participant())
	}
	r.members[sid] = m
	r.rebuildSnapshot()
	r.mu.Unlock()
	s.mu.Unlock()

	r.broadcast(sid, &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_ParticipantJoined{
		ParticipantJoined: &controlpb.ParticipantJoined{Participant: m.participant()},
	}})

	state := &controlpb.RoomState{
		Room:          name,
		YourSessionId: uint32(sid),
		Participants:  append(others, m.participant()),
	}
	return m, state, nil
}

func (s *RoomService) Leave(_ context.Context, m *Member, reason string) {
	r := m.room
	s.mu.Lock()
	r.mu.Lock()
	if r.members[m.SessionID] != m {
		r.mu.Unlock()
		s.mu.Unlock()
		return
	}
	delete(r.members, m.SessionID)
	r.rebuildSnapshot()
	empty := len(r.members) == 0
	r.mu.Unlock()
	if empty {
		delete(s.rooms, r.name)
	}
	s.mu.Unlock()

	r.broadcast(m.SessionID, &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_ParticipantLeft{
		ParticipantLeft: &controlpb.ParticipantLeft{SessionId: uint32(m.SessionID), Reason: reason},
	}})
}

func (s *RoomService) SetMute(_ context.Context, m *Member, muted, deafened bool) {
	m.selfMuted.Store(muted)
	m.deafened.Store(deafened)
	m.room.broadcast(0, &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_ParticipantUpdated{
		ParticipantUpdated: &controlpb.ParticipantUpdated{Participant: m.participant()},
	}})
}

func (s *RoomService) Forward(m *Member, uplink []byte) {
	mt := s.metrics
	if len(uplink) < wire.UplinkHeaderLen {
		mt.Drop(metrics.DropMalformed)
		return
	}
	if !m.Identity.CanSpeak() || m.serverMuted.Load() || m.selfMuted.Load() {
		mt.Drop(metrics.DropNotAllowed)
		return
	}
	// Voice targets (whisper / sub-groups) come in a later milestone.
	if uplink[3] != 0 {
		mt.Drop(metrics.DropTargetUnsupported)
		return
	}
	members := m.room.snapshot.Load()
	if members == nil {
		return
	}
	voiced := uplink[2]&0x80 != 0
	if voiced {
		m.lastVoice.Store(s.now().UnixNano())
	}
	mt.FrameForwarded(voiced)
	sent, bytes := 0, 0
	for _, o := range *members {
		if o == m || o.deafened.Load() {
			continue
		}
		out, err := wire.ForwardUplink(make([]byte, 0, len(uplink)+2), uplink, m.SessionID, wire.TargetRoom)
		if err != nil {
			mt.Drop(metrics.DropMalformed)
			break
		}
		if err := o.peer.SendDatagram(out); err != nil {
			mt.Drop(sendDropReason(err))
			continue
		}
		sent++
		bytes += len(out)
	}
	if sent > 0 {
		mt.DatagramsSent(metrics.SentDownlink, sent, bytes)
	}
}

// sendDropReason classifies a SendDatagram failure. Only called on failure.
func sendDropReason(err error) metrics.DropReason {
	switch {
	case errors.Is(err, ErrSendQueueFull):
		return metrics.DropSendQueueFull
	case errors.Is(err, ErrDatagramTooLarge):
		return metrics.DropSendTooLarge
	}
	return metrics.DropSendFailed
}

// allocID hands out session ids monotonically, skipping 0 and ids in use, so a
// late packet from someone who just left isn't attributed to a newcomer.
// Caller holds r.mu.
func (r *Room) allocID() (uint16, error) {
	for range 1 << 16 {
		r.nextID++
		if r.nextID == 0 {
			continue
		}
		if _, used := r.members[r.nextID]; !used {
			return r.nextID, nil
		}
	}
	return 0, errNoSessions
}

// Caller holds r.mu.
func (r *Room) rebuildSnapshot() {
	list := make([]*Member, 0, len(r.members))
	for _, m := range r.members {
		list = append(list, m)
	}
	r.snapshot.Store(&list)
}

// broadcast sends a control message to every member except skip (0 = nobody).
func (r *Room) broadcast(skip uint16, msg *controlpb.ControlMessage) {
	members := r.snapshot.Load()
	if members == nil {
		return
	}
	for _, o := range *members {
		if o.SessionID == skip {
			continue
		}
		_ = o.peer.SendControl(msg)
	}
}
