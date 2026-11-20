// Package voice is the audio half of the quicvoice client: microphone
// capture, voice activity detection and push-to-talk, Opus encoding, per-sender
// jitter buffers, mixing with per-user volume, and speaker playback.
//
// It needs cgo (libopus, miniaudio). Build with -tags nolibopusfile unless
// libopusfile is installed; only libopus is used.
package voice

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/boson-chat/quicvoice/wire"
	"gopkg.in/hraban/opus.v2"
)

const (
	SampleRate   = 48000
	FrameSamples = 960 // 20 ms at 48 kHz mono
	maxOpusFrame = 1000
)

// Sender is where encoded frames go; *client.Client implements it.
type Sender interface {
	SendAudio(u *wire.Uplink) error
}

// ClockSender is a Sender that can also carry lip-sync reports (the
// quicvoice client does). The engine sends one when a talk spurt starts
// and about once a second while transmitting; the audio itself is
// unchanged.
type ClockSender interface {
	SendAudioClock(ts uint32, captureUS int64) error
}

// clockReportEvery is how often a lip-sync report is sent while talking.
const clockReportEvery = time.Second

// capFrame is one 20 ms frame and the wall clock (Unix µs) of its first
// sample.
type capFrame struct {
	pcm []int16
	us  int64
}

// Mode decides when the mic transmits.
type Mode int

const (
	ModeVoiceActivity Mode = iota // transmit while the mic is above the threshold
	ModePushToTalk                // transmit while PTT is held
)

// Options tune the engine. Zero values give the defaults in brackets.
type Options struct {
	Bitrate      int     // Opus bitrate in bit/s [32000]; see SetBitrate
	Music        bool    // music mode from the start; see SetMusic
	JitterFrames int     // frames buffered before play-out starts [3 = 60 ms]
	VADThreshold float32 // mic level 0..1 above which VAD opens [0.25 ≈ -45 dBFS]
}

func (o *Options) defaults() {
	if o.Bitrate == 0 {
		o.Bitrate = 32000
	}
	if o.JitterFrames == 0 {
		o.JitterFrames = 3
	}
	if o.VADThreshold == 0 {
		o.VADThreshold = 0.25
	}
}

// LoopbackSession is the session id used for your own voice in loopback mode.
const LoopbackSession = 0

// Stats is a UI snapshot of the engine.
type Stats struct {
	MicLevel     float32 // 0..1
	Transmitting bool
	FramesSent   uint64
	Senders      []SenderState
}

// Engine processes audio independently of devices: Capture takes mic PCM,
// Playback produces speaker PCM. Device (see device.go) wires these to
// hardware; tests call them directly.
type Engine struct {
	opts Options
	mix  *mixer
	enc  *opus.Encoder

	sender atomic.Pointer[senderBox]

	mode     atomic.Int32
	pttDown  atomic.Bool
	muted    atomic.Bool
	loopback atomic.Bool

	// Capture state, owned by the capture goroutine.
	capBuf  []int16
	frames  chan capFrame
	stop    chan struct{}
	stopped sync.WaitGroup

	// want: the encoder settings asked for (SetBitrate, SetMusic); the
	// encode goroutine applies them before its next frame (libopus
	// encoders aren't safe for concurrent use).
	wantMu    sync.Mutex
	want      encSettings
	wantDirty atomic.Bool

	micLevel     atomic.Uint32 // float32 bits
	transmitting atomic.Bool
	framesSent   atomic.Uint64
}

type senderBox struct{ s Sender }

// encSettings are what the Opus encoder is set up with.
type encSettings struct {
	bitrate int
	music   bool
}

// Bitrate limits (Opus's useful range).
const (
	MinBitrate = 6000
	MaxBitrate = 510000
)

// newEncoder makes a mono Opus encoder for es: AppVoIP for speech, AppAudio
// for music; in-band FEC either way.
func newEncoder(es encSettings) (*opus.Encoder, error) {
	app := opus.AppVoIP
	if es.music {
		app = opus.AppAudio
	}
	enc, err := opus.NewEncoder(SampleRate, 1, app)
	if err != nil {
		return nil, err
	}
	for _, set := range []func() error{
		func() error { return enc.SetBitrate(es.bitrate) },
		func() error { return enc.SetInBandFEC(true) },
		func() error { return enc.SetPacketLossPerc(10) },
	} {
		if err := set(); err != nil {
			return nil, err
		}
	}
	return enc, nil
}

func clampBitrate(bps int) int { return max(MinBitrate, min(MaxBitrate, bps)) }

func NewEngine(opts Options) (*Engine, error) {
	opts.defaults()
	opts.Bitrate = clampBitrate(opts.Bitrate)
	es := encSettings{bitrate: opts.Bitrate, music: opts.Music}
	enc, err := newEncoder(es)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		opts:   opts,
		enc:    enc,
		want:   es,
		capBuf: make([]int16, 0, FrameSamples),
		frames: make(chan capFrame, 8),
		stop:   make(chan struct{}),
		mix: newMixer(func() (frameDecoder, error) {
			return opus.NewDecoder(SampleRate, 1)
		}, opts.JitterFrames),
	}
	e.stopped.Add(1)
	go e.encodeLoop()
	return e, nil
}

// Close stops the encoder goroutine.
func (e *Engine) Close() {
	select {
	case <-e.stop:
	default:
		close(e.stop)
	}
	e.stopped.Wait()
}

// SetSender attaches (or with nil, detaches) the connection frames go to.
func (e *Engine) SetSender(s Sender) {
	if s == nil {
		e.sender.Store(nil)
		return
	}
	e.sender.Store(&senderBox{s})
}

// HandleAudio feeds a received frame in; wire it to client.Config.OnAudio.
func (e *Engine) HandleAudio(d wire.Downlink) { e.mix.push(d) }

// RemoveSender drops a participant's stream (call on ParticipantLeft).
func (e *Engine) RemoveSender(sessionID uint16) { e.mix.remove(sessionID) }

// SetBitrate changes the Opus bitrate (bit/s, clamped to 6000..510000)
// from the next frame, without interrupting the stream.
func (e *Engine) SetBitrate(bps int) {
	e.wantMu.Lock()
	e.want.bitrate = clampBitrate(bps)
	e.wantMu.Unlock()
	e.wantDirty.Store(true)
}

// SetMusic turns music mode on or off from the next frame. Music mode
// encodes for music (Opus's audio application, rather than VoIP's speech
// tuning) and, in voice-activity mode, transmits all the time instead of
// gating on the mic level, so quiet passages and fades aren't cut.
// Mute and push-to-talk still apply.
func (e *Engine) SetMusic(on bool) {
	e.wantMu.Lock()
	e.want.music = on
	e.wantMu.Unlock()
	e.wantDirty.Store(true)
}

// Bitrate is the bitrate the engine was last asked for.
func (e *Engine) Bitrate() int {
	e.wantMu.Lock()
	defer e.wantMu.Unlock()
	return e.want.bitrate
}

// applySettings brings the encoder in line with SetBitrate/SetMusic. On the
// encode goroutine only. A failed change keeps the old encoder.
func (e *Engine) applySettings(cur *encSettings) {
	if !e.wantDirty.Swap(false) {
		return
	}
	e.wantMu.Lock()
	want := e.want
	e.wantMu.Unlock()
	if want == *cur {
		return
	}
	if want.music != cur.music {
		// The application can't change after the first frame: a new
		// encoder (the receivers' decoders don't mind).
		enc, err := newEncoder(want)
		if err != nil {
			return
		}
		e.enc = enc
	} else if e.enc.SetBitrate(want.bitrate) != nil {
		return
	}
	*cur = want
}

func (e *Engine) SetMode(m Mode)     { e.mode.Store(int32(m)) }
func (e *Engine) SetPTT(down bool)   { e.pttDown.Store(down) }
func (e *Engine) SetMuted(m bool)    { e.muted.Store(m) }
func (e *Engine) SetLoopback(b bool) { e.loopback.Store(b) }

func (e *Engine) SetDeafened(d bool) {
	e.mix.mu.Lock()
	e.mix.deafened = d
	e.mix.mu.Unlock()
}

// SetUserVolume sets a sender's gain (0..2). It survives them leaving.
func (e *Engine) SetUserVolume(sessionID uint16, gain float32) {
	e.mix.setGain(sessionID, clamp(gain, 0, 2))
}

func (e *Engine) SetMasterVolume(gain float32) {
	e.mix.mu.Lock()
	e.mix.master = clamp(gain, 0, 2)
	e.mix.mu.Unlock()
}

func (e *Engine) Stats() Stats {
	return Stats{
		MicLevel:     math.Float32frombits(e.micLevel.Load()),
		Transmitting: e.transmitting.Load(),
		FramesSent:   e.framesSent.Load(),
		Senders:      e.mix.snapshot(),
	}
}

// Capture takes mic samples in any chunk size. Called from the audio
// callback: it only copies, encoding happens on its own goroutine.
func (e *Engine) Capture(pcm []int16) { e.captureAt(pcm, time.Now().UnixMicro()) }

// captureAt is Capture with the callback's wall clock: the chunk's last
// sample was captured at nowUS, so a frame's first sample was captured
// (samples from it to the chunk's end) earlier.
func (e *Engine) captureAt(pcm []int16, nowUS int64) {
	for len(pcm) > 0 {
		n := min(FrameSamples-len(e.capBuf), len(pcm))
		e.capBuf = append(e.capBuf, pcm[:n]...)
		pcm = pcm[n:]
		if len(e.capBuf) == FrameSamples {
			frame := make([]int16, FrameSamples)
			copy(frame, e.capBuf)
			e.capBuf = e.capBuf[:0]
			startUS := nowUS - int64(FrameSamples+len(pcm))*1_000_000/SampleRate
			select {
			case e.frames <- capFrame{pcm: frame, us: startUS}:
			default: // encoder behind; drop rather than block the callback
			}
		}
	}
}

// Playback fills pcm with mixed audio. Called from the audio callback.
func (e *Engine) Playback(pcm []int16) { e.mix.read(pcm) }

// Playout is for lip sync: the sender's audio timestamp ts reaches the
// speaker at local wall clock localUS (Unix µs). Map ts to the sender's
// capture time with an avsync.Clock fed by its AudioClock reports, and
// show the sender's video frame captured at that time at localUS. false
// while the sender is silent.
func (e *Engine) Playout(sessionID uint16) (ts uint32, localUS int64, ok bool) {
	return e.mix.playout(sessionID)
}

// maxPlayoutDelay caps SetPlayoutDelay.
const (
	maxPlayoutDelay = 200 * time.Millisecond
	// FrameDuration is one audio frame (FrameSamples at SampleRate).
	FrameDuration = time.Duration(FrameSamples) * time.Second / SampleRate
)

// SetPlayoutDelay delays one sender's audio by d (rounded up to 20 ms
// frames, at most 200 ms; 0 = none): lip sync, when their video arrives
// later than their audio would play. It takes effect from their next talk
// spurt (a longer delay can't be added mid-word without a gap); a shorter
// one applies at once by skipping ahead. Kept across them leaving and
// rejoining, like SetUserVolume.
func (e *Engine) SetPlayoutDelay(sessionID uint16, d time.Duration) {
	d = min(max(d, 0), maxPlayoutDelay)
	frames := int((d + FrameDuration - 1) / FrameDuration)
	e.mix.setDelay(sessionID, frames)
}

// SetOutputLatency sets the time from a playback callback to the speaker
// (the device's buffering), when the device can tell; otherwise one
// callback's length is assumed.
func (e *Engine) SetOutputLatency(d time.Duration) {
	e.mix.mu.Lock()
	e.mix.latencyUS = d.Microseconds()
	e.mix.mu.Unlock()
}

// vadHangover keeps VAD open between words (15 frames = 300 ms).
const vadHangover = 15

func (e *Engine) encodeLoop() {
	defer e.stopped.Done()
	var (
		seq     uint16
		ts      = uint32(time.Now().UnixNano())
		sending bool
		hang    int
		out     = make([]byte, maxOpusFrame)
		lastRep time.Time
		cur     = encSettings{bitrate: e.opts.Bitrate, music: e.opts.Music}
	)
	for {
		var cf capFrame
		select {
		case <-e.stop:
			return
		case cf = <-e.frames:
		}
		frame := cf.pcm
		e.applySettings(&cur)

		level := rmsUnit(frame)
		e.micLevel.Store(math.Float32bits(level))

		voiced := level >= e.opts.VADThreshold
		if voiced {
			hang = vadHangover
		} else if hang > 0 {
			hang--
		}
		var want bool
		switch Mode(e.mode.Load()) {
		case ModePushToTalk:
			want = e.pttDown.Load()
		default:
			// Music mode doesn't gate: a quiet passage isn't silence.
			want = hang > 0 || cur.music
		}
		if e.muted.Load() {
			want = false
		}

		var flags uint8
		switch {
		case want && !sending:
			flags = wire.FlagStart
		case !want && sending:
			flags = wire.FlagEnd // one last frame closes the spurt
		case !want:
			ts += FrameSamples
			e.transmitting.Store(false)
			continue
		}
		sending = want
		e.transmitting.Store(want)

		n, err := e.enc.Encode(frame, out)
		if err != nil {
			ts += FrameSamples
			continue
		}
		u := &wire.Uplink{Audio: wire.Audio{
			Flags:     flags,
			Voice:     voiced || hang > 0,
			Level:     unitToLevel(level),
			Seq:       seq,
			Timestamp: ts,
			Payload:   out[:n],
		}}
		if b := e.sender.Load(); b != nil {
			if err := b.s.SendAudio(u); err == nil {
				e.framesSent.Add(1)
			}
			// Lip sync: this frame's timestamp ↔ its capture wall clock, at
			// a spurt start and then about once a second.
			if cs, ok := b.s.(ClockSender); ok && (flags == wire.FlagStart || time.Since(lastRep) >= clockReportEvery) {
				if cs.SendAudioClock(ts, cf.us) == nil {
					lastRep = time.Now()
				}
			}
		}
		if e.loopback.Load() {
			e.mix.push(wire.Downlink{Audio: copyAudio(u.Audio), SessionID: LoopbackSession})
		}
		seq++
		ts += FrameSamples
	}
}

func copyAudio(a wire.Audio) wire.Audio {
	a.Payload = append([]byte(nil), a.Payload...)
	return a
}

// rmsUnit returns frame loudness on a 0..1 scale covering -60..0 dBFS.
func rmsUnit(frame []int16) float32 {
	var sum float64
	for _, v := range frame {
		f := float64(v) / 32768
		sum += f * f
	}
	rms := math.Sqrt(sum / float64(len(frame)))
	if rms <= 0 {
		return 0
	}
	db := 20 * math.Log10(rms) // ≤ 0
	return clamp(float32(1+db/60), 0, 1)
}

// unitToLevel converts 0..1 back to the wire's -dBov (0 loud .. 127 silent).
func unitToLevel(u float32) uint8 {
	if u <= 0 {
		return wire.LevelSilence
	}
	return uint8(math.Round(float64((1 - u) * 60)))
}

func clamp(v, lo, hi float32) float32 { return max(lo, min(hi, v)) }

var ErrNoDevice = errors.New("voice: no audio device")
