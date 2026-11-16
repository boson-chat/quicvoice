package voice

import (
	"math"
	"sync"
	"time"

	"github.com/boson-chat/quicvoice/wire"
)

// stream is one remote sender: its jitter buffer plus what the UI needs.
type stream struct {
	jb   *jitterBuffer
	gain float32 // per-user volume, 1 = 100%

	lastVoice time.Time // last frame with the sender's VAD bit set
	level     uint8     // last reported level, -dBov

	// Lip sync: the audio timestamp of this sender's frame in m.frame
	// (curOK: it contributed to the current frame).
	curTS uint32
	curOK bool
}

// mixer pulls one frame from every stream, applies per-user and master gain
// and a soft limiter, and serves the result to the playback callback in
// whatever chunk size the device asks for.
type mixer struct {
	newDecoder func() (frameDecoder, error)
	target     int

	mu       sync.Mutex
	streams  map[uint16]*stream
	gains    map[uint16]float32 // remembered across a sender leaving/rejoining
	master   float32
	deafened bool

	// Play-out state, touched only from the audio callback.
	frame []int16
	pos   int
	acc   []float32
	tmp   []int16

	// Lip sync, under mu: the last callback's wall clock and how far into
	// m.frame it started reading, and the output latency to add.
	cbUS      int64
	cbPos     int
	cbSamples int   // that callback's length
	latencyUS int64 // 0 = one callback's worth
}

func newMixer(newDecoder func() (frameDecoder, error), target int) *mixer {
	return &mixer{
		newDecoder: newDecoder,
		target:     target,
		streams:    map[uint16]*stream{},
		gains:      map[uint16]float32{},
		master:     1,
		frame:      make([]int16, FrameSamples),
		pos:        FrameSamples, // empty: mix on first read
		acc:        make([]float32, FrameSamples),
		tmp:        make([]int16, FrameSamples),
	}
}

// push routes a downlink frame to its sender's jitter buffer.
func (m *mixer) push(d wire.Downlink) {
	m.mu.Lock()
	s, ok := m.streams[d.SessionID]
	if !ok {
		dec, err := m.newDecoder()
		if err != nil {
			m.mu.Unlock()
			return
		}
		gain, ok := m.gains[d.SessionID]
		if !ok {
			gain = 1
		}
		s = &stream{jb: newJitterBuffer(dec, m.target), gain: gain}
		m.streams[d.SessionID] = s
	}
	if d.Voice {
		s.lastVoice = time.Now()
	}
	s.level = d.Level
	m.mu.Unlock()

	s.jb.push(d)
}

func (m *mixer) remove(sessionID uint16) {
	m.mu.Lock()
	delete(m.streams, sessionID)
	m.mu.Unlock()
}

func (m *mixer) setGain(sessionID uint16, gain float32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gains[sessionID] = gain
	if s, ok := m.streams[sessionID]; ok {
		s.gain = gain
	}
}

// read fills dst with mixed playback audio. Called from the audio callback.
func (m *mixer) read(dst []int16) {
	if m.pos == len(m.frame) {
		m.mixFrame()
		m.pos = 0
	}
	m.mu.Lock()
	m.cbUS, m.cbPos, m.cbSamples = time.Now().UnixMicro(), m.pos, len(dst)
	m.mu.Unlock()
	for len(dst) > 0 {
		if m.pos == len(m.frame) {
			m.mixFrame()
			m.pos = 0
		}
		n := copy(dst, m.frame[m.pos:])
		m.pos += n
		dst = dst[n:]
	}
}

func (m *mixer) mixFrame() {
	clear(m.acc)

	m.mu.Lock()
	master, deafened := m.master, m.deafened
	// Pop every stream even when deafened so buffers keep draining.
	for _, s := range m.streams {
		s.curOK = false
		if !s.jb.pop(m.tmp) {
			continue
		}
		s.curTS, s.curOK = s.jb.poppedTS()
		g := s.gain / 32768
		for i, v := range m.tmp {
			m.acc[i] += float32(v) * g
		}
	}
	m.mu.Unlock()

	if deafened {
		clear(m.frame)
		return
	}
	for i, v := range m.acc {
		m.frame[i] = int16(softLimit(v*master) * 32767)
	}
}

// playout says which of a sender's audio timestamps reaches the speaker
// when: the sample at the start of the last playback callback, at that
// callback's time plus the output latency. false while the sender is
// silent (nothing of theirs is playing).
func (m *mixer) playout(sessionID uint16) (ts uint32, localUS int64, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.streams[sessionID]
	if s == nil || !s.curOK || m.cbUS == 0 {
		return 0, 0, false
	}
	lat := m.latencyUS
	if lat == 0 {
		lat = int64(m.cbSamples) * 1_000_000 / SampleRate
	}
	return s.curTS + uint32(m.cbPos), m.cbUS + lat, true
}

// softLimit is linear up to 0.8 and bends smoothly towards ±1 above it, so
// several loud talkers saturate gently instead of clipping.
func softLimit(x float32) float32 {
	const knee = 0.8
	ax := float32(math.Abs(float64(x)))
	if ax <= knee {
		return x
	}
	y := knee + (1-knee)*float32(math.Tanh(float64((ax-knee)/(1-knee))))
	if x < 0 {
		return -y
	}
	return y
}

// speakingWindow is how long after the last voiced frame a sender still
// counts as speaking (covers the gaps between words).
const speakingWindow = 300 * time.Millisecond

// SenderState is a UI snapshot of one remote sender.
type SenderState struct {
	SessionID uint16
	Speaking  bool
	Level     float32 // 0..1 loudness for meters
	Gain      float32
	Jitter    JitterStats
}

func (m *mixer) snapshot() []SenderState {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	out := make([]SenderState, 0, len(m.streams))
	for sid, s := range m.streams {
		speaking := now.Sub(s.lastVoice) < speakingWindow
		lvl := float32(0)
		if speaking {
			lvl = levelToUnit(s.level)
		}
		out = append(out, SenderState{SessionID: sid, Speaking: speaking, Level: lvl, Gain: s.gain, Jitter: s.jb.Stats()})
	}
	return out
}

// levelToUnit maps -dBov (0 loud .. 127 silent) to 0..1 over a 60 dB range.
func levelToUnit(l uint8) float32 {
	if l >= 60 {
		return 0
	}
	return 1 - float32(l)/60
}
