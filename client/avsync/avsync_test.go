package avsync

import (
	"math"
	"math/rand"
	"testing"
)

const t0 = int64(1_800_000_000_000_000) // a Unix µs

// sender simulates a sender whose audio clock runs ppm fast relative to
// its wall clock, starting at a random 32-bit timestamp.
type sender struct {
	start uint32
	ppm   float64
}

// ts is the audio timestamp of the sample captured at wall time w.
func (s sender) ts(w int64) uint32 {
	samples := float64(w-t0) * SampleRate / 1e6 * (1 + s.ppm/1e6)
	return s.start + uint32(int64(math.Round(samples)))
}

func within(t *testing.T, what string, got, want, tolUS int64) {
	t.Helper()
	if d := got - want; d > tolUS || d < -tolUS {
		t.Fatalf("%s: off by %d µs (got %d, want %d)", what, d, got, want)
	}
}

func TestNoReports(t *testing.T) {
	var c Clock
	if _, ok := c.WallUS(123); ok || c.Have() {
		t.Fatal("mapped without a report")
	}
}

// A random start near 2^32: timestamps wrap a few seconds in.
func TestWrapAndRandomStart(t *testing.T) {
	s := sender{start: math.MaxUint32 - 3*SampleRate}
	var c Clock
	for i := range 3 {
		w := t0 + int64(i)*1_000_000
		c.Report(s.ts(w), w)
	}
	for _, sec := range []float64{3.5, 5, 10} { // after the wrap
		w := t0 + int64(sec*1e6)
		got, ok := c.WallUS(s.ts(w))
		if !ok {
			t.Fatal("no mapping")
		}
		within(t, "after wrap", got, w, 30)
	}
	// And a timestamp from before the last report (late packet).
	w := t0 + 500_000
	got, _ := c.WallUS(s.ts(w))
	within(t, "earlier timestamp", got, w, 30)
}

// A sender whose sound card runs 300 ppm fast: the fit follows it, where
// the nominal rate would be 1.5 ms off after 5 s.
func TestClockDrift(t *testing.T) {
	s := sender{start: rand.Uint32(), ppm: 300}
	var c Clock
	for i := range 12 {
		w := t0 + int64(i)*1_000_000
		c.Report(s.ts(w), w)
	}
	last := t0 + 11_000_000
	w := last + 5_000_000 // 5 s past the last report
	got, _ := c.WallUS(s.ts(w))
	within(t, "5 s extrapolation with drift", got, w, 100)
	if nominal := float64(5_000_000) * 300 / 1e6; nominal < 1000 {
		t.Fatalf("test too weak: nominal error %v µs", nominal)
	}
}

// Wall stamps carry a few ms of scheduling jitter: the fit averages it out.
func TestJitteredReports(t *testing.T) {
	s := sender{start: rand.Uint32(), ppm: -150}
	r := rand.New(rand.NewSource(1))
	var c Clock
	for i := range 16 {
		w := t0 + int64(i)*1_000_000
		c.Report(s.ts(w), w+int64(r.NormFloat64()*2000)) // ±2 ms
	}
	w := t0 + 16_000_000
	got, _ := c.WallUS(s.ts(w))
	within(t, "jittered reports", got, w, 2000)
}

// The sender restarts its audio clock (new random start): the old mapping
// is dropped instead of producing garbage.
func TestRestart(t *testing.T) {
	a := sender{start: 1_000}
	var c Clock
	for i := range 5 {
		w := t0 + int64(i)*1_000_000
		c.Report(a.ts(w), w)
	}
	b := sender{start: 3_000_000_000}
	for i := 5; i < 8; i++ {
		w := t0 + int64(i)*1_000_000
		c.Report(b.ts(w), w)
	}
	w := t0 + 8_500_000
	got, _ := c.WallUS(b.ts(w))
	within(t, "after restart", got, w, 30)
}

// Only the most recent reports count (a long call keeps a bounded window).
func TestBoundedHistory(t *testing.T) {
	s := sender{start: rand.Uint32(), ppm: 50}
	var c Clock
	for i := range 1000 {
		w := t0 + int64(i)*1_000_000
		c.Report(s.ts(w), w)
	}
	if len(c.pts) != keep {
		t.Fatalf("kept %d reports", len(c.pts))
	}
	w := t0 + 1000*1_000_000
	got, _ := c.WallUS(s.ts(w))
	within(t, "after 1000 reports", got, w, 50)
}
