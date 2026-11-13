package voice

import (
	"math"
	"testing"

	"github.com/boson-chat/quicvoice/wire"
	"gopkg.in/hraban/opus.v2"
)

// BenchmarkMix8Talkers measures one 20 ms mix of 8 simultaneous Opus
// talkers (decode ×8 + gain + limiter). Must stay far below 20 ms.
func BenchmarkMix8Talkers(b *testing.B) {
	const talkers = 8
	enc, _ := opus.NewEncoder(SampleRate, 1, opus.AppVoIP)
	_ = enc.SetBitrate(32000)
	pcm := make([]int16, FrameSamples)
	for i := range pcm {
		pcm[i] = int16(8000 * math.Sin(float64(i)*2*math.Pi*440/SampleRate))
	}
	buf := make([]byte, maxOpusFrame)
	n, _ := enc.Encode(pcm, buf)
	payload := buf[:n]

	m := newMixer(func() (frameDecoder, error) { return opus.NewDecoder(SampleRate, 1) }, 1)
	out := make([]int16, FrameSamples)
	var seq uint16
	b.ResetTimer()
	for range b.N {
		for s := range uint16(talkers) {
			m.push(wire.Downlink{Audio: wire.Audio{Seq: seq, Voice: true, Payload: payload}, SessionID: s + 1})
		}
		seq++
		m.read(out)
	}
}
