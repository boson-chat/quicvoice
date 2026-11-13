package transport

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/boson-chat/quicvoice/server/metrics"
	"github.com/boson-chat/quicvoice/wire"
	"github.com/quic-go/quic-go"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func uplinkSeq(seq uint16, flags uint8) []byte {
	return wire.AppendUplink(nil, &wire.Uplink{Audio: wire.Audio{Flags: flags, Seq: seq, Payload: []byte("x")}})
}

func lostFrames(t *testing.T, r *sdkmetric.ManualReader) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "quicvoice.audio.uplink.lost" {
				return m.Data.(metricdata.Sum[int64]).DataPoints[0].Value
			}
		}
	}
	t.Fatal("quicvoice.audio.uplink.lost not collected")
	return 0
}

func TestTrackSeq(t *testing.T) {
	cases := []struct {
		name  string
		seqs  []uint16
		start map[int]bool // indexes carrying FlagStart
		want  int64
	}{
		{"contiguous", []uint16{1, 2, 3, 4}, nil, 0},
		{"gap", []uint16{1, 2, 5, 6}, nil, 2},
		{"wraps", []uint16{65534, 65535, 0, 1}, nil, 0},
		{"gap across wrap", []uint16{65534, 1}, nil, 2},
		{"reordered", []uint16{1, 3, 2, 4}, nil, 1}, // 2 counted lost on arrival of 3, then late
		{"duplicate", []uint16{1, 1, 2}, nil, 0},
		{"restart jump", []uint16{100, 101, 9000, 9001}, nil, 0},
		{"restart backwards", []uint16{9000, 9001, 5, 6, 8}, nil, 1},
		{"talk spurt start", []uint16{1, 2, 40, 41}, map[int]bool{2: true}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := sdkmetric.NewManualReader()
			m, err := metrics.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
			if err != nil {
				t.Fatal(err)
			}
			var last uint16
			have := false
			for i, s := range c.seqs {
				var flags uint8
				if c.start[i] {
					flags = wire.FlagStart
				}
				last, have = trackSeq(m, uplinkSeq(s, flags), last, have)
			}
			if got := lostFrames(t, r); got != c.want {
				t.Fatalf("lost = %d, want %d", got, c.want)
			}
		})
	}
}

func TestDisconnectReason(t *testing.T) {
	cases := []struct {
		err  error
		want metrics.DisconnectReason
	}{
		{&quic.ApplicationError{Remote: true, ErrorCode: 0}, metrics.DisconnectClientClosed},
		{&quic.ApplicationError{ErrorCode: CodeSlow}, metrics.DisconnectSlowConsumer},
		{&quic.ApplicationError{ErrorCode: CodeProtocol}, metrics.DisconnectProtocolError},
		{&quic.ApplicationError{ErrorCode: CodeShutdown}, metrics.DisconnectServerShutdown},
		{fmt.Errorf("read: %w", &quic.IdleTimeoutError{}), metrics.DisconnectIdleTimeout},
		{io.EOF, metrics.DisconnectClientClosed},
		{context.Canceled, metrics.DisconnectServerShutdown},
		{fmt.Errorf("wire: message too large"), metrics.DisconnectError},
	}
	for _, c := range cases {
		if got := disconnectReason(c.err); got != c.want {
			t.Errorf("disconnectReason(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
