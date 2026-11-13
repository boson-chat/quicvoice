package voice

import (
	"sync"

	"github.com/boson-chat/quicvoice/wire"
)

// frameDecoder is the subset of the Opus decoder the jitter buffer needs.
type frameDecoder interface {
	Decode(data []byte, pcm []int16) (int, error)
	DecodeFEC(data []byte, pcm []int16) error
	DecodePLC(pcm []int16) error
}

const (
	ringSize = 64 // > 1 s of 20 ms frames
	// Consecutive missing frames we conceal before treating the sender as gone.
	maxConcealed = 10
)

type jbState uint8

const (
	jbIdle jbState = iota
	jbBuffering
	jbPlaying
)

type slot struct {
	valid   bool
	seq     uint16
	flags   uint8
	payload []byte
}

// JitterStats are cumulative per-sender counters.
type JitterStats struct {
	Received  uint64 // frames accepted into the buffer
	Played    uint64 // frames decoded on time
	Concealed uint64 // frames covered by FEC or PLC
	Late      uint64 // arrived after their play-out slot
	Dropped   uint64 // discarded to catch up when the buffer ran long
	Depth     int    // frames currently buffered
}

// jitterBuffer reorders one sender's frames and turns them into a steady
// 20 ms PCM stream. push runs on the network goroutine, pop on the audio
// callback, so both take mu (held only for slot bookkeeping and one decode).
type jitterBuffer struct {
	mu     sync.Mutex
	dec    frameDecoder
	target int // frames to buffer before starting play-out

	ring      [ringSize]slot
	state     jbState
	next      uint16 // seq of the next frame to play
	concealed int    // consecutive concealed frames
	stats     JitterStats
}

func newJitterBuffer(dec frameDecoder, target int) *jitterBuffer {
	return &jitterBuffer{dec: dec, target: target}
}

// seqDiff is b-a in sequence space, treating wrap-around correctly.
func seqDiff(a, b uint16) int { return int(int16(b - a)) }

func (j *jitterBuffer) push(d wire.Downlink) {
	j.mu.Lock()
	defer j.mu.Unlock()

	// A new talk spurt resets play-out so we re-buffer instead of stretching
	// the silence gap with concealment.
	if d.Flags&wire.FlagStart != 0 && j.state == jbPlaying {
		j.resetLocked()
	}
	switch j.state {
	case jbIdle:
		j.state = jbBuffering
		j.next = d.Seq
	case jbBuffering:
		if seqDiff(j.next, d.Seq) < 0 {
			j.next = d.Seq // an earlier frame arrived out of order
		}
	case jbPlaying:
		ahead := seqDiff(j.next, d.Seq)
		if ahead < 0 {
			j.stats.Late++
			return
		}
		if ahead >= ringSize {
			// Huge jump (sender restarted, long outage): start over.
			j.resetLocked()
			j.state = jbBuffering
			j.next = d.Seq
		}
	}

	s := &j.ring[d.Seq%ringSize]
	s.valid, s.seq, s.flags = true, d.Seq, d.Flags
	s.payload = append(s.payload[:0], d.Payload...)
	j.stats.Received++
}

// pop writes the next 20 ms frame into pcm (len FrameSamples). It returns
// false when the sender is silent and pcm was left untouched.
func (j *jitterBuffer) pop(pcm []int16) bool {
	j.mu.Lock()
	defer j.mu.Unlock()

	switch j.state {
	case jbIdle:
		return false
	case jbBuffering:
		if j.depthLocked() < j.target {
			return false
		}
		j.state = jbPlaying
	}

	// Ran long (sender clock faster than ours, or a burst after a stall):
	// skip ahead so latency doesn't creep up.
	for j.depthLocked() > j.target+4 {
		if s := j.slotFor(j.next); s != nil {
			s.valid = false
			j.stats.Dropped++
		}
		j.next++
	}

	played := true
	if s := j.slotFor(j.next); s != nil {
		_, err := j.dec.Decode(s.payload, pcm)
		s.valid = false
		j.concealed = 0
		j.stats.Played++
		if err != nil {
			clear(pcm)
		}
		if s.flags&wire.FlagEnd != 0 {
			j.resetLocked()
			return true
		}
	} else {
		// Missing: recover it from the next frame's in-band FEC if we have
		// that, otherwise let Opus extrapolate (PLC).
		if nxt := j.slotFor(j.next + 1); nxt != nil {
			if err := j.dec.DecodeFEC(nxt.payload, pcm); err != nil {
				_ = j.dec.DecodePLC(pcm)
			}
		} else if err := j.dec.DecodePLC(pcm); err != nil {
			clear(pcm)
		}
		j.concealed++
		j.stats.Concealed++
		if j.concealed > maxConcealed {
			j.resetLocked()
			played = false
		}
	}
	j.next++
	return played
}

func (j *jitterBuffer) slotFor(seq uint16) *slot {
	s := &j.ring[seq%ringSize]
	if s.valid && s.seq == seq {
		return s
	}
	return nil
}

func (j *jitterBuffer) depthLocked() int {
	n := 0
	for i := range ringSize {
		s := &j.ring[(int(j.next)+i)%ringSize]
		if s.valid && seqDiff(j.next, s.seq) >= 0 {
			n++
		}
	}
	return n
}

func (j *jitterBuffer) resetLocked() {
	for i := range j.ring {
		j.ring[i].valid = false
	}
	j.state = jbIdle
	j.concealed = 0
}

func (j *jitterBuffer) Stats() JitterStats {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := j.stats
	s.Depth = j.depthLocked()
	return s
}
