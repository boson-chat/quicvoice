package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/golang-jwt/jwt/v5"
)

type testKey struct {
	kid  string
	priv ed25519.PrivateKey
}

func newKey(t *testing.T, kid string) testKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{kid: kid, priv: priv}
}

// jwksServer serves whatever keys are currently set.
type jwksServer struct {
	mu   sync.Mutex
	keys []testKey
	hits int
}

func (s *jwksServer) set(keys ...testKey) {
	s.mu.Lock()
	s.keys = keys
	s.mu.Unlock()
}

func (s *jwksServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits++
	type jwk struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		X   string `json:"x"`
		Kid string `json:"kid"`
		Alg string `json:"alg"`
		Use string `json:"use"`
	}
	var out struct {
		Keys []jwk `json:"keys"`
	}
	for _, k := range s.keys {
		pub := k.priv.Public().(ed25519.PublicKey)
		out.Keys = append(out.Keys, jwk{"OKP", "Ed25519", base64.RawURLEncoding.EncodeToString(pub), k.kid, "EdDSA", "sig"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

const (
	testIssuer   = "https://auth.example.test"
	testAudience = "quicvoice"
)

func signEd(t *testing.T, k testKey, c Claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c)
	tok.Header["kid"] = k.kid
	s, err := tok.SignedString(k.priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func claims(sub, aud, iss string, exp time.Time) Claims {
	return Claims{
		Name: "Alice", Role: "moderator",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: sub, Issuer: iss, Audience: jwt.ClaimStrings{aud}, ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
}

func TestJWKSVerify(t *testing.T) {
	k1 := newKey(t, "k1")
	srv := &jwksServer{}
	srv.set(k1)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, err := NewJWKSAuthService(ctx, ts.URL, testIssuer, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(5 * time.Minute)

	id, err := svc.Verify(ctx, signEd(t, k1, claims("usr_1", testAudience, testIssuer, exp)))
	if err != nil {
		t.Fatal(err)
	}
	if id.UserID != "usr_1" || id.DisplayName != "Alice" || id.Role != controlpb.Role_MODERATOR {
		t.Fatalf("identity: %+v", id)
	}

	cases := map[string]string{
		"wrong audience": signEd(t, k1, claims("usr_1", "boson", testIssuer, exp)),
		"wrong issuer":   signEd(t, k1, claims("usr_1", testAudience, "https://evil.test", exp)),
		"forged key":     signEd(t, newKey(t, "k1"), claims("usr_1", testAudience, testIssuer, exp)),
		"no subject":     signEd(t, k1, claims("", testAudience, testIssuer, exp)),
	}
	for name, tok := range cases {
		if _, err := svc.Verify(ctx, tok); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: got %v, want ErrUnauthenticated", name, err)
		}
	}
	if _, err := svc.Verify(ctx, signEd(t, k1, claims("usr_1", testAudience, testIssuer, time.Now().Add(-time.Hour)))); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("expired: got %v", err)
	}

	// Rotation: a token signed with a brand-new kid triggers a JWKS refetch.
	k2 := newKey(t, "k2")
	srv.set(k1, k2)
	if _, err := svc.Verify(ctx, signEd(t, k2, claims("usr_2", testAudience, testIssuer, exp))); err != nil {
		t.Fatalf("rotated key not picked up: %v", err)
	}
}

func TestCompositeRoutesByAlg(t *testing.T) {
	k := newKey(t, "k1")
	srv := &jwksServer{}
	srv.set(k)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx := context.Background()
	jw, err := NewJWKSAuthService(ctx, ts.URL, testIssuer, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Minute)
	hs := sign(t, "s3cret", Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "legacy", ExpiresAt: jwt.NewNumericDate(exp)}})
	ed := signEd(t, k, claims("usr_1", testAudience, testIssuer, exp))

	both := &CompositeAuthService{HMAC: NewJWTAuthService("s3cret"), JWKS: jw}
	for name, tok := range map[string]string{"hs256": hs, "eddsa": ed} {
		if _, err := both.Verify(ctx, tok); err != nil {
			t.Errorf("%s via composite: %v", name, err)
		}
	}

	jwksOnly := &CompositeAuthService{JWKS: jw}
	if _, err := jwksOnly.Verify(ctx, hs); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("HS256 accepted with no secret configured: %v", err)
	}
	if _, err := jwksOnly.Verify(ctx, "not-a-jwt"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("garbage token: %v", err)
	}
}
