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

// Mode decides when the mic transmits.
type Mode int

const (
	ModeVoiceActivity Mode = iota // transmit while the mic is above the threshold
	ModePushToTalk                // transmit while PTT is held
)

// Options tune the engine. Zero values give the defaults in brackets.
type Options struct {
	Bitrate      int     // Opus bitrate in bit/s [32000]
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
	frames  chan []int16
	stop    chan struct{}
	stopped sync.WaitGroup

	micLevel     atomic.Uint32 // float32 bits
	transmitting atomic.Bool
	framesSent   atomic.Uint64
}

type senderBox struct{ s Sender }

func NewEngine(opts Options) (*Engine, error) {
	opts.defaults()
	enc, err := opus.NewEncoder(SampleRate, 1, opus.AppVoIP)
	if err != nil {
		return nil, err
	}
	for _, set := range []func() error{
		func() error { return enc.SetBitrate(opts.Bitrate) },
		func() error { return enc.SetInBandFEC(true) },
		func() error { return enc.SetPacketLossPerc(10) },
	} {
		if err := set(); err != nil {
			return nil, err
		}
	}
	e := &Engine{
		opts:   opts,
		enc:    enc,
		capBuf: make([]int16, 0, FrameSamples),
		frames: make(chan []int16, 8),
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
func (e *Engine) Capture(pcm []int16) {
	for len(pcm) > 0 {
		n := min(FrameSamples-len(e.capBuf), len(pcm))
		e.capBuf = append(e.capBuf, pcm[:n]...)
		pcm = pcm[n:]
		if len(e.capBuf) == FrameSamples {
			frame := make([]int16, FrameSamples)
			copy(frame, e.capBuf)
			e.capBuf = e.capBuf[:0]
			select {
			case e.frames <- frame:
			default: // encoder behind; drop rather than block the callback
			}
		}
	}
}

// Playback fills pcm with mixed audio. Called from the audio callback.
func (e *Engine) Playback(pcm []int16) { e.mix.read(pcm) }

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
	)
	for {
		var frame []int16
		select {
		case <-e.stop:
			return
		case frame = <-e.frames:
		}

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
			want = hang > 0
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
