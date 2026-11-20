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
	"github.com/boson-chat/quicvoice/server/internal/services/videoticket"
	"github.com/boson-chat/quicvoice/server/internal/services/voicechannel"
	"github.com/golang-jwt/jwt/v5"
)

// TestPrivateCallOverQUIC: a private call room ("call:<id>") on a strict
// server with a registry is joined by the two people whose tokens name it,
// gets video tickets like a channel, and stays out of GET /v1/rooms. Anyone
// else, even with an any-room token, is refused.
func TestPrivateCallOverQUIC(t *testing.T) {
	const secret = "s3cret"
	const callRoom = "call:01JPRIVATECALL"
	tickets, err := videoticket.New(videoticket.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ts := startServerWith(t, serverSetup{
		auth:    auth.NewJWTAuthService(secret),
		tickets: tickets,
		channels: &voicechannel.Options{
			Static:   []room.Def{{ID: "general", Name: "General"}},
			Settings: settings.NewSettingsService(settings.Options{Fallback: settings.CreateAdmins}),
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mint := func(sub string, rooms ...string) string {
		none := []string{}
		c := auth.Claims{Name: sub, Rooms: rooms, Roles: &none, Perms: &none,
			RegisteredClaims: jwt.RegisteredClaims{Subject: "usr_" + sub, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))}}
		s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(secret))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	dial := func(tok string) *client.Client {
		cl, err := client.Dial(ctx, client.Config{Addr: ts.addr, Token: tok, InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cl.Close() })
		return cl
	}

	for name, tok := range map[string]string{
		"any room":    mint("mallory"),
		"other rooms": mint("mallory", "general", "call:other"),
	} {
		if _, err := dial(tok).Join(ctx, callRoom); err == nil {
			t.Fatalf("%s token joined the private call", name)
		}
	}

	alice, bob := dial(mint("alice", callRoom)), dial(mint("bob", callRoom))
	if _, err := alice.Join(ctx, callRoom); err != nil {
		t.Fatalf("alice: %v", err)
	}
	st, err := bob.Join(ctx, callRoom)
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	if len(st.GetParticipants()) != 2 {
		t.Fatalf("participants %v", st.GetParticipants())
	}
	vt, err := bob.VideoTicket(ctx)
	if err != nil {
		t.Fatalf("video ticket in a private call: %v", err)
	}
	if vt.GetRoom() != callRoom {
		t.Fatalf("ticket room %q", vt.GetRoom())
	}

	// Unlisted, even for the people on the call.
	for _, tok := range []string{mint("alice", callRoom), mint("carol")} {
		code, body := api(t, "GET", ts.publicURL+"/v1/rooms", tok, "")
		if code != 200 {
			t.Fatalf("list: %d %s", code, body)
		}
		if strings.Contains(body, "call:") {
			t.Fatalf("private call listed: %s", body)
		}
	}
}
