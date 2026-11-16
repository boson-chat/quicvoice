// Package metrics defines quicvoiced's OpenTelemetry instruments and the
// meter provider that exports them (Prometheus on /metrics, optional OTLP).
//
// Two kinds of recording live here:
//
//   - Per-event, low-rate things (handshakes, joins, sessions, QUIC samples) use
//     ordinary synchronous OTel instruments with attribute sets built once at
//     startup.
//   - Per-datagram things (the audio hot path) only bump plain atomics. Their
//     values are reported through observable counters when a reader collects,
//     so a datagram costs one atomic add per counter and never allocates or
//     touches the SDK.
package metrics

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// ScopeName is the instrumentation scope of every quicvoice instrument.
const ScopeName = "github.com/boson-chat/quicvoice/server"

// FrameDuration is the audio frame length the server announces in Welcome.
// Speech time is derived from it.
const FrameDuration = 20 * time.Millisecond

// HandshakeOutcome is how a new connection's Hello/Welcome exchange ended.
type HandshakeOutcome uint8

const (
	HandshakeOK       HandshakeOutcome = iota
	HandshakeNoStream                  // never opened the control stream
	HandshakeNoHello                   // opened it but sent nothing readable in time
	HandshakeProtocol                  // first message wasn't Hello
	HandshakeBadToken                  // token rejected
	HandshakeExpired                   // token expired
	numHandshakeOutcomes
)

var handshakeNames = [numHandshakeOutcomes]string{"ok", "no_stream", "no_hello", "protocol", "bad_token", "expired"}

func (o HandshakeOutcome) String() string { return handshakeNames[o] }

// JoinOutcome is the result of a Join request.
type JoinOutcome uint8

const (
	JoinOK JoinOutcome = iota
	JoinFull
	JoinForbidden
	JoinBadRequest    // e.g. empty room name
	JoinAlreadyInRoom // client bug: Join while still in a room
	JoinNotFound      // room not configured (VOICE_ROOMS_STRICT)
	numJoinOutcomes
)

var joinNames = [numJoinOutcomes]string{"ok", "full", "forbidden", "bad_request", "already_in_room", "not_found"}

func (o JoinOutcome) String() string { return joinNames[o] }

// LeaveReason is why a participant left a room.
type LeaveReason uint8

const (
	LeaveRequested    LeaveReason = iota // client sent Leave
	LeaveDisconnected                    // connection ended while in the room
	LeaveKicked                          // removed by the server (room deleted)
	numLeaveReasons
)

var leaveNames = [numLeaveReasons]string{"left", "disconnected", "kicked"}

func (r LeaveReason) String() string { return leaveNames[r] }

// DisconnectReason is why an authenticated session ended.
type DisconnectReason uint8

const (
	DisconnectClientClosed   DisconnectReason = iota // peer closed the connection or stream
	DisconnectIdleTimeout                            // no packets: network gone, client vanished
	DisconnectSlowConsumer                           // control queue full, we closed it
	DisconnectProtocolError                          // bad control framing / write failure
	DisconnectServerShutdown                         // we're shutting down
	DisconnectError                                  // anything else
	numDisconnectReasons
)

var disconnectNames = [numDisconnectReasons]string{"client_closed", "idle_timeout", "slow_consumer", "protocol_error", "server_shutdown", "error"}

func (r DisconnectReason) String() string { return disconnectNames[r] }

// ControlType is the type of a control message received from a client.
type ControlType uint8

const (
	ControlHello ControlType = iota
	ControlJoin
	ControlLeave
	ControlSetMute
	ControlPing
	ControlAudioClock
	ControlUnsupported // known-but-unimplemented or future messages
	numControlTypes
)

var controlNames = [numControlTypes]string{"hello", "join", "leave", "set_mute", "ping", "audio_clock", "unsupported"}

func (t ControlType) String() string { return controlNames[t] }

// RoomOp is a change to the voice room registry.
type RoomOp uint8

const (
	RoomCreate RoomOp = iota
	RoomUpdate
	RoomDelete
	numRoomOps
)

var roomOpNames = [numRoomOps]string{"create", "update", "delete"}

func (o RoomOp) String() string { return roomOpNames[o] }

// RoomChangeOutcome is how a registry change request ended.
type RoomChangeOutcome uint8

const (
	RoomChangeOK          RoomChangeOutcome = iota
	RoomChangeForbidden                     // policy / role check said no
	RoomChangeInvalid                       // bad name, parent, role…
	RoomChangeConflict                      // name taken under that parent
	RoomChangeNotFound                      // no such room
	RoomChangeLimit                         // per-user or total room limit
	RoomChangeUnavailable                   // Ergo needed but unreachable
	RoomChangeError                         // storage failure
	numRoomChangeOutcomes
)

var roomChangeNames = [numRoomChangeOutcomes]string{"ok", "forbidden", "invalid", "conflict", "not_found", "limit", "unavailable", "error"}

func (o RoomChangeOutcome) String() string { return roomChangeNames[o] }

// DefinedRooms is the size of the room registry.
type DefinedRooms struct{ Static, Dynamic int64 }

// DatagramKind is the kind nibble of a received datagram, bucketed.
type DatagramKind uint8

const (
	DatagramUplink DatagramKind = iota
	DatagramPing
	DatagramOther
	numDatagramKinds
)

var datagramKindNames = [numDatagramKinds]string{"uplink", "ping", "other"}

// SentKind is the kind of a datagram the server sends.
type SentKind uint8

const (
	SentDownlink SentKind = iota
	SentPong
	numSentKinds
)

var sentKindNames = [numSentKinds]string{"downlink", "pong"}

// DropReason is why a datagram was not delivered.
type DropReason uint8

const (
	DropMalformed         DropReason = iota // too short to parse
	DropRateLimited                         // sender over MAX_DATAGRAMS_PER_SEC
	DropTooLarge                            // uplink payload over MAX_DATAGRAM_PAYLOAD
	DropNotInRoom                           // audio before Join / after Leave
	DropNotAllowed                          // listener role, self- or server-muted
	DropTargetUnsupported                   // voice targets not implemented yet
	DropUnknownKind                         // reserved datagram kind
	DropSendTooLarge                        // receiver's path MTU too small for the downlink
	DropSendFailed                          // receiver connection closed / no datagram support
	DropSendQueueFull                       // receiver's downlink queue full (congested/slow receiver)
	numDropReasons
)

var dropNames = [numDropReasons]string{
	"malformed", "rate_limited", "too_large", "not_in_room", "not_allowed",
	"target_unsupported", "unknown_kind", "send_too_large", "send_failed",
	"send_queue_full",
}

func (r DropReason) String() string { return dropNames[r] }

// Roles reported on quicvoice.participants. Indexed by controlpb.Role.
var roleNames = [...]string{"unspecified", "listener", "speaker", "moderator", "admin"}

const numRoles = len(roleNames)

func roleIndex(r controlpb.Role) int {
	if r < 0 || int(r) >= numRoles {
		return 0
	}
	return int(r)
}

// RoomStats is a point-in-time view of the room service, read at collection.
type RoomStats struct {
	Rooms        int64
	Participants [numRoles]int64 // by role, indexed by controlpb.Role
	MaxRoomSize  int64
}

// AddParticipant counts one participant of the given role.
func (s *RoomStats) AddParticipant(r controlpb.Role) { s.Participants[roleIndex(r)]++ }

// QUICSample is the change in a connection's QUIC counters since the last
// sample, plus its current RTT estimate.
type QUICSample struct {
	SmoothedRTT     time.Duration
	RTTVariation    time.Duration
	PacketsSent     uint64
	PacketsReceived uint64
	PacketsLost     uint64
	BytesSent       uint64
	BytesReceived   uint64
}

// Minimum packets sent in a sample before its loss ratio is recorded; below
// this a single lost keepalive would read as huge loss.
const minPacketsForLoss = 50

// Metrics holds every quicvoice instrument. Create one per process with New
// and pass it to the services that record into it. All methods are safe for
// concurrent use.
type Metrics struct {
	meter metric.Meter

	// Synchronous instruments (low rate).
	connections       metric.Int64UpDownCounter
	handshakes        metric.Int64Counter
	handshakeDuration metric.Float64Histogram
	sessions          metric.Int64UpDownCounter
	sessionDuration   metric.Float64Histogram
	joins             metric.Int64Counter
	leaves            metric.Int64Counter
	roomSizeOnJoin    metric.Int64Histogram
	controlMessages   metric.Int64Counter
	quicRTT           metric.Float64Histogram
	quicRTTVar        metric.Float64Histogram
	quicLoss          metric.Float64Histogram
	quicPackets       metric.Int64Counter
	quicPacketsLost   metric.Int64Counter
	quicIO            metric.Int64Counter
	roomChanges       metric.Int64Counter

	handshakeAttrs  [numHandshakeOutcomes]metric.MeasurementOption
	joinAttrs       [numJoinOutcomes]metric.MeasurementOption
	leaveAttrs      [numLeaveReasons]metric.MeasurementOption
	disconnectAttrs [numDisconnectReasons]metric.MeasurementOption
	controlAttrs    [numControlTypes]metric.MeasurementOption
	roleAttrs       [numRoles]metric.MeasurementOption
	roomChangeAttrs [numRoomOps][numRoomChangeOutcomes]metric.MeasurementOption
	kindStatic      metric.MeasurementOption
	kindDynamic     metric.MeasurementOption
	dirTx, dirRx    metric.MeasurementOption

	clientVersions *clientVersions
	roomStats      atomic.Pointer[func() RoomStats]
	definedRooms   atomic.Pointer[func() DefinedRooms]

	// Hot-path counters, reported by the observable callback.
	dgIn         [numDatagramKinds]atomic.Int64
	dgInBytes    atomic.Int64
	dgOut        [numSentKinds]atomic.Int64
	dgOutBytes   atomic.Int64
	drops        [numDropReasons]atomic.Int64
	framesFwd    atomic.Int64
	framesVoiced atomic.Int64
	uplinkLost   atomic.Int64

	dgInKindAttrs  [numDatagramKinds]metric.MeasurementOption
	dgOutKindAttrs [numSentKinds]metric.MeasurementOption
	dropAttrs      [numDropReasons]metric.MeasurementOption
}

func attrOpt(kv ...attribute.KeyValue) metric.MeasurementOption {
	return metric.WithAttributeSet(attribute.NewSet(kv...))
}

// Bucket boundaries, chosen for a voice service.
var (
	// Accept → Welcome: one client RTT plus token verification.
	handshakeBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}
	// Minutes to hours.
	sessionBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200, 14400, 28800}
	// Room size including the joiner.
	roomSizeBuckets = []float64{1, 2, 3, 4, 5, 6, 8, 10, 15, 20, 30, 50, 75, 100, 200}
	// Voice gets uncomfortable past ~150 ms one way.
	rttBuckets    = []float64{.005, .01, .02, .03, .05, .075, .1, .15, .2, .3, .5, 1, 2}
	rttVarBuckets = []float64{.001, .002, .005, .01, .02, .03, .05, .1, .2, .5}
	// Opus FEC copes with a few percent; 10%+ is audible.
	lossBuckets = []float64{0, .001, .005, .01, .02, .03, .05, .1, .2, .5}
)

// New creates the instruments on mp's quicvoice meter.
func New(mp metric.MeterProvider) (*Metrics, error) {
	m := &Metrics{meter: mp.Meter(ScopeName), clientVersions: newClientVersions(maxClientVersions)}
	if err := m.init(); err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	return m, nil
}

// Noop returns Metrics backed by a no-op provider, for tests and tools.
func Noop() *Metrics {
	m, err := New(noop.NewMeterProvider())
	if err != nil {
		panic(err) // the no-op provider never fails
	}
	return m
}

func (m *Metrics) init() error {
	var err error
	mt := m.meter
	must := func(e error) {
		if err == nil && e != nil {
			err = e
		}
	}
	var e error

	m.connections, e = mt.Int64UpDownCounter("quicvoice.connections", metric.WithUnit("{connection}"),
		metric.WithDescription("Open QUIC connections, authenticated or not."))
	must(e)
	m.handshakes, e = mt.Int64Counter("quicvoice.handshakes", metric.WithUnit("{handshake}"),
		metric.WithDescription("Hello/Welcome exchanges by outcome."))
	must(e)
	m.handshakeDuration, e = mt.Float64Histogram("quicvoice.handshake.duration", metric.WithUnit("s"),
		metric.WithDescription("Time from QUIC accept to Welcome for successful handshakes."),
		metric.WithExplicitBucketBoundaries(handshakeBuckets...))
	must(e)
	m.sessions, e = mt.Int64UpDownCounter("quicvoice.sessions", metric.WithUnit("{session}"),
		metric.WithDescription("Authenticated sessions, by client version."))
	must(e)
	m.sessionDuration, e = mt.Float64Histogram("quicvoice.session.duration", metric.WithUnit("s"),
		metric.WithDescription("Lifetime of authenticated sessions, by disconnect reason."),
		metric.WithExplicitBucketBoundaries(sessionBuckets...))
	must(e)
	m.joins, e = mt.Int64Counter("quicvoice.room.joins", metric.WithUnit("{join}"),
		metric.WithDescription("Join requests by outcome."))
	must(e)
	m.leaves, e = mt.Int64Counter("quicvoice.room.leaves", metric.WithUnit("{leave}"),
		metric.WithDescription("Participants leaving a room, by reason."))
	must(e)
	m.roomSizeOnJoin, e = mt.Int64Histogram("quicvoice.room.size", metric.WithUnit("{participant}"),
		metric.WithDescription("Room size (including the joiner) sampled on every successful join."),
		metric.WithExplicitBucketBoundaries(roomSizeBuckets...))
	must(e)
	m.controlMessages, e = mt.Int64Counter("quicvoice.control.messages", metric.WithUnit("{message}"),
		metric.WithDescription("Control messages received from clients, by type."))
	must(e)
	m.quicRTT, e = mt.Float64Histogram("quicvoice.quic.rtt", metric.WithUnit("s"),
		metric.WithDescription("Smoothed RTT of authenticated connections, sampled periodically and at close."),
		metric.WithExplicitBucketBoundaries(rttBuckets...))
	must(e)
	m.quicRTTVar, e = mt.Float64Histogram("quicvoice.quic.rtt.variation", metric.WithUnit("s"),
		metric.WithDescription("RTT mean deviation (jitter proxy) of authenticated connections, sampled."),
		metric.WithExplicitBucketBoundaries(rttVarBuckets...))
	must(e)
	m.quicLoss, e = mt.Float64Histogram("quicvoice.quic.packet_loss", metric.WithUnit("1"),
		metric.WithDescription("Fraction of packets the server sent that were declared lost, per connection per sample window."),
		metric.WithExplicitBucketBoundaries(lossBuckets...))
	must(e)
	m.quicPackets, e = mt.Int64Counter("quicvoice.quic.packets", metric.WithUnit("{packet}"),
		metric.WithDescription("QUIC packets on authenticated connections, by direction."))
	must(e)
	m.quicPacketsLost, e = mt.Int64Counter("quicvoice.quic.packets.lost", metric.WithUnit("{packet}"),
		metric.WithDescription("QUIC packets sent by the server and declared lost."))
	must(e)
	m.quicIO, e = mt.Int64Counter("quicvoice.quic.io", metric.WithUnit("By"),
		metric.WithDescription("QUIC bytes on the wire (excluding UDP/IP headers) on authenticated connections, by direction."))
	must(e)
	m.roomChanges, e = mt.Int64Counter("quicvoice.room.changes", metric.WithUnit("{change}"),
		metric.WithDescription("Voice room create/update/delete requests on the public API, by op and outcome."))
	must(e)

	// Hot-path observables.
	dgIn, e := mt.Int64ObservableCounter("quicvoice.datagrams.received", metric.WithUnit("{datagram}"),
		metric.WithDescription("Datagrams received from clients, by kind (before rate limiting)."))
	must(e)
	dgOut, e := mt.Int64ObservableCounter("quicvoice.datagrams.sent", metric.WithUnit("{datagram}"),
		metric.WithDescription("Datagrams sent to clients, by kind."))
	must(e)
	dgIO, e := mt.Int64ObservableCounter("quicvoice.datagrams.io", metric.WithUnit("By"),
		metric.WithDescription("Datagram payload bytes, by direction."))
	must(e)
	drops, e := mt.Int64ObservableCounter("quicvoice.datagrams.dropped", metric.WithUnit("{datagram}"),
		metric.WithDescription("Datagrams not delivered, by reason. Send-side reasons count once per receiver."))
	must(e)
	fwd, e := mt.Int64ObservableCounter("quicvoice.audio.frames.forwarded", metric.WithUnit("{frame}"),
		metric.WithDescription("Uplink audio frames accepted and fanned out to the room."))
	must(e)
	speech, e := mt.Float64ObservableCounter("quicvoice.audio.speech", metric.WithUnit("s"),
		metric.WithDescription("Forwarded audio with the sender's voice-activity bit set. Its rate is the average number of people talking."))
	must(e)
	lost, e := mt.Int64ObservableCounter("quicvoice.audio.uplink.lost", metric.WithUnit("{frame}"),
		metric.WithDescription("Uplink frames missing on arrival, estimated from per-sender sequence gaps."))
	must(e)
	if err != nil {
		return err
	}

	for i := range numHandshakeOutcomes {
		m.handshakeAttrs[i] = attrOpt(attribute.String("outcome", handshakeNames[i]))
	}
	for i := range numJoinOutcomes {
		m.joinAttrs[i] = attrOpt(attribute.String("outcome", joinNames[i]))
	}
	for i := range numLeaveReasons {
		m.leaveAttrs[i] = attrOpt(attribute.String("reason", leaveNames[i]))
	}
	for op := range numRoomOps {
		for o := range numRoomChangeOutcomes {
			m.roomChangeAttrs[op][o] = attrOpt(attribute.String("op", roomOpNames[op]), attribute.String("outcome", roomChangeNames[o]))
		}
	}
	m.kindStatic = attrOpt(attribute.String("kind", "static"))
	m.kindDynamic = attrOpt(attribute.String("kind", "dynamic"))
	for i := range numDisconnectReasons {
		m.disconnectAttrs[i] = attrOpt(attribute.String("reason", disconnectNames[i]))
	}
	for i := range numControlTypes {
		m.controlAttrs[i] = attrOpt(attribute.String("type", controlNames[i]))
	}
	for i := range numRoles {
		m.roleAttrs[i] = attrOpt(attribute.String("role", roleNames[i]))
	}
	for i := range numDatagramKinds {
		m.dgInKindAttrs[i] = attrOpt(attribute.String("kind", datagramKindNames[i]))
	}
	for i := range numSentKinds {
		m.dgOutKindAttrs[i] = attrOpt(attribute.String("kind", sentKindNames[i]))
	}
	for i := range numDropReasons {
		m.dropAttrs[i] = attrOpt(attribute.String("reason", dropNames[i]))
	}
	m.dirTx = attrOpt(attribute.String("network.io.direction", "transmit"))
	m.dirRx = attrOpt(attribute.String("network.io.direction", "receive"))

	// Export every bounded series from the start (as 0), so rate() and
	// "absent" alerts work before the first event of each kind.
	bg := context.Background()
	m.connections.Add(bg, 0)
	m.quicPacketsLost.Add(bg, 0)
	for _, o := range m.handshakeAttrs {
		m.handshakes.Add(bg, 0, o)
	}
	for _, o := range m.joinAttrs {
		m.joins.Add(bg, 0, o)
	}
	for _, o := range m.leaveAttrs {
		m.leaves.Add(bg, 0, o)
	}
	for _, o := range m.controlAttrs {
		m.controlMessages.Add(bg, 0, o)
	}
	for op := range numRoomOps {
		for _, o := range []RoomChangeOutcome{RoomChangeOK, RoomChangeForbidden} {
			m.roomChanges.Add(bg, 0, m.roomChangeAttrs[op][o])
		}
	}
	for _, o := range []metric.MeasurementOption{m.dirTx, m.dirRx} {
		m.quicPackets.Add(bg, 0, o)
		m.quicIO.Add(bg, 0, o)
	}

	_, err = mt.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for i := range numDatagramKinds {
			o.ObserveInt64(dgIn, m.dgIn[i].Load(), m.dgInKindAttrs[i])
		}
		for i := range numSentKinds {
			o.ObserveInt64(dgOut, m.dgOut[i].Load(), m.dgOutKindAttrs[i])
		}
		o.ObserveInt64(dgIO, m.dgInBytes.Load(), m.dirRx)
		o.ObserveInt64(dgIO, m.dgOutBytes.Load(), m.dirTx)
		for i := range numDropReasons {
			o.ObserveInt64(drops, m.drops[i].Load(), m.dropAttrs[i])
		}
		o.ObserveInt64(fwd, m.framesFwd.Load())
		o.ObserveFloat64(speech, float64(m.framesVoiced.Load())*FrameDuration.Seconds())
		o.ObserveInt64(lost, m.uplinkLost.Load())
		return nil
	}, dgIn, dgOut, dgIO, drops, fwd, speech, lost)
	if err != nil {
		return err
	}

	// Room state, read from the room service at collection time so the
	// gauges can't drift.
	rooms, e := mt.Int64ObservableGauge("quicvoice.rooms", metric.WithUnit("{room}"),
		metric.WithDescription("Rooms with at least one participant."))
	must(e)
	parts, e := mt.Int64ObservableGauge("quicvoice.participants", metric.WithUnit("{participant}"),
		metric.WithDescription("Participants across all rooms, by role."))
	must(e)
	maxSize, e := mt.Int64ObservableGauge("quicvoice.room.size.max", metric.WithUnit("{participant}"),
		metric.WithDescription("Participants in the largest room (compare with ROOM_MAX_PARTICIPANTS)."))
	must(e)
	if err != nil {
		return err
	}
	_, err = mt.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		fn := m.roomStats.Load()
		if fn == nil {
			return nil
		}
		s := (*fn)()
		o.ObserveInt64(rooms, s.Rooms)
		for i := range numRoles {
			o.ObserveInt64(parts, s.Participants[i], m.roleAttrs[i])
		}
		o.ObserveInt64(maxSize, s.MaxRoomSize)
		return nil
	}, rooms, parts, maxSize)
	if err != nil {
		return err
	}

	defined, e := mt.Int64ObservableGauge("quicvoice.rooms.defined", metric.WithUnit("{room}"),
		metric.WithDescription("Rooms in the registry, by kind (static = VOICE_ROOMS, dynamic = created via the API)."))
	if e != nil {
		return e
	}
	_, err = mt.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		fn := m.definedRooms.Load()
		if fn == nil {
			return nil
		}
		d := (*fn)()
		o.ObserveInt64(defined, d.Static, m.kindStatic)
		o.ObserveInt64(defined, d.Dynamic, m.kindDynamic)
		return nil
	}, defined)
	return err
}

// ObserveDefinedRooms sets the function quicvoice.rooms.defined is read from.
func (m *Metrics) ObserveDefinedRooms(fn func() DefinedRooms) { m.definedRooms.Store(&fn) }

// RoomChange counts one registry change request.
func (m *Metrics) RoomChange(ctx context.Context, op RoomOp, o RoomChangeOutcome) {
	m.roomChanges.Add(ctx, 1, m.roomChangeAttrs[op][o])
}

// ObserveRooms sets the function quicvoice.rooms, quicvoice.participants and
// quicvoice.room.size.max are read from. It is called on the collecting
// goroutine, once per collection.
func (m *Metrics) ObserveRooms(stats func() RoomStats) { m.roomStats.Store(&stats) }

// --- connection / session lifecycle -----------------------------------------

func (m *Metrics) ConnectionOpened(ctx context.Context) { m.connections.Add(ctx, 1) }
func (m *Metrics) ConnectionClosed(ctx context.Context) { m.connections.Add(ctx, -1) }

// Handshake records how a Hello/Welcome exchange ended. took is only recorded
// for HandshakeOK.
func (m *Metrics) Handshake(ctx context.Context, o HandshakeOutcome, took time.Duration) {
	m.handshakes.Add(ctx, 1, m.handshakeAttrs[o])
	if o == HandshakeOK {
		m.handshakeDuration.Record(ctx, took.Seconds())
	}
}

// SessionStarted counts an authenticated session for clientVersion (as sent in
// Hello). Pass the returned value to SessionEnded.
func (m *Metrics) SessionStarted(ctx context.Context, clientVersion string) SessionToken {
	opt := m.clientVersions.attr(clientVersion)
	m.sessions.Add(ctx, 1, opt)
	return SessionToken{version: opt, start: time.Now()}
}

// SessionToken carries per-session metric state from start to end.
type SessionToken struct {
	version metric.MeasurementOption
	start   time.Time
}

func (m *Metrics) SessionEnded(ctx context.Context, t SessionToken, reason DisconnectReason) {
	m.sessions.Add(ctx, -1, t.version)
	m.sessionDuration.Record(ctx, time.Since(t.start).Seconds(), m.disconnectAttrs[reason])
}

func (m *Metrics) ControlMessage(ctx context.Context, t ControlType) {
	m.controlMessages.Add(ctx, 1, m.controlAttrs[t])
}

// --- rooms -------------------------------------------------------------------

func (m *Metrics) Join(ctx context.Context, o JoinOutcome) { m.joins.Add(ctx, 1, m.joinAttrs[o]) }

// RoomSize records the size of a room right after someone joined it.
func (m *Metrics) RoomSize(ctx context.Context, n int) { m.roomSizeOnJoin.Record(ctx, int64(n)) }

func (m *Metrics) Leave(ctx context.Context, r LeaveReason) { m.leaves.Add(ctx, 1, m.leaveAttrs[r]) }

// --- QUIC transport health ---------------------------------------------------

// QUICSample records one sample of a connection's transport stats. RTT is
// skipped while it's still zero (no ack seen yet).
func (m *Metrics) QUICSample(ctx context.Context, s QUICSample) {
	if s.SmoothedRTT > 0 {
		m.quicRTT.Record(ctx, s.SmoothedRTT.Seconds())
		m.quicRTTVar.Record(ctx, s.RTTVariation.Seconds())
	}
	if s.PacketsSent >= minPacketsForLoss {
		m.quicLoss.Record(ctx, float64(s.PacketsLost)/float64(s.PacketsSent))
	}
	if s.PacketsSent > 0 {
		m.quicPackets.Add(ctx, int64(s.PacketsSent), m.dirTx)
	}
	if s.PacketsReceived > 0 {
		m.quicPackets.Add(ctx, int64(s.PacketsReceived), m.dirRx)
	}
	if s.PacketsLost > 0 {
		m.quicPacketsLost.Add(ctx, int64(s.PacketsLost))
	}
	if s.BytesSent > 0 {
		m.quicIO.Add(ctx, int64(s.BytesSent), m.dirTx)
	}
	if s.BytesReceived > 0 {
		m.quicIO.Add(ctx, int64(s.BytesReceived), m.dirRx)
	}
}

// --- hot path: atomics only, no context, no allocation -----------------------

// DatagramReceived counts one incoming datagram of n bytes.
func (m *Metrics) DatagramReceived(k DatagramKind, n int) {
	m.dgIn[k].Add(1)
	m.dgInBytes.Add(int64(n))
}

// DatagramsSent counts count outgoing datagrams totalling n bytes.
func (m *Metrics) DatagramsSent(k SentKind, count, n int) {
	m.dgOut[k].Add(int64(count))
	m.dgOutBytes.Add(int64(n))
}

func (m *Metrics) Drop(r DropReason) { m.drops[r].Add(1) }

// FrameForwarded counts an uplink frame accepted for fan-out.
func (m *Metrics) FrameForwarded(voiced bool) {
	m.framesFwd.Add(1)
	if voiced {
		m.framesVoiced.Add(1)
	}
}

// UplinkLost counts n frames missing from a sender's sequence.
func (m *Metrics) UplinkLost(n int) { m.uplinkLost.Add(int64(n)) }
