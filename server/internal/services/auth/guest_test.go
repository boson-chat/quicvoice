package auth

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/golang-jwt/jwt/v5"
)

// TestGuestClaims: guest tokens on both the HS256 and the JWKS path. A guest
// is never an Ergo account and never holds roles, perms or staff role,
// whatever the token says; guest:<id> subjects need the guest claim and the
// guest claim needs a guest:<id> subject.
func TestGuestClaims(t *testing.T) {
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
	sign := map[string]func(jwt.MapClaims) string{
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
	base := func(extra jwt.MapClaims) jwt.MapClaims {
		mc := jwt.MapClaims{"sub": "guest:abcdef0123456789", "name": "Wanderer", "guest": true, "role": "speaker",
			"roles": []string{}, "perms": []string{}, "exp": exp, "iss": testIssuer, "aud": testAudience}
		for k, v := range extra {
			mc[k] = v
		}
		return mc
	}
	for alg, s := range sign {
		ok := []struct {
			name  string
			extra jwt.MapClaims
			role  controlpb.Role
			rooms int
		}{
			{"contract token", nil, controlpb.Role_SPEAKER, 0},
			{"rooms", jwt.MapClaims{"rooms": []string{"general"}}, controlpb.Role_SPEAKER, 1},
			{"listener", jwt.MapClaims{"role": "listener"}, controlpb.Role_LISTENER, 0},
			{"moderator capped", jwt.MapClaims{"role": "moderator"}, controlpb.Role_SPEAKER, 0},
			{"admin + perms ignored", jwt.MapClaims{"role": "admin", "roles": []string{"Owner"},
				"perms": []string{PermVoiceManageAny, PermVoiceBypassLocks, PermVoiceCreate}}, controlpb.Role_SPEAKER, 0},
			{"no roles/perms claims", jwt.MapClaims{"roles": nil, "perms": nil}, controlpb.Role_SPEAKER, 0},
		}
		for _, c := range ok {
			id, err := both.Verify(ctx, s(base(c.extra)))
			if err != nil {
				t.Fatalf("%s %s: %v", alg, c.name, err)
			}
			if !id.Guest || id.Account != "" || id.UserID != "guest:abcdef0123456789" || id.DisplayName != "Wanderer" ||
				id.Role != c.role || id.IsStaff() || len(id.Rooms) != c.rooms ||
				!id.HasPerms || len(id.Perms) != 0 || !id.HasRoles || len(id.NetworkRoles) != 0 ||
				id.HasPerm(PermVoiceManageAny) || id.HasNetworkRole("Owner") {
				t.Errorf("%s %s: %+v", alg, c.name, id)
			}
		}
		bad := map[string]jwt.MapClaims{
			"guest sub without prefix":  {"sub": "usr_1"},
			"guest sub is only prefix":  {"sub": "guest:"},
			"guest prefix, no claim":    {"guest": false},
			"guest prefix, claim unset": {"guest": nil},
		}
		for name, extra := range bad {
			mc := base(extra)
			if v, ok := extra["guest"]; ok && v == nil {
				delete(mc, "guest")
			}
			if _, err := both.Verify(ctx, s(mc)); !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("%s %s: err = %v, want unauthenticated", alg, name, err)
			}
		}
	}
	// Name falls back to the subject, as for accounts; still no account.
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{Guest: true,
		RegisteredClaims: jwt.RegisteredClaims{Subject: "guest:x", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))}}).
		SignedString([]byte("s3cret"))
	if id, err := NewJWTAuthService("s3cret").Verify(ctx, tok); err != nil || id.DisplayName != "guest:x" || id.Account != "" {
		t.Fatalf("no name: %+v %v", id, err)
	}
}

func TestGuestIsNeverStaff(t *testing.T) {
	if (&Identity{Guest: true, Role: controlpb.Role_ADMIN}).IsStaff() {
		t.Fatal("guest with admin role is staff")
	}
}
