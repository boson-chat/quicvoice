// Package avsync maps a sender's audio timestamps to its wall clock for
// lip sync.
//
// quicvoice audio datagrams carry a 32-bit 48 kHz timestamp with a random
// start; quicvideo frames carry the sender's capture time in Unix µs. A
// sender's AudioClock reports (control stream, relayed by quicvoiced) tie
// the two together: "timestamp T was captured at wall time W". A Clock per
// sender turns any later audio timestamp into the sender's wall time, so a
// receiver can show the video frame captured at the same moment as the
// audio it is playing. No clock sync between machines is needed: both
// sides of the mapping are the sender's clocks.
package avsync

import (
	"sync"
)

const (
	// SampleRate is the audio timestamp rate.
	SampleRate = 48000
	// keep is how many recent reports the drift fit uses (~16 s at 1/s).
	keep = 16
	// minFitSpanUS: below this span of reports, assume the nominal rate.
	minFitSpanUS = 2_000_000
	// maxDriftPPM bounds the fitted rate (sound cards are within ~100 ppm).
	maxDriftPPM = 1000
	// restartUS: a report this far off the current mapping means the
	// sender restarted its audio clock (new random start): start over.
	restartUS = 2_000_000
)

type point struct {
	ts   int64 // unwrapped audio timestamp
	wall int64 // Unix µs
}

// Clock is one sender's audio clock → wall clock mapping. Safe for
// concurrent use.
type Clock struct {
	mu   sync.Mutex
	pts  []point
	last int64 // last unwrapped timestamp seen (reports and queries)
}

// unwrap places a 32-bit timestamp next to ref (within ±2^31 samples,
// about 12 hours at 48 kHz).
func unwrap(x uint32, ref int64) int64 {
	return ref + int64(int32(x-uint32(ref)))
}

// Report records "audio timestamp ts was captured at wallUS".
func (c *Clock) Report(ts uint32, wallUS int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pts) == 0 {
		c.last = int64(ts)
		c.pts = append(c.pts, point{c.last, wallUS})
		return
	}
	u := unwrap(ts, c.last)
	if w, _ := c.wallLocked(u); abs(w-wallUS) > restartUS {
		c.pts = c.pts[:0] // new audio clock: forget the old mapping
		u = int64(ts)
	}
	c.last = u
	c.pts = append(c.pts, point{u, wallUS})
	if len(c.pts) > keep {
		c.pts = append(c.pts[:0], c.pts[len(c.pts)-keep:]...)
	}
}

// WallUS is the sender's wall clock (Unix µs) when the audio with
// timestamp ts was captured; false before the first report.
func (c *Clock) WallUS(ts uint32) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pts) == 0 {
		return 0, false
	}
	return c.wallLocked(unwrap(ts, c.last))
}

// Have reports whether any report arrived (no reports = play unsynced).
func (c *Clock) Have() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pts) > 0
}

// wallLocked maps an unwrapped timestamp: a least-squares line through
// the recent reports (it follows the sender's audio/wall drift), or the
// nominal rate from the newest report while they span too little time.
func (c *Clock) wallLocked(u int64) (int64, bool) {
	n := len(c.pts)
	newest := c.pts[n-1]
	nominal := float64(SampleRate) / 1e6 // samples per µs
	rate := nominal
	anchorTS, anchorWall := float64(newest.ts), float64(newest.wall)
	if n >= 2 && newest.wall-c.pts[0].wall >= minFitSpanUS {
		// Fit ts = a + rate·wall around the means (well conditioned).
		var mw, mt float64
		for _, p := range c.pts {
			mw += float64(p.wall - newest.wall)
			mt += float64(p.ts - newest.ts)
		}
		mw /= float64(n)
		mt /= float64(n)
		var sxx, sxy float64
		for _, p := range c.pts {
			dw := float64(p.wall-newest.wall) - mw
			dt := float64(p.ts-newest.ts) - mt
			sxx += dw * dw
			sxy += dw * dt
		}
		if sxx > 0 {
			rate = sxy / sxx
			lo, hi := nominal*(1-maxDriftPPM/1e6), nominal*(1+maxDriftPPM/1e6)
			rate = min(max(rate, lo), hi)
			anchorTS = float64(newest.ts) + mt
			anchorWall = float64(newest.wall) + mw
		}
	}
	return int64(anchorWall + (float64(u)-anchorTS)/rate), true
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
