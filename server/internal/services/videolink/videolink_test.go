package videolink

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeVideo struct {
	mu     sync.Mutex
	events []event
	snaps  []map[string][]string
	fail   atomic.Int32 // fail this many event posts first
	auth   atomic.Value
}

func (f *fakeVideo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.auth.Store(r.Header.Get("Authorization"))
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/control/events":
		if f.fail.Add(-1) >= 0 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		var body struct{ Events []event }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.events = append(f.events, body.Events...)
		f.mu.Unlock()
	case r.Method == http.MethodPut && r.URL.Path == "/v1/control/rooms":
		var body struct{ Rooms map[string][]string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.snaps = append(f.snaps, body.Rooms)
		f.mu.Unlock()
	default:
		http.NotFound(w, r)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEventsAndSnapshots(t *testing.T) {
	f := &fakeVideo{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := New(Options{URL: srv.URL + "/", Token: "s3cret", SyncInterval: 50 * time.Millisecond,
		Snapshot: func() map[string][]string { return map[string][]string{"general": {"u1"}} }})
	go l.Run(ctx)

	l.MemberLeft("general", "u2")
	l.RoomClosed("afk")
	waitFor(t, "events", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.events) == 2 })
	f.mu.Lock()
	if f.events[0] != (event{Type: "leave", Room: "general", User: "u2"}) || f.events[1] != (event{Type: "close", Room: "afk"}) {
		t.Fatalf("events %+v", f.events)
	}
	f.mu.Unlock()
	waitFor(t, "snapshot", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.snaps) > 0 })
	f.mu.Lock()
	if got := f.snaps[0]["general"]; len(got) != 1 || got[0] != "u1" {
		t.Fatalf("snapshot %v", f.snaps[0])
	}
	f.mu.Unlock()
	if f.auth.Load() != "Bearer s3cret" {
		t.Fatalf("auth header %v", f.auth.Load())
	}
}

func TestEventsRetryUntilDelivered(t *testing.T) {
	f := &fakeVideo{}
	f.fail.Store(1)
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := New(Options{URL: srv.URL, SyncInterval: time.Hour})
	go l.Run(ctx)

	l.MemberLeft("general", "u1")
	waitFor(t, "retried event", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.events) == 1 })
}

func TestRunStopsAndNotifyNeverBlocks(t *testing.T) {
	l := New(Options{URL: "http://127.0.0.1:1"})
	for range 20_000 { // no Run: the queue is bounded and push never blocks
		l.MemberLeft("r", "u")
	}
	if len(l.queue) != 10_000 {
		t.Fatalf("queue %d", len(l.queue))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't stop")
	}
}
