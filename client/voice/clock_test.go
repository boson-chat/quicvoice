package voice

import (
	"sync"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/wire"
)

type report struct {
	ts uint32
	us int64
}

// clockSender is a captureSender that also takes lip-sync reports.
type clockSender struct {
	captureSender
	mu      sync.Mutex
	reports []report
}

func (c *clockSender) SendAudioClock(ts uint32, us int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reports = append(c.reports, report{ts, us})
	return nil
}

func (c *clockSender) got() []report {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]report(nil), c.reports...)
}

// talk feeds n frames of tone (or silence), one 20 ms frame per callback,
// at synthetic wall clock *us, paced in real time.
func talk(e *Engine, n int, tone bool, us *int64) {
	var phase float64
	for range n {
		*us += 20_000
		pcm := make([]int16, FrameSamples)
		if tone {
			pcm = sine(FrameSamples, 0.5, &phase)
		}
		e.captureAt(pcm, *us)
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond)
}

// Lip-sync reports go out at a spurt start and about once a second while
// talking; each maps a sent frame's timestamp to its capture time. The
// audio stream itself is the same as without reports.
func TestClockReports(t *testing.T) {
	e, err := NewEngine(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	snd := &clockSender{}
	e.SetSender(snd)

	us := int64(1_800_000_000_000_000)
	talk(e, 125, true, &us) // 2.5 s of speech
	talk(e, 25, false, &us) // pause
	talk(e, 10, true, &us)  // a new spurt

	sent := snd.sent()
	reps := snd.got()
	if len(reps) < 3 || len(reps) > 6 {
		t.Fatalf("%d reports for 2.5 s + a second spurt, want ~4", len(reps))
	}
	// Every report names a sent frame, with that frame's capture time
	// (one frame per callback: it started 20 ms before the callback).
	byTS := map[uint32]int{}
	for i, u := range sent {
		byTS[u.Timestamp] = i
	}
	starts := 0
	for _, r := range reps {
		i, ok := byTS[r.ts]
		if !ok {
			t.Fatalf("report for timestamp %d, which wasn't sent", r.ts)
		}
		if sent[i].Flags&wire.FlagStart != 0 {
			starts++
		}
	}
	if starts != 2 {
		t.Fatalf("%d reports at spurt starts, want 2", starts)
	}
	// Capture times follow the audio clock exactly: 960 samples = 20 ms.
	first := reps[0]
	for _, r := range reps[1:] {
		wantUS := first.us + int64(r.ts-first.ts)*1_000_000/SampleRate
		if r.us != wantUS {
			t.Fatalf("report %+v: capture µs %d, want %d", r, r.us, wantUS)
		}
	}
	// The audio is untouched: consecutive frames, 960 samples apart.
	for i := 1; i < len(sent); i++ {
		if sent[i].Seq != sent[i-1].Seq+1 {
			t.Fatalf("frame %d: seq %d after %d", i, sent[i].Seq, sent[i-1].Seq)
		}
	}
}

// A plain Sender (older embedders) gets no reports and works as before.
func TestNoClockSender(t *testing.T) {
	e, err := NewEngine(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	snd := &captureSender{}
	e.SetSender(snd)
	us := int64(1_800_000_000_000_000)
	talk(e, 30, true, &us)
	if len(snd.sent()) < 25 {
		t.Fatalf("sent %d frames", len(snd.sent()))
	}
}
