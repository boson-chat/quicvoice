package settings_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/server/internal/services/settings"
)

type fakeAuth struct {
	mu     sync.Mutex
	value  string
	status int
	calls  atomic.Int64
}

func (f *fakeAuth) set(v string, status int) {
	f.mu.Lock()
	f.value, f.status = v, status
	f.mu.Unlock()
}

func (f *fakeAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	if r.URL.Path != "/internal/v1/settings" || r.Header.Get("X-Internal-Token") != "itok" {
		http.Error(w, "nope", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status != 0 {
		http.Error(w, "down", f.status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"registration":"open","require_email_verification":true,"channel_creation":"registered","voice_channel_creation":"` + f.value + `"}`))
}

func TestPollsWithTTLAndKeepsLastGood(t *testing.T) {
	fa := &fakeAuth{value: "founders"}
	srv := httptest.NewServer(fa)
	defer srv.Close()
	now := time.Unix(0, 0)
	s := settings.NewSettingsService(settings.Options{URL: srv.URL, Token: "itok", TTL: 30 * time.Second,
		Fallback: settings.CreateAdmins, Now: func() time.Time { return now }})
	ctx := context.Background()

	if v := s.VoiceChannelCreation(ctx); v != settings.CreateFounders {
		t.Fatalf("first = %q", v)
	}
	fa.set("registered", 0)
	now = now.Add(10 * time.Second)
	if v := s.VoiceChannelCreation(ctx); v != settings.CreateFounders || fa.calls.Load() != 1 {
		t.Fatalf("within TTL = %q (%d calls)", v, fa.calls.Load())
	}
	now = now.Add(21 * time.Second)
	if v := s.VoiceChannelCreation(ctx); v != settings.CreateRegistered {
		t.Fatalf("after TTL = %q", v)
	}
	fa.set("", http.StatusInternalServerError)
	now = now.Add(31 * time.Second)
	if v := s.VoiceChannelCreation(ctx); v != settings.CreateRegistered {
		t.Fatalf("on error = %q, want last good", v)
	}
	fa.set("bogus", 0)
	now = now.Add(31 * time.Second)
	if v := s.VoiceChannelCreation(ctx); v != settings.CreateRegistered {
		t.Fatalf("bad value = %q, want last good", v)
	}
}

func TestFallback(t *testing.T) {
	ctx := context.Background()
	if v := settings.NewSettingsService(settings.Options{Fallback: settings.CreateRegistered}).VoiceChannelCreation(ctx); v != settings.CreateRegistered {
		t.Fatalf("no URL = %q", v)
	}
	// Unreachable before any success: fallback.
	s := settings.NewSettingsService(settings.Options{URL: "http://127.0.0.1:1", Timeout: 200 * time.Millisecond})
	if v := s.VoiceChannelCreation(ctx); v != settings.CreateAdmins {
		t.Fatalf("unreachable = %q", v)
	}
	// Field missing (older auth): fallback.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"registration":"open"}`))
	}))
	defer srv.Close()
	s = settings.NewSettingsService(settings.Options{URL: srv.URL, Fallback: settings.CreateFounders})
	if v := s.VoiceChannelCreation(ctx); v != settings.CreateFounders {
		t.Fatalf("missing field = %q", v)
	}
}

func TestParse(t *testing.T) {
	for _, s := range []string{"admins", "FOUNDERS", " registered "} {
		if _, err := settings.ParseVoiceCreation(s); err != nil {
			t.Fatal(s, err)
		}
	}
	if _, err := settings.ParseVoiceCreation("everyone"); err == nil {
		t.Fatal("accepted")
	}
}
