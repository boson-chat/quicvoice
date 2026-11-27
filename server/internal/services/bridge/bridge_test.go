package bridge_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/server/internal/services/bridge"
	"github.com/boson-chat/quicvoice/server/internal/services/bridge/bridgetest"
)

func TestNickIn(t *testing.T) {
	f := bridgetest.New(t, "btok")
	f.Join("Vic", "#Boson")
	f.Watch("#quiet")
	now := time.Unix(1000, 0)
	s := bridge.NewBridgeService(bridge.Options{URL: f.URL() + "/", Token: "btok", TTL: 10 * time.Second, Now: func() time.Time { return now }})
	if !s.Enabled() {
		t.Fatal("not enabled")
	}
	ctx := context.Background()
	for _, c := range []struct {
		nick, channel string
		want          bool
	}{
		{"vic", "#boson", true},
		{"VIC", "#BOSON", true},
		{"carol", "#boson", false},
		{"vic", "#quiet", false},   // evrd is there, vic isn't
		{"vic", "#nowhere", false}, // evrd isn't there: can't tell
		{"", "#boson", false},
		{"vic", " ", false},
	} {
		if in, err := s.NickIn(ctx, c.nick, c.channel); err != nil || in != c.want {
			t.Errorf("NickIn(%q, %q) = %v %v, want %v", c.nick, c.channel, in, err, c.want)
		}
	}
	calls := f.Calls.Load()
	_, _ = s.NickIn(ctx, "Vic", "#Boson")
	if f.Calls.Load() != calls {
		t.Fatal("not cached")
	}
	now = now.Add(11 * time.Second)
	f.Part("vic", "#boson")
	if in, _ := s.NickIn(ctx, "vic", "#boson"); in || f.Calls.Load() != calls+1 {
		t.Fatalf("not refetched after TTL: in=%v", in)
	}
	s.SetTTL(time.Hour)
	s.SetTTL(0)
}

func TestUnavailable(t *testing.T) {
	ctx := context.Background()
	for _, o := range []bridge.Options{{}, {URL: "http://x"}, {Token: "t"}} {
		s := bridge.NewBridgeService(o)
		if _, err := s.NickIn(ctx, "vic", "#boson"); s.Enabled() || !errors.Is(err, bridge.ErrUnavailable) {
			t.Fatalf("%+v: enabled=%v err=%v", o, s.Enabled(), err)
		}
	}
	f := bridgetest.New(t, "btok")
	if _, err := bridge.NewBridgeService(bridge.Options{URL: f.URL(), Token: "wrong"}).NickIn(ctx, "vic", "#boson"); !errors.Is(err, bridge.ErrUnavailable) {
		t.Fatalf("bad token: %v", err)
	}
	for _, code := range []int{http.StatusNotFound, http.StatusInternalServerError} { // 404: an evrd without the endpoint
		f.SetStatus(code)
		if _, err := bridge.NewBridgeService(bridge.Options{URL: f.URL(), Token: "btok"}).NickIn(ctx, "vic", "#boson"); !errors.Is(err, bridge.ErrUnavailable) {
			t.Fatalf("%d: %v", code, err)
		}
	}
	// Not the managed API's answer.
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }))
	t.Cleanup(junk.Close)
	if _, err := bridge.NewBridgeService(bridge.Options{URL: junk.URL, Token: "btok"}).NickIn(ctx, "vic", "#boson"); !errors.Is(err, bridge.ErrUnavailable) {
		t.Fatalf("junk: %v", err)
	}
	if _, err := bridge.NewBridgeService(bridge.Options{URL: "http://127.0.0.1:1", Token: "btok", Timeout: time.Second}).NickIn(ctx, "vic", "#boson"); !errors.Is(err, bridge.ErrUnavailable) {
		t.Fatalf("unreachable: %v", err)
	}
}

// The channel's # (and anything else) is escaped in the path.
func TestEscaping(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"known":true,"present":true}`))
	}))
	t.Cleanup(srv.Close)
	in, err := bridge.NewBridgeService(bridge.Options{URL: srv.URL, Token: "t"}).NickIn(context.Background(), "v[i]c", "#a/b?c")
	if err != nil || !in || got != "/api/managed/channels/%23a%2Fb%3Fc/members/v%5Bi%5Dc" {
		t.Fatalf("in=%v err=%v path=%s", in, err, got)
	}
}
