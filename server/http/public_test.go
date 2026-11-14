package http_test

import (
	"context"
	"encoding/json"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	qvhttp "github.com/boson-chat/quicvoice/server/http"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/metrics"
	"github.com/golang-jwt/jwt/v5"
)

const secret = "test-secret"

type nopPeer struct{}

func (nopPeer) SendDatagram([]byte) error                   { return nil }
func (nopPeer) SendControl(*controlpb.ControlMessage) error { return nil }

func token(t *testing.T, sub string, ttl time.Duration, rooms ...string) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, auth.Claims{
		Name: sub, Rooms: rooms,
		RegisteredClaims: jwt.RegisteredClaims{Subject: sub, ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl))},
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func get(t *testing.T, url, authz string) (*nethttp.Response, string) {
	t.Helper()
	req, _ := nethttp.NewRequest(nethttp.MethodGet, url, nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := nethttp.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestPublicRooms(t *testing.T) {
	ctx := context.Background()
	rs := room.NewRoomService(10, metrics.Noop(),
		room.WithRooms([]room.Def{{ID: "general", Name: "General"}, {ID: "gaming", Name: "Gaming"}}, true))
	_, _, _ = rs.Join(ctx, "general", &auth.Identity{UserID: "usr_1", DisplayName: "alice", Role: controlpb.Role_SPEAKER}, nopPeer{})
	srv := httptest.NewServer(qvhttp.NewPublicRouter(auth.NewJWTAuthService(secret), rs))
	defer srv.Close()

	t.Run("unauthorized", func(t *testing.T) {
		for name, authz := range map[string]string{
			"missing": "",
			"scheme":  "Basic " + token(t, "u", time.Minute),
			"empty":   "Bearer ",
			"invalid": "Bearer not-a-jwt",
			"expired": "Bearer " + token(t, "u", -time.Minute),
		} {
			resp, body := get(t, srv.URL+"/v1/rooms", authz)
			if resp.StatusCode != nethttp.StatusUnauthorized {
				t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
			}
			if resp.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
				t.Errorf("%s: headers %v", name, resp.Header)
			}
			if !strings.Contains(body, `"error"`) {
				t.Errorf("%s: body %q", name, body)
			}
		}
	})

	t.Run("ok", func(t *testing.T) {
		resp, body := get(t, srv.URL+"/v1/rooms", "Bearer "+token(t, "u", time.Minute))
		if resp.StatusCode != nethttp.StatusOK {
			t.Fatalf("status %d: %s", resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type %q", ct)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control %q", cc)
		}
		want := `{"rooms":[{"id":"general","name":"General","participants":[{"session_id":1,"user_id":"usr_1","name":"alice","muted":false,"deafened":false,"speaking":false}]},{"id":"gaming","name":"Gaming","participants":[]}]}`
		if strings.TrimSpace(body) != want {
			t.Errorf("body\n got %s\nwant %s", body, want)
		}
	})

	t.Run("rooms claim filters", func(t *testing.T) {
		_, body := get(t, srv.URL+"/v1/rooms", "bearer "+token(t, "u", time.Minute, "gaming"))
		var got struct {
			Rooms []struct {
				ID string `json:"id"`
			} `json:"rooms"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Rooms) != 1 || got.Rooms[0].ID != "gaming" {
			t.Fatalf("rooms = %s", body)
		}
	})

	t.Run("no metrics on public listener", func(t *testing.T) {
		if resp, _ := get(t, srv.URL+"/metrics", ""); resp.StatusCode != nethttp.StatusNotFound {
			t.Fatalf("/metrics status %d, want 404", resp.StatusCode)
		}
		if resp, body := get(t, srv.URL+"/healthz", ""); resp.StatusCode != nethttp.StatusOK || body != "ok" {
			t.Fatalf("/healthz %d %q", resp.StatusCode, body)
		}
	})
}
