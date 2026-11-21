package jwks

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type testKey struct {
	kid  string
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func newKey(t *testing.T, kid string) testKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{kid: kid, priv: priv, pub: pub}
}

func (k testKey) sign(t *testing.T) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"sub": "usr_test",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = k.kid
	s, err := tok.SignedString(k.priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// jwksServer serves the keys it currently holds and counts requests.
type jwksServer struct {
	mu   sync.Mutex
	keys []testKey
	hits atomic.Int64
}

func (s *jwksServer) set(keys ...testKey) {
	s.mu.Lock()
	s.keys = keys
	s.mu.Unlock()
}

func (s *jwksServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.hits.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]string{}
	for _, k := range s.keys {
		out = append(out, map[string]string{
			"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig",
			"kid": k.kid, "x": base64.RawURLEncoding.EncodeToString(k.pub),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": out})
}

func verify(c *Cache, tok string) error {
	_, err := jwt.Parse(tok, c.Keyfunc, jwt.WithValidMethods([]string{"EdDSA"}))
	return err
}

// eventually polls f until it returns nil or within elapses.
func eventually(t *testing.T, within time.Duration, f func() error) time.Duration {
	t.Helper()
	start := time.Now()
	var err error
	for time.Since(start) < within {
		if err = f(); err == nil {
			return time.Since(start)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("not within %s: %v", within, err)
	return 0
}

// The incident: the service starts while auth isn't listening yet. Once
// auth comes up, a valid token must verify within a few seconds (with the
// production defaults), not after keyfunc's 5-minute / 1-hour refresh.
// "traffic": tokens keep arriving while auth is down, so the unknown-kid
// budget is spent on failed fetches and the background retry must load the
// keys; "quiet": nothing arrives until auth is up, and the first token's
// unknown-kid fetch (or the retry) loads them.
func TestStartsWithJWKSDown(t *testing.T) {
	for _, traffic := range []bool{true, false} {
		name := map[bool]string{true: "traffic", false: "quiet"}[traffic]
		t.Run(name, func(t *testing.T) { testStartsWithJWKSDown(t, traffic) })
	}
}

func testStartsWithJWKSDown(t *testing.T, traffic bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Reserve a port, then free it: connection refused until we listen.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	start := time.Now()
	c, err := New(ctx, []string{"http://" + addr + "/.well-known/jwks.json"}, Options{})
	if err != nil {
		t.Fatalf("New must not fail with the JWKS down: %v", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("New blocked for %s", d)
	}
	if c.Ready() {
		t.Fatal("Ready before the keys loaded")
	}
	k := newKey(t, "k1")
	tok := k.sign(t)
	upAt := make(chan struct{})
	var downOK atomic.Bool
	if traffic {
		go func() {
			for {
				select {
				case <-upAt:
					return
				default:
				}
				if verify(c, tok) == nil {
					downOK.Store(true)
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}

	time.Sleep(time.Second)
	srv := &jwksServer{}
	srv.set(k)
	ln, err = net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("relisten %s: %v", addr, err)
	}
	close(upAt)
	hs := &http.Server{Handler: srv}
	go func() { _ = hs.Serve(ln) }()
	defer hs.Close()
	if downOK.Load() {
		t.Fatal("a token verified while the JWKS was down")
	}

	took := eventually(t, 5*time.Second, func() error { return verify(c, tok) })
	t.Logf("verified %s after the JWKS came up", took)
	if !c.Ready() {
		t.Fatal("not Ready after the keys loaded")
	}
}

// A new kid is fetched straight away; a second rotation inside the rate
// limit is picked up once the limit allows (well under keyfunc's 5 min).
func TestKeyRotation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &jwksServer{}
	k1, k2, k3 := newKey(t, "k1"), newKey(t, "k2"), newKey(t, "k3")
	srv.set(k1)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	c, err := New(ctx, []string{ts.URL}, Options{UnknownKIDEvery: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 2*time.Second, func() error { return verify(c, k1.sign(t)) })

	srv.set(k1, k2)
	if err := verify(c, k2.sign(t)); err != nil {
		t.Fatalf("new kid not fetched promptly: %v", err)
	}

	srv.set(k2, k3)
	if err := verify(c, k3.sign(t)); err == nil {
		t.Fatal("refetched inside the unknown-kid rate limit")
	}
	took := eventually(t, 2*time.Second, func() error { return verify(c, k3.sign(t)) })
	if took > time.Second {
		t.Fatalf("rotation took %s with a 500ms limit", took)
	}
	// k1 left the set: its tokens no longer verify.
	if err := verify(c, k1.sign(t)); err == nil {
		t.Fatal("retired key still verifies")
	}
}

// Tokens arriving together with a new kid all verify: one fetches, the rest
// wait for it instead of failing on the rate limit.
func TestConcurrentNewKID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &jwksServer{}
	k1, k2 := newKey(t, "k1"), newKey(t, "k2")
	srv.set(k1)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c, err := New(ctx, []string{ts.URL}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 2*time.Second, func() error { return verify(c, k1.sign(t)) })

	srv.set(k1, k2)
	tok := k2.sign(t)
	var wg sync.WaitGroup
	var failed atomic.Int64
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := verify(c, tok); err != nil {
				failed.Add(1)
				t.Log(err)
			}
		}()
	}
	wg.Wait()
	if n := failed.Load(); n > 0 {
		t.Fatalf("%d of 20 concurrent tokens failed", n)
	}
}

// Unknown kids can't make the cache hammer the issuer.
func TestUnknownKIDRateLimited(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &jwksServer{}
	k1 := newKey(t, "k1")
	srv.set(k1)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c, err := New(ctx, []string{ts.URL}, Options{Settle: -1})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 2*time.Second, func() error { return verify(c, k1.sign(t)) })
	before := srv.hits.Load()
	bogus := newKey(t, "nope")
	for range 50 {
		if verify(c, bogus.sign(t)) == nil {
			t.Fatal("unknown key verified")
		}
	}
	if n := srv.hits.Load() - before; n > 1 {
		t.Fatalf("%d refetches for 50 unknown-kid tokens; want at most 1 per 10s", n)
	}
}

// An empty set (issuer up but without keys yet) keeps retrying.
func TestEmptySetNotReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &jwksServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c, err := New(ctx, []string{ts.URL}, Options{MinBackoff: 50 * time.Millisecond, MaxBackoff: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if c.Ready() {
		t.Fatal("Ready with an empty JWK Set")
	}
	k := newKey(t, "k1")
	srv.set(k)
	eventually(t, time.Second, func() error {
		if !c.Ready() {
			return context.DeadlineExceeded
		}
		return nil
	})
}

// Several URLs (e.g. auth + quicvoiced): keys are matched across them and
// Ready waits for all.
func TestMultipleURLs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := &jwksServer{}, &jwksServer{}
	ka, kb := newKey(t, "a"), newKey(t, "b")
	a.set(ka)
	b.set(kb)
	ta, tb := httptest.NewServer(a), httptest.NewServer(b)
	defer ta.Close()
	defer tb.Close()
	c, err := New(ctx, []string{ta.URL, tb.URL}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 2*time.Second, func() error { return verify(c, ka.sign(t)) })
	eventually(t, 2*time.Second, func() error { return verify(c, kb.sign(t)) })
	if !c.Ready() {
		t.Fatal("not Ready")
	}
}

func TestBadURL(t *testing.T) {
	if _, err := New(context.Background(), nil, Options{}); err == nil {
		t.Fatal("no URLs accepted")
	}
	if _, err := New(context.Background(), []string{"not a url"}, Options{}); err == nil {
		t.Fatal("bad URL accepted")
	}
}

// A token racing the first load (its fetch is the one that loads the keys)
// doesn't use up the unknown-kid budget: a rotation right after still works.
func TestFirstLoadIsFree(t *testing.T) {
	for range 20 {
		ctx, cancel := context.WithCancel(context.Background())
		srv := &jwksServer{}
		k1, k2 := newKey(t, "k1"), newKey(t, "k2")
		srv.set(k1)
		ts := httptest.NewServer(srv)
		c, err := New(ctx, []string{ts.URL}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := verify(c, k1.sign(t)); err != nil {
			t.Fatalf("first token: %v", err)
		}
		srv.set(k1, k2)
		if err := verify(c, k2.sign(t)); err != nil {
			t.Fatalf("rotation right after the first load: %v", err)
		}
		cancel()
		ts.Close()
	}
}
