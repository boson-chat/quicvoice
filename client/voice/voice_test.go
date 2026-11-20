package voice

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/wire"
)

// fakeDecoder "decodes" a payload into a frame filled with its first byte,
// so tests can see which frame played and whether it was concealed.
type fakeDecoder struct{ fec, plc int }

func (f *fakeDecoder) Decode(data []byte, pcm []int16) (int, error) {
	for i := range pcm {
		pcm[i] = int16(data[0])
	}
	return len(pcm), nil
}
func (f *fakeDecoder) DecodeFEC(data []byte, pcm []int16) error {
	f.fec++
	for i := range pcm {
		pcm[i] = -int16(data[0]) // negative marks "recovered via FEC from this frame"
	}
	return nil
}
func (f *fakeDecoder) DecodePLC(pcm []int16) error {
	f.plc++
	for i := range pcm {
		pcm[i] = 0
	}
	return nil
}

func frame(seq uint16, flags uint8) wire.Downlink {
	return wire.Downlink{Audio: wire.Audio{Seq: seq, Flags: flags, Payload: []byte{byte(seq)}}}
}

func TestJitterReordersAndBuffers(t *testing.T) {
	dec := &fakeDecoder{}
	jb := newJitterBuffer(dec, 3)
	pcm := make([]int16, FrameSamples)

	jb.push(frame(11, 0))
	jb.push(frame(10, wire.FlagStart))
	if jb.pop(pcm) {
		t.Fatal("played before target depth")
	}
	jb.push(frame(12, 0))

	for _, want := range []int16{10, 11, 12} {
		if !jb.pop(pcm) || pcm[0] != want {
			t.Fatalf("got %d, want %d", pcm[0], want)
		}
	}
}

func TestJitterConcealsWithFECThenPLC(t *testing.T) {
	dec := &fakeDecoder{}
	jb := newJitterBuffer(dec, 1)
	pcm := make([]int16, FrameSamples)

	jb.push(frame(1, wire.FlagStart))
	jb.pop(pcm) // 1
	jb.push(frame(3, 0))
	jb.pop(pcm) // 2 missing, 3 present → FEC from 3
	if dec.fec != 1 || pcm[0] != -3 {
		t.Fatalf("want FEC recovery from frame 3, got fec=%d pcm=%d", dec.fec, pcm[0])
	}
	jb.pop(pcm) // 3
	if pcm[0] != 3 {
		t.Fatalf("want frame 3, got %d", pcm[0])
	}
	jb.pop(pcm) // 4 missing, nothing after → PLC
	if dec.plc != 1 {
		t.Fatalf("want PLC, got %d", dec.plc)
	}
	if s := jb.Stats(); s.Concealed != 2 || s.Played != 2 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestJitterGoesIdleAfterEndAndDropsLate(t *testing.T) {
	jb := newJitterBuffer(&fakeDecoder{}, 1)
	pcm := make([]int16, FrameSamples)
	jb.push(frame(5, wire.FlagStart))
	jb.pop(pcm)
	jb.push(frame(4, 0)) // late
	jb.push(frame(6, wire.FlagEnd))
	jb.pop(pcm)
	if jb.pop(pcm) {
		t.Fatal("still playing after END")
	}
	if s := jb.Stats(); s.Late != 1 {
		t.Fatalf("late = %d", s.Late)
	}
}

func TestJitterCatchesUpWhenLong(t *testing.T) {
	jb := newJitterBuffer(&fakeDecoder{}, 2)
	pcm := make([]int16, FrameSamples)
	for s := range uint16(20) {
		jb.push(frame(s, 0))
	}
	jb.pop(pcm)
	if d := jb.Stats().Depth; d > 2+4 {
		t.Fatalf("depth %d after catch-up", d)
	}
}

func TestSeqWrap(t *testing.T) {
	jb := newJitterBuffer(&fakeDecoder{}, 2)
	pcm := make([]int16, FrameSamples)
	jb.push(frame(65535, wire.FlagStart))
	jb.push(frame(0, 0))
	if !jb.pop(pcm) || !jb.pop(pcm) {
		t.Fatal("did not play across wrap")
	}
	if pcm[0] != 0 {
		t.Fatalf("got %d after wrap", pcm[0])
	}
}

func TestSoftLimit(t *testing.T) {
	if softLimit(0.5) != 0.5 {
		t.Fatal("linear region changed")
	}
	for _, x := range []float32{1, 3, 100} {
		if y := softLimit(x); y > 1 || y <= 0.8 {
			t.Fatalf("softLimit(%v) = %v", x, y)
		}
	}
}

type captureSender struct {
	mu     sync.Mutex
	frames []wire.Uplink
}

func (c *captureSender) SendAudio(u *wire.Uplink) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := *u
	cp.Payload = append([]byte(nil), u.Payload...)
	c.frames = append(c.frames, cp)
	return nil
}

func (c *captureSender) sent() []wire.Uplink {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]wire.Uplink(nil), c.frames...)
}

func sine(n int, amp float64, phase *float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amp * 32767 * math.Sin(*phase))
		*phase += 2 * math.Pi * 440 / SampleRate
	}
	return out
}

// TestEngineLoopback runs a tone through VAD → Opus encode → loopback jitter
// buffer → Opus decode → mixer and checks the tone comes out the other end.
func TestEngineLoopback(t *testing.T) {
	e, err := NewEngine(Options{JitterFrames: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	snd := &captureSender{}
	e.SetSender(snd)
	e.SetLoopback(true)

	var phase float64
	for range 25 { // 0.5 s of tone, fed in 10 ms chunks like a device would
		e.Capture(sine(FrameSamples/2, 0.5, &phase))
		e.Capture(sine(FrameSamples/2, 0.5, &phase))
		time.Sleep(2 * time.Millisecond) // let the encoder goroutine keep up
	}
	time.Sleep(50 * time.Millisecond)

	sent := snd.sent()
	if len(sent) < 20 {
		t.Fatalf("sent %d frames, want ~25", len(sent))
	}
	if sent[0].Flags&wire.FlagStart == 0 || !sent[0].Voice {
		t.Fatalf("first frame should start a voiced spurt: %+v", sent[0].Audio)
	}

	out := make([]int16, FrameSamples)
	var energy float64
	for range 10 {
		e.Playback(out)
		for _, v := range out {
			energy += float64(v) * float64(v)
		}
	}
	rms := math.Sqrt(energy/float64(10*FrameSamples)) / 32768
	if rms < 0.1 {
		t.Fatalf("loopback output rms %.3f, tone lost", rms)
	}

	// Silence closes the spurt with an END frame, then stops sending.
	for range 30 {
		e.Capture(make([]int16, FrameSamples))
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	sent = snd.sent()
	last := sent[len(sent)-1]
	if last.Flags&wire.FlagEnd == 0 {
		t.Fatalf("last frame should be END, flags=%d", last.Flags)
	}

	// Muted: nothing more goes out.
	n := len(sent)
	e.SetMuted(true)
	for range 10 {
		e.Capture(sine(FrameSamples, 0.5, &phase))
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	if got := len(snd.sent()); got != n {
		t.Fatalf("sent %d frames while muted", got-n)
	}
}

// feed captures n frames of tone (amp) and waits for the encoder.
func feed(e *Engine, n int, amp float64, phase *float64) {
	for range n {
		e.Capture(sine(FrameSamples, amp, phase))
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(40 * time.Millisecond)
}

func avgPayload(fs []wire.Uplink) float64 {
	var n int
	for _, f := range fs {
		n += len(f.Payload)
	}
	return float64(n) / float64(len(fs))
}

// TestEngineSetBitrate: a bitrate change takes effect mid-stream, with no
// new spurt (no START flag) and the timestamps carrying on.
func TestEngineSetBitrate(t *testing.T) {
	e, err := NewEngine(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.Bitrate() != 32000 {
		t.Fatalf("default bitrate %d", e.Bitrate())
	}
	snd := &captureSender{}
	e.SetSender(snd)
	var phase float64
	feed(e, 25, 0.5, &phase)
	low := snd.sent()
	e.SetBitrate(128000)
	feed(e, 25, 0.5, &phase)
	all := snd.sent()
	high := all[len(low)+5:] // skip the frames around the switch
	if a, b := avgPayload(low[5:]), avgPayload(high); b < 2*a {
		t.Fatalf("payload %.0f B at 32k, %.0f B at 128k: bitrate didn't change", a, b)
	}
	for _, f := range all[1:] {
		if f.Flags&wire.FlagStart != 0 {
			t.Fatal("bitrate change restarted the spurt")
		}
	}
	if e.SetBitrate(1); e.Bitrate() != MinBitrate {
		t.Fatalf("clamp low: %d", e.Bitrate())
	}
	if e.SetBitrate(1e9); e.Bitrate() != MaxBitrate {
		t.Fatalf("clamp high: %d", e.Bitrate())
	}
}

// TestEngineMusicMode: music mode keeps transmitting through quiet
// passages in voice-activity mode (speech mode gates them), and switching
// back gates again.
func TestEngineMusicMode(t *testing.T) {
	e, err := NewEngine(Options{Bitrate: 128000, Music: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	snd := &captureSender{}
	e.SetSender(snd)
	var phase float64
	feed(e, 10, 0.5, &phase)
	feed(e, 40, 0.0005, &phase) // ≈ -66 dBFS: below the VAD threshold
	sent := snd.sent()
	if len(sent) < 45 {
		t.Fatalf("music mode sent %d of 50 frames: quiet passage gated", len(sent))
	}
	for _, f := range sent {
		if f.Flags&wire.FlagEnd != 0 {
			t.Fatal("music mode ended the spurt")
		}
	}
	// Muted still means silent.
	e.SetMuted(true)
	feed(e, 5, 0.0005, &phase)
	n := len(snd.sent())
	feed(e, 10, 0.5, &phase)
	if got := len(snd.sent()); got != n {
		t.Fatalf("sent %d frames while muted in music mode", got-n)
	}
	e.SetMuted(false)

	// Back to speech: the quiet closes the spurt and sending stops.
	e.SetMusic(false)
	feed(e, 30, 0.0005, &phase)
	n = len(snd.sent())
	feed(e, 10, 0.0005, &phase)
	if got := len(snd.sent()); got != n {
		t.Fatalf("speech mode sent %d frames of near-silence", got-n)
	}
}
