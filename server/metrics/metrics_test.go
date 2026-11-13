package metrics

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func newTest(t *testing.T) (*Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	r := sdkmetric.NewManualReader()
	m, err := New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	if err != nil {
		t.Fatal(err)
	}
	return m, r
}

func collect(t *testing.T, r *sdkmetric.ManualReader) map[string]metricdata.Aggregation {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]metricdata.Aggregation{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m.Data
		}
	}
	return out
}

// point finds the data point of name whose attributes contain kv (all of
// them). Sums and gauges return their value; histograms their count.
func point(t *testing.T, data map[string]metricdata.Aggregation, name string, kv ...attribute.KeyValue) float64 {
	t.Helper()
	match := func(s attribute.Set) bool {
		for _, want := range kv {
			if v, ok := s.Value(want.Key); !ok || v != want.Value {
				return false
			}
		}
		return true
	}
	switch d := data[name].(type) {
	case metricdata.Sum[int64]:
		for _, p := range d.DataPoints {
			if match(p.Attributes) {
				return float64(p.Value)
			}
		}
	case metricdata.Sum[float64]:
		for _, p := range d.DataPoints {
			if match(p.Attributes) {
				return p.Value
			}
		}
	case metricdata.Gauge[int64]:
		for _, p := range d.DataPoints {
			if match(p.Attributes) {
				return float64(p.Value)
			}
		}
	case metricdata.Histogram[float64]:
		for _, p := range d.DataPoints {
			if match(p.Attributes) {
				return float64(p.Count)
			}
		}
	case metricdata.Histogram[int64]:
		for _, p := range d.DataPoints {
			if match(p.Attributes) {
				return float64(p.Count)
			}
		}
	case nil:
		t.Fatalf("%s: not collected", name)
	}
	t.Fatalf("%s%v: no matching point", name, kv)
	return 0
}

func TestHotPathCounters(t *testing.T) {
	m, r := newTest(t)
	for range 3 {
		m.DatagramReceived(DatagramUplink, 100)
	}
	m.DatagramReceived(DatagramPing, 9)
	m.Drop(DropRateLimited)
	m.Drop(DropRateLimited)
	m.FrameForwarded(true)
	m.FrameForwarded(false)
	m.DatagramsSent(SentDownlink, 4, 400)
	m.UplinkLost(3)

	d := collect(t, r)
	kind := func(v string) attribute.KeyValue { return attribute.String("kind", v) }
	checks := []struct {
		name string
		kv   []attribute.KeyValue
		want float64
	}{
		{"quicvoice.datagrams.received", []attribute.KeyValue{kind("uplink")}, 3},
		{"quicvoice.datagrams.received", []attribute.KeyValue{kind("ping")}, 1},
		{"quicvoice.datagrams.received", []attribute.KeyValue{kind("other")}, 0},
		{"quicvoice.datagrams.io", []attribute.KeyValue{attribute.String("network.io.direction", "receive")}, 309},
		{"quicvoice.datagrams.io", []attribute.KeyValue{attribute.String("network.io.direction", "transmit")}, 400},
		{"quicvoice.datagrams.sent", []attribute.KeyValue{kind("downlink")}, 4},
		{"quicvoice.datagrams.dropped", []attribute.KeyValue{attribute.String("reason", "rate_limited")}, 2},
		{"quicvoice.datagrams.dropped", []attribute.KeyValue{attribute.String("reason", "send_failed")}, 0},
		{"quicvoice.audio.frames.forwarded", nil, 2},
		{"quicvoice.audio.speech", nil, FrameDuration.Seconds()},
		{"quicvoice.audio.uplink.lost", nil, 3},
	}
	for _, c := range checks {
		if got := point(t, d, c.name, c.kv...); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.kv, got, c.want)
		}
	}
}

// The per-datagram methods must not allocate, even with a real SDK behind them.
func TestHotPathDoesNotAllocate(t *testing.T) {
	m, _ := newTest(t)
	allocs := testing.AllocsPerRun(1000, func() {
		m.DatagramReceived(DatagramUplink, 120)
		m.FrameForwarded(true)
		m.DatagramsSent(SentDownlink, 5, 600)
		m.Drop(DropNotInRoom)
		m.UplinkLost(1)
	})
	if allocs != 0 {
		t.Fatalf("hot path allocates %v times per datagram", allocs)
	}
}

func TestRoomStatsObserved(t *testing.T) {
	m, r := newTest(t)
	m.ObserveRooms(func() RoomStats {
		var s RoomStats
		s.Rooms = 2
		s.MaxRoomSize = 3
		s.AddParticipant(controlpb.Role_SPEAKER)
		s.AddParticipant(controlpb.Role_SPEAKER)
		s.AddParticipant(controlpb.Role_LISTENER)
		s.AddParticipant(controlpb.Role(99)) // unknown → unspecified
		return s
	})
	d := collect(t, r)
	if got := point(t, d, "quicvoice.rooms"); got != 2 {
		t.Errorf("rooms = %v", got)
	}
	if got := point(t, d, "quicvoice.room.size.max"); got != 3 {
		t.Errorf("max room size = %v", got)
	}
	for role, want := range map[string]float64{"speaker": 2, "listener": 1, "unspecified": 1, "admin": 0} {
		if got := point(t, d, "quicvoice.participants", attribute.String("role", role)); got != want {
			t.Errorf("participants{role=%s} = %v, want %v", role, got, want)
		}
	}
}

func TestSessionLifecycle(t *testing.T) {
	m, r := newTest(t)
	ctx := context.Background()
	m.Handshake(ctx, HandshakeOK, 3*time.Millisecond)
	m.Handshake(ctx, HandshakeBadToken, 0)
	tok := m.SessionStarted(ctx, "QuicVoice-Go/0.1")
	other := m.SessionStarted(ctx, "")
	m.SessionEnded(ctx, other, DisconnectIdleTimeout)

	d := collect(t, r)
	if got := point(t, d, "quicvoice.handshakes", attribute.String("outcome", "bad_token")); got != 1 {
		t.Errorf("bad_token handshakes = %v", got)
	}
	if got := point(t, d, "quicvoice.handshake.duration"); got != 1 {
		t.Errorf("handshake duration count = %v, want 1 (failures aren't timed)", got)
	}
	if got := point(t, d, "quicvoice.sessions", attribute.String("client.version", "quicvoice-go/0.1")); got != 1 {
		t.Errorf("sessions{quicvoice-go/0.1} = %v", got)
	}
	if got := point(t, d, "quicvoice.sessions", attribute.String("client.version", "unknown")); got != 0 {
		t.Errorf("sessions{unknown} = %v", got)
	}
	if got := point(t, d, "quicvoice.session.duration", attribute.String("reason", "idle_timeout")); got != 1 {
		t.Errorf("session duration{idle_timeout} count = %v", got)
	}
	m.SessionEnded(ctx, tok, DisconnectClientClosed)
}

func TestQUICSampleLossNeedsEnoughPackets(t *testing.T) {
	m, r := newTest(t)
	ctx := context.Background()
	m.QUICSample(ctx, QUICSample{PacketsSent: 10, PacketsLost: 1}) // too few for a ratio, no RTT yet
	m.QUICSample(ctx, QUICSample{SmoothedRTT: 40 * time.Millisecond, PacketsSent: 500, PacketsLost: 5, BytesSent: 1000})
	d := collect(t, r)
	if got := point(t, d, "quicvoice.quic.packet_loss"); got != 1 {
		t.Errorf("loss samples = %v, want 1", got)
	}
	if got := point(t, d, "quicvoice.quic.rtt"); got != 1 {
		t.Errorf("rtt samples = %v, want 1", got)
	}
	if got := point(t, d, "quicvoice.quic.packets.lost"); got != 6 {
		t.Errorf("packets lost = %v, want 6", got)
	}
	if got := point(t, d, "quicvoice.quic.packets", attribute.String("network.io.direction", "transmit")); got != 510 {
		t.Errorf("packets sent = %v, want 510", got)
	}
}

func TestClientVersionCardinalityIsBounded(t *testing.T) {
	c := newClientVersions(3)
	seen := map[attribute.Set]bool{}
	for i := range 100 {
		seen[optSet(c.attr(fmt.Sprintf("client/%d", i)))] = true
	}
	// 3 distinct + "other".
	if len(seen) != 4 {
		t.Fatalf("%d distinct attribute sets, want 4", len(seen))
	}
	if got := optSet(c.attr("client/99")); got != optSet(c.other) {
		t.Fatalf("overflow version not bucketed as other: %v", got)
	}
	if got := optSet(c.attr("client/0")); got == optSet(c.other) {
		t.Fatal("early version lost its own series")
	}
}

func optSet(o metric.MeasurementOption) attribute.Set {
	return metric.NewAddConfig([]metric.AddOption{o}).Attributes()
}

func TestNormaliseClientVersion(t *testing.T) {
	for in, want := range map[string]string{
		"quicvoice-go/0.1":          "quicvoice-go/0.1",
		"  Boson Desktop/1.2.3 ":    "boson_desktop/1.2.3",
		"":                          "unknown",
		"💥💥":                        "unknown",
		"x\"; DROP TABLE users; --": "x_drop_table_users_--",
	} {
		if got := normaliseClientVersion(in); got != want {
			t.Errorf("normalise(%q) = %q, want %q", in, got, want)
		}
	}
	if got := normaliseClientVersion(string(make([]byte, 500)) + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); len(got) > maxClientVersionLen {
		t.Errorf("not truncated: %d chars", len(got))
	}
}
