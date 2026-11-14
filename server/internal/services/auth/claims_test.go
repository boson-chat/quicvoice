package auth

import (
	"context"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TestNetworkClaims: roles/perms are parsed on both the HS256 and the JWKS
// path, and an absent claim is told apart from an empty one.
func TestNetworkClaims(t *testing.T) {
	ctx := context.Background()
	k := newKey(t, "k1")
	srv := &jwksServer{}
	srv.set(k)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	jw, err := NewJWKSAuthService(ctx, ts.URL, testIssuer, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	both := &CompositeAuthService{HMAC: NewJWTAuthService("s3cret"), JWKS: jw}
	exp := time.Now().Add(time.Minute).Unix()

	mint := map[string]func(jwt.MapClaims) string{
		"hs256": func(c jwt.MapClaims) string {
			s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte("s3cret"))
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		"eddsa": func(c jwt.MapClaims) string {
			tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c)
			tok.Header["kid"] = k.kid
			s, err := tok.SignedString(k.priv)
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	}
	cases := []struct {
		name               string
		extra              jwt.MapClaims
		hasRoles, hasPerms bool
		roles, perms       []string
	}{
		{"old token", jwt.MapClaims{}, false, false, nil, nil},
		{"null claims", jwt.MapClaims{"roles": nil, "perms": nil}, false, false, nil, nil},
		{"empty claims", jwt.MapClaims{"roles": []string{}, "perms": []string{}}, true, true, []string{}, []string{}},
		{"perms only", jwt.MapClaims{"perms": []string{"voice.create"}}, false, true, nil, []string{"voice.create"}},
		{"full", jwt.MapClaims{"roles": []string{"Owner", " DJ ", ""}, "perms": []string{"voice.manage_any", "chat.kick"}},
			true, true, []string{"Owner", "DJ"}, []string{"voice.manage_any", "chat.kick"}},
	}
	for alg, sign := range mint {
		for _, c := range cases {
			mc := jwt.MapClaims{"sub": "usr_1", "name": "alice", "role": "speaker", "exp": exp, "iss": testIssuer, "aud": testAudience}
			for k, v := range c.extra {
				mc[k] = v
			}
			id, err := both.Verify(ctx, sign(mc))
			if err != nil {
				t.Fatalf("%s %s: %v", alg, c.name, err)
			}
			if id.HasRoles != c.hasRoles || id.HasPerms != c.hasPerms || id.HasNetworkPerms() != c.hasPerms ||
				!slices.Equal(id.NetworkRoles, c.roles) || !slices.Equal(id.Perms, c.perms) {
				t.Errorf("%s %s: %+v", alg, c.name, id)
			}
		}
	}
}

func TestIdentityPermHelpers(t *testing.T) {
	id := &Identity{NetworkRoles: []string{"Owner", "DJ"}, Perms: []string{PermVoiceCreate}, HasPerms: true}
	if !id.HasNetworkRole("owner") || !id.HasNetworkRole("dj") || id.HasNetworkRole("Moderator") {
		t.Error("HasNetworkRole should be case-insensitive and exact")
	}
	if !id.HasPerm(PermVoiceCreate) || id.HasPerm(PermVoiceManageAny) || id.HasPerm("VOICE.CREATE") {
		t.Error("HasPerm")
	}
}
