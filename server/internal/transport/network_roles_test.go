package transport_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/client"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/services/settings"
	"github.com/boson-chat/quicvoice/server/internal/services/voicechannel"
	"github.com/golang-jwt/jwt/v5"
)

// TestAllowedRolesOverQUIC: a voice.manage_any user creates a room locked to
// network roles; QUIC joins are admitted by the token's roles claim (or
// voice.bypass_locks) and refused otherwise, including old tokens.
func TestAllowedRolesOverQUIC(t *testing.T) {
	const secret = "s3cret"
	ts := startServerWith(t, serverSetup{
		auth: auth.NewJWTAuthService(secret),
		channels: &voicechannel.Options{
			Static:   []room.Def{{ID: "general", Name: "General"}},
			Settings: settings.NewSettingsService(settings.Options{Fallback: settings.CreateAdmins}),
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mint := func(sub string, netRoles, perms []string) string {
		c := auth.Claims{Name: sub, RegisteredClaims: jwt.RegisteredClaims{Subject: "usr_" + sub, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))}}
		if netRoles != nil {
			c.Roles = &netRoles
		}
		if perms != nil {
			c.Perms = &perms
		}
		s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(secret))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	none := []string{}
	mgr := mint("mgr", none, []string{auth.PermVoiceManageAny})
	if code, body := api(t, "POST", ts.publicURL+"/v1/rooms", mint("cara", none, []string{auth.PermVoiceCreate}), `{"name":"Booth"}`); code != 403 {
		t.Fatalf("voice.create under admins policy: %d %s", code, body)
	}
	if code, body := api(t, "POST", ts.publicURL+"/v1/rooms", mgr, `{"name":"Booth","allowed_roles":["DJ"]}`); code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	for name, c := range map[string]struct {
		tok string
		ok  bool
	}{
		"dj":        {mint("dan", []string{"dj"}, none), true},
		"bypass":    {mint("bea", none, []string{auth.PermVoiceBypassLocks}), true},
		"other":     {mint("ola", []string{"Member"}, none), false},
		"manageAny": {mgr, false},
		"oldToken":  {mint("old", nil, nil), false},
	} {
		cl, err := client.Dial(ctx, client.Config{Addr: ts.addr, Token: c.tok, InsecureSkipVerify: true})
		if err != nil {
			t.Fatalf("%s: dial: %v", name, err)
		}
		_, err = cl.Join(ctx, "booth")
		if (err == nil) != c.ok {
			t.Errorf("%s: join err = %v, want ok=%v", name, err, c.ok)
		}
		if err != nil && (!strings.Contains(err.Error(), "FORBIDDEN") || !strings.Contains(err.Error(), "DJ")) {
			t.Errorf("%s: err %v", name, err)
		}
		_ = cl.Close()
	}
}
