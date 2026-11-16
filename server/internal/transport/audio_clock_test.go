package transport_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/client"
	"github.com/boson-chat/quicvoice/gen/controlpb"
)

type clockLog struct {
	mu   sync.Mutex
	msgs []*controlpb.AudioClock
}

func (l *clockLog) on(m *controlpb.ControlMessage) {
	if c := m.GetAudioClock(); c != nil {
		l.mu.Lock()
		l.msgs = append(l.msgs, c)
		l.mu.Unlock()
	}
}

func (l *clockLog) all() []*controlpb.AudioClock {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*controlpb.AudioClock(nil), l.msgs...)
}

// TestAudioClockRelay: a lip-sync report reaches the others in the room
// with the sender's session id, not the sender, at most 4 a second, and
// nothing is relayed outside a room.
func TestAudioClockRelay(t *testing.T) {
	ts := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var aliceLog, bobLog clockLog
	alice, err := client.Dial(ctx, client.Config{Addr: ts.addr, Token: "alice", InsecureSkipVerify: true, OnControl: aliceLog.on})
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	bob, err := client.Dial(ctx, client.Config{Addr: ts.addr, Token: "bob", InsecureSkipVerify: true, OnControl: bobLog.on})
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()

	// Not in a room yet: dropped.
	if err := alice.SendAudioClock(1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Join(ctx, "general"); err != nil {
		t.Fatal(err)
	}
	st, err := alice.Join(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(bobLog.all()); n != 0 {
		t.Fatalf("relayed %d reports sent outside a room", n)
	}

	// A burst of 10: the first goes through, the rest are within 250 ms.
	for i := range 10 {
		if err := alice.SendAudioClock(uint32(4_000_000_000+i), 1_800_000_000_000_000+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	_ = alice.SendAudioClock(4_000_048_000, 1_800_000_001_000_000) // after the window: relayed
	deadline := time.Now().Add(3 * time.Second)
	for len(bobLog.all()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	got := bobLog.all()
	if len(got) != 2 {
		t.Fatalf("bob got %d reports, want 2 (burst rate-limited)", len(got))
	}
	first := got[0]
	if first.GetSessionId() != st.GetYourSessionId() || first.GetTimestamp() != 4_000_000_000 ||
		first.GetCaptureUs() != 1_800_000_000_000_000 || first.GetVersion() != 1 {
		t.Fatalf("relayed %+v, alice is session %d", first, st.GetYourSessionId())
	}
	if got[1].GetTimestamp() != 4_000_048_000 {
		t.Fatalf("second report %+v", got[1])
	}
	if n := len(aliceLog.all()); n != 0 {
		t.Fatalf("alice got %d of her own reports back", n)
	}
}
