package ergo_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/server/internal/roles"
	"github.com/boson-chat/quicvoice/server/internal/services/ergo"
	"github.com/boson-chat/quicvoice/server/internal/services/ergo/ergotest"
)

func TestStanding(t *testing.T) {
	f := ergotest.New(t, "tok")
	f.Register("#Boson", "Alice")
	f.Join("alice", "#Boson", "")
	f.Join("alice", "#Dev", "h")
	f.Join("bob", "#boson", "o")

	now := time.Unix(1000, 0)
	s := ergo.NewErgoService(ergo.Options{URL: f.URL(), Token: "tok", TTL: 10 * time.Second, Now: func() time.Time { return now }})
	ctx := context.Background()

	a, err := s.Standing(ctx, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if a.Role("#BOSON") != roles.Founder || a.Role("#dev") != roles.Halfop || a.Role("#other") != roles.Member {
		t.Fatalf("alice roles: %v %v %v", a.Role("#boson"), a.Role("#dev"), a.Role("#other"))
	}
	if !a.In("#boson") || a.In("#other") {
		t.Fatal("alice membership")
	}
	if got := a.FoundedChannels(); !slices.Equal(got, []string{"#boson"}) {
		t.Fatalf("founded = %v", got)
	}
	b, _ := s.Standing(ctx, "bob")
	if b.Role("#boson") != roles.Op {
		t.Fatalf("bob = %v", b.Role("#boson"))
	}
	nobody, err := s.Standing(ctx, "carol")
	if err != nil || nobody.Role("#boson") != roles.Member || nobody.In("#boson") {
		t.Fatalf("carol = %+v %v", nobody, err)
	}

	// Cached for the TTL: 3 accounts × 2 calls.
	calls := f.Calls.Load()
	_, _ = s.Standing(ctx, "ALICE")
	if f.Calls.Load() != calls || calls != 6 {
		t.Fatalf("calls %d → %d, want cached", calls, f.Calls.Load())
	}
	now = now.Add(11 * time.Second)
	f.Part("alice", "#dev")
	a, _ = s.Standing(ctx, "alice")
	if a.In("#dev") || f.Calls.Load() != calls+2 {
		t.Fatal("not refetched after TTL")
	}
}

func TestUnavailable(t *testing.T) {
	ctx := context.Background()
	if _, err := ergo.NewErgoService(ergo.Options{}).Standing(ctx, "a"); !errors.Is(err, ergo.ErrUnavailable) {
		t.Fatalf("disabled: %v", err)
	}
	f := ergotest.New(t, "tok")
	if _, err := ergo.NewErgoService(ergo.Options{URL: f.URL(), Token: "wrong"}).Standing(ctx, "a"); !errors.Is(err, ergo.ErrUnavailable) {
		t.Fatalf("bad token: %v", err)
	}
	f.SetDown(true)
	if _, err := ergo.NewErgoService(ergo.Options{URL: f.URL(), Token: "tok"}).Standing(ctx, "a"); !errors.Is(err, ergo.ErrUnavailable) {
		t.Fatalf("down: %v", err)
	}
}

// TestNickIn: a guest's membership by nick counts only a client that isn't
// logged into any account; cached like Standing.
func TestNickIn(t *testing.T) {
	f := ergotest.New(t, "tok")
	f.JoinGuest("Vic", "#Boson", "o")
	f.Join("alice", "#boson", "") // logged in as alice: not a guest
	now := time.Unix(1000, 0)
	s := ergo.NewErgoService(ergo.Options{URL: f.URL(), Token: "tok", TTL: 10 * time.Second, Now: func() time.Time { return now }})
	ctx := context.Background()
	for _, c := range []struct {
		nick, channel string
		want          bool
	}{
		{"vic", "#BOSON", true},
		{"VIC", "#other", false},
		{"alice", "#boson", false},
		{"carol", "#boson", false},
		{"", "#boson", false},
		{"vic", "", false},
	} {
		if in, err := s.NickIn(ctx, c.nick, c.channel); err != nil || in != c.want {
			t.Errorf("NickIn(%q, %q) = %v %v, want %v", c.nick, c.channel, in, err, c.want)
		}
	}
	// As an account, vic (whose client isn't logged in) has nothing.
	if st, _ := s.Standing(ctx, "vic"); st.In("#boson") || st.Role("#boson") != roles.Member {
		t.Fatalf("account vic: %+v", st)
	}
	calls := f.Calls.Load()
	_, _ = s.NickIn(ctx, "Vic", "#dev")
	if f.Calls.Load() != calls {
		t.Fatal("guest lookup not cached")
	}
	now = now.Add(11 * time.Second)
	f.Part("vic", "#boson")
	if in, _ := s.NickIn(ctx, "vic", "#boson"); in || f.Calls.Load() != calls+1 {
		t.Fatalf("not refetched after TTL: in=%v calls=%d", in, f.Calls.Load()-calls)
	}

	if _, err := ergo.NewErgoService(ergo.Options{}).NickIn(ctx, "vic", "#boson"); !errors.Is(err, ergo.ErrUnavailable) {
		t.Fatalf("disabled: %v", err)
	}
	f.SetDown(true)
	if _, err := ergo.NewErgoService(ergo.Options{URL: f.URL(), Token: "tok"}).NickIn(ctx, "vic", "#boson"); !errors.Is(err, ergo.ErrUnavailable) {
		t.Fatalf("down: %v", err)
	}
}
