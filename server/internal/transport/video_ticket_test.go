package transport_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/client"
	"github.com/boson-chat/quicvoice/server/internal/services/videoticket"
	"github.com/golang-jwt/jwt/v5"
)

// TestVideoTicketOverQUIC: a session gets a video ticket only while in a
// voice channel; the ticket names that channel and verifies against the
// JWKS on the public API, the way quicvideod checks it.
func TestVideoTicketOverQUIC(t *testing.T) {
	tickets, err := videoticket.New(videoticket.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ts := startServerWith(t, serverSetup{tickets: tickets})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cl, err := client.Dial(ctx, client.Config{Addr: ts.addr, Token: "alice", InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	if _, err := cl.VideoTicket(ctx); err == nil || !strings.Contains(err.Error(), "join a voice channel first") {
		t.Fatalf("ticket outside a channel: %v", err)
	}
	if _, err := cl.Join(ctx, "general"); err != nil {
		t.Fatal(err)
	}
	vt, err := cl.VideoTicket(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if vt.GetRoom() != "general" || vt.GetQuicUrl() != "quic://video.test:7710" || vt.GetWebtransportUrl() != "https://video.test:7710/wt" {
		t.Fatalf("ticket %+v", vt)
	}
	if d := time.Until(time.Unix(vt.GetExpiresAt(), 0)); d < 4*time.Minute || d > 6*time.Minute {
		t.Fatalf("expires in %v", d)
	}

	resp, err := http.Get(ts.publicURL + "/.well-known/video-jwks.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var set struct {
		Keys []struct{ Kid, Crv, X string }
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil || len(set.Keys) != 1 || set.Keys[0].Crv != "Ed25519" {
		t.Fatalf("jwks %+v %v", set, err)
	}
	x, _ := base64.RawURLEncoding.DecodeString(set.Keys[0].X)
	c := &videoticket.Claims{}
	if _, err := jwt.ParseWithClaims(vt.GetToken(), c, func(tk *jwt.Token) (any, error) {
		if tk.Header["kid"] != set.Keys[0].Kid {
			t.Fatalf("kid %v", tk.Header["kid"])
		}
		return ed25519.PublicKey(x), nil
	}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithAudience("quicvideo"), jwt.WithIssuer("voice")); err != nil {
		t.Fatalf("ticket doesn't verify against the JWKS: %v", err)
	}
	if c.Subject != "alice" || len(c.Rooms) != 1 || c.Rooms[0] != "general" || len(c.Perms) != 1 || c.Perms[0] != "video.publish" {
		t.Fatalf("claims %+v", c)
	}

	// After leaving, no more tickets.
	if err := cl.Leave(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := cl.VideoTicket(ctx); err == nil {
		t.Fatal("ticket after leaving")
	}
}

// TestVideoTicketDisabled: without VIDEO_QUIC_URL there are no tickets
// and no JWKS.
func TestVideoTicketDisabled(t *testing.T) {
	ts := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cl, err := client.Dial(ctx, client.Config{Addr: ts.addr, Token: "alice", InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if _, err := cl.Join(ctx, "general"); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.VideoTicket(ctx); err == nil || !strings.Contains(err.Error(), "isn't enabled") {
		t.Fatalf("ticket with video off: %v", err)
	}
	resp, err := http.Get(ts.publicURL + "/.well-known/video-jwks.json")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("jwks with video off: %d", resp.StatusCode)
	}
}
