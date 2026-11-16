package videoticket

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// verify checks tok the way quicvideod does: against the JWKS key.
func verify(t *testing.T, s *VideoTicketService, tok string, now time.Time) *Claims {
	t.Helper()
	keys := s.JWKS()["keys"].([]map[string]string)
	x, err := base64.RawURLEncoding.DecodeString(keys[0]["x"])
	if err != nil {
		t.Fatal(err)
	}
	c := &Claims{}
	_, err = jwt.ParseWithClaims(tok, c, func(tk *jwt.Token) (any, error) {
		if tk.Header["kid"] != keys[0]["kid"] {
			t.Fatalf("kid %v, JWKS has %s", tk.Header["kid"], keys[0]["kid"])
		}
		return ed25519.PublicKey(x), nil
	}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithAudience(Audience), jwt.WithIssuer("voice"),
		jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("ticket doesn't verify: %v", err)
	}
	return c
}

func TestIssueVerifiesAgainstJWKS(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s, err := New(Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	tok, exp, err := s.Issue("u1", "Alice", "general", true)
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("exp %v", exp)
	}
	c := verify(t, s, tok, now)
	if c.Subject != "u1" || c.Name != "Alice" || len(c.Rooms) != 1 || c.Rooms[0] != "general" {
		t.Fatalf("claims %+v", c)
	}
	if len(c.Perms) != 1 || c.Perms[0] != PermVideoPublish {
		t.Fatalf("perms %v", c.Perms)
	}

	// A listener gets a ticket that can watch but not publish.
	tok, _, _ = s.Issue("u2", "Bob", "general", false)
	if c := verify(t, s, tok, now); len(c.Perms) != 0 || c.Perms == nil {
		t.Fatalf("listener perms %#v (want an empty list)", c.Perms)
	}

	// Expired tickets fail.
	tok, _, _ = s.Issue("u1", "Alice", "general", true)
	if _, err := jwt.Parse(tok, func(*jwt.Token) (any, error) { return s.key.Public(), nil },
		jwt.WithTimeFunc(func() time.Time { return now.Add(6 * time.Minute) })); err == nil {
		t.Fatal("expired ticket verified")
	}
}

func TestKeyFilePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "video-ticket.key")
	a, err := New(Options{KeyFile: path})
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode().Perm())
	}
	b, err := New(Options{KeyFile: path})
	if err != nil {
		t.Fatal(err)
	}
	if a.kid != b.kid {
		t.Fatal("restart changed the key")
	}
	tok, _, _ := a.Issue("u1", "A", "r", true)
	verify(t, b, tok, time.Now())

	if err := os.WriteFile(path, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{KeyFile: path}); err == nil {
		t.Fatal("junk key file accepted")
	}
}
