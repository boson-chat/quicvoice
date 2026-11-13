package room

import (
	"context"
	"fmt"
	"testing"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/boson-chat/quicvoice/server/metrics"
	"github.com/boson-chat/quicvoice/wire"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// tooSmallPeer rejects every datagram as larger than its path MTU.
type tooSmallPeer struct{ fakePeer }

func (*tooSmallPeer) SendDatagram([]byte) error {
	return fmt.Errorf("%w: DATAGRAM frame too large", ErrDatagramTooLarge)
}

func collected(t *testing.T, r *sdkmetric.ManualReader) map[string]map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	// name → first attribute value (or "") → value
	out := map[string]map[string]int64{}
	add := func(name string, attrs attribute.Set, v int64) {
		key := ""
		if attrs.Len() > 0 {
			key = attrs.ToSlice()[0].Value.Emit()
		}
		if out[name] == nil {
			out[name] = map[string]int64{}
		}
		out[name][key] = v
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					add(m.Name, p.Attributes, p.Value)
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					add(m.Name, p.Attributes, p.Value)
				}
			}
		}
	}
	return out
}

func TestForwardMetrics(t *testing.T) {
	ctx := context.Background()
	r := sdkmetric.NewManualReader()
	m, err := metrics.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	if err != nil {
		t.Fatal(err)
	}
	s := NewRoomService(10, m)
	a, _, _ := s.Join(ctx, "r", ident("a", controlpb.Role_SPEAKER), &fakePeer{})
	_, _, _ = s.Join(ctx, "r", ident("b", controlpb.Role_SPEAKER), &fakePeer{})
	_, _, _ = s.Join(ctx, "r", ident("c", controlpb.Role_LISTENER), &tooSmallPeer{})
	l, _, _ := s.Join(ctx, "other", ident("l", controlpb.Role_LISTENER), &fakePeer{})

	s.Forward(a, uplink(1))                          // → b ok, → c too large
	s.Forward(l, uplink(1))                          // listener: not_allowed
	s.Forward(a, uplink(2)[:wire.UplinkHeaderLen-1]) // malformed
	s.Forward(a, append([]byte{}, uplink(3)[:3]...)) // malformed
	targeted := uplink(4)
	targeted[3] = 1
	s.Forward(a, targeted) // target_unsupported

	got := collected(t, r)
	want := map[string]map[string]int64{
		"quicvoice.audio.frames.forwarded": {"": 1},
		"quicvoice.datagrams.sent":         {"downlink": 1, "pong": 0},
		"quicvoice.datagrams.dropped": {
			"send_too_large": 1, "not_allowed": 1, "malformed": 2, "target_unsupported": 1, "send_failed": 0,
		},
		"quicvoice.rooms":         {"": 2},
		"quicvoice.room.size.max": {"": 3},
		"quicvoice.participants":  {"speaker": 2, "listener": 2},
	}
	for name, vals := range want {
		for k, v := range vals {
			if got[name][k] != v {
				t.Errorf("%s{%s} = %d, want %d", name, k, got[name][k], v)
			}
		}
	}
}
