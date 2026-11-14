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
