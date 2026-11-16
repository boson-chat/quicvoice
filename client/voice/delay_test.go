package voice

import (
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/wire"
)

// pops counts reads (20 ms frames) until the sender is heard.
func popsUntilHeard(m *mixer, max int) int {
	buf := make([]int16, FrameSamples)
	for i := 1; i <= max; i++ {
		m.read(buf)
		if _, _, ok := m.playout(7); ok {
			return i
		}
	}
	return -1
}

// An extra playout delay makes the sender's next spurt wait for that many
// more frames before it plays.
func TestPlayoutDelay(t *testing.T) {
	m := newMixer(func() (frameDecoder, error) { return &fakeDecoder{}, nil }, 2)
	m.setDelay(7, 3)
	for i := range 8 {
		flags := uint8(0)
		if i == 0 {
			flags = wire.FlagStart
		}
		m.push(tsFrame(uint16(i), uint32(i*960), flags))
	}
	// Buffering waits for target (2) + extra (3) frames; all 8 are in, so
	// it plays at once, but the catch-up rule leaves the extra depth.
	if n := popsUntilHeard(m, 3); n != 1 {
		t.Fatalf("heard after %d reads", n)
	}
	m.mu.Lock()
	depth := m.streams[7].jb.depthLocked()
	m.mu.Unlock()
	if depth < 5 {
		t.Fatalf("buffer depth %d after the first frame: the extra delay was skipped away", depth)
	}
}

func TestPlayoutDelayWaitsForFrames(t *testing.T) {
	m := newMixer(func() (frameDecoder, error) { return &fakeDecoder{}, nil }, 2)
	m.setDelay(7, 2)
	buf := make([]int16, FrameSamples)
	m.push(tsFrame(0, 0, wire.FlagStart))
	for i := 1; i < 4; i++ { // 4 frames < target 2 + extra 2 … not yet
		m.read(buf)
		if _, _, ok := m.playout(7); ok {
			t.Fatalf("played with %d frames buffered", i)
		}
		m.push(tsFrame(uint16(i), uint32(i*960), 0))
	}
	m.read(buf)
	if _, _, ok := m.playout(7); !ok {
		t.Fatal("not playing with target + extra frames buffered")
	}
}

func TestSetPlayoutDelayRoundsAndCaps(t *testing.T) {
	e, err := NewEngine(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for d, want := range map[time.Duration]int{0: 0, 1: 1, 20 * time.Millisecond: 1, 25 * time.Millisecond: 2, 150 * time.Millisecond: 8, time.Second: 10, -time.Second: 0} {
		e.SetPlayoutDelay(9, d)
		e.mix.mu.Lock()
		got := e.mix.delays[9]
		e.mix.mu.Unlock()
		if got != want {
			t.Errorf("%v → %d frames, want %d", d, got, want)
		}
	}
	// Kept for a sender that (re)appears later.
	e.SetPlayoutDelay(9, 60*time.Millisecond)
	e.mix.push(wire.Downlink{SessionID: 9, Audio: wire.Audio{Seq: 1, Flags: wire.FlagStart, Payload: []byte{1}}})
	e.mix.mu.Lock()
	defer e.mix.mu.Unlock()
	if e.mix.streams[9].jb.extra != 3 {
		t.Fatalf("new stream's extra %d", e.mix.streams[9].jb.extra)
	}
}
