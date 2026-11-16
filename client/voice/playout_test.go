package voice

import (
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/wire"
)

func tsFrame(seq uint16, ts uint32, flags uint8) wire.Downlink {
	return wire.Downlink{SessionID: 7, Audio: wire.Audio{Seq: seq, Timestamp: ts, Flags: flags, Payload: []byte{1}}}
}

// Playout follows the samples handed to the device: the timestamp at the
// start of each callback, at that callback's time plus the output latency;
// concealed frames continue the timeline; nothing while silent.
func TestPlayout(t *testing.T) {
	m := newMixer(func() (frameDecoder, error) { return &fakeDecoder{}, nil }, 2)
	const t0 = uint32(4_294_000_000) // wraps during the test
	if _, _, ok := m.playout(7); ok {
		t.Fatal("playout before any audio")
	}
	m.push(tsFrame(100, t0, wire.FlagStart))
	m.push(tsFrame(101, t0+960, 0))
	m.push(tsFrame(102, t0+1920, 0))
	// 103 is lost (concealed), 104 arrives.
	m.push(tsFrame(104, t0+3840, 0))

	buf := make([]int16, 480) // 10 ms callbacks
	want := []uint32{t0, t0 + 480, t0 + 960, t0 + 1440, t0 + 1920, t0 + 2400, t0 + 2880, t0 + 3360, t0 + 3840}
	for i, w := range want {
		before := time.Now().UnixMicro()
		m.read(buf)
		ts, local, ok := m.playout(7)
		if !ok {
			t.Fatalf("callback %d: no playout", i)
		}
		if ts != w {
			t.Fatalf("callback %d: ts %d, want %d (offset %d)", i, ts, w, int64(ts)-int64(w))
		}
		// Default latency: one callback (480 samples = 10 ms).
		if d := local - before; d < 9_000 || d > 30_000 {
			t.Fatalf("callback %d: playout %d µs ahead of the callback, want ~10 ms", i, d)
		}
	}

	m.mu.Lock()
	m.latencyUS = 45_000 // the device reports its real latency
	m.mu.Unlock()
	m.push(tsFrame(105, t0+4800, wire.FlagEnd))
	before := time.Now().UnixMicro()
	m.read(buf)
	if _, local, ok := m.playout(7); !ok || local-before < 44_000 || local-before > 60_000 {
		t.Fatalf("with 45 ms latency: %d µs ahead, ok=%v", local-before, ok)
	}
	// After the spurt ends the sender is silent: no playout.
	for range 6 {
		m.read(buf)
	}
	if _, _, ok := m.playout(7); ok {
		t.Fatal("playout while the sender is silent")
	}
}
