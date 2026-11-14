package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/client"
	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/boson-chat/quicvoice/server/internal/services/ergo"
	"github.com/boson-chat/quicvoice/server/internal/services/ergo/ergotest"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/services/settings"
	"github.com/boson-chat/quicvoice/server/internal/services/voicechannel"
	"github.com/boson-chat/quicvoice/wire"
)

func api(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

// TestDynamicRoomOverQUIC: a founder creates a role-locked room through the
// public API, real QUIC clients join it (one downgraded to listener, one
// refused), then the room is deleted and everyone in it gets KICKED.
func TestDynamicRoomOverQUIC(t *testing.T) {
	fe := ergotest.New(t, "etok")
	fe.Register("#boson", "fred")
	fe.Join("vic", "#boson", "v")
	ts := startServerWith(t, serverSetup{channels: &voicechannel.Options{
		Static:   []room.Def{{ID: "general", Name: "General"}},
		Settings: settings.NewSettingsService(settings.Options{Fallback: settings.CreateFounders}),
		Ergo:     ergo.NewErgoService(ergo.Options{URL: fe.URL(), Token: "etok", TTL: time.Nanosecond}),
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// AUTH_INSECURE tokens: the token is the account name.
	if code, body := api(t, "POST", ts.publicURL+"/v1/rooms", "mia", `{"name":"Stage","parent":"#boson"}`); code != 403 {
		t.Fatalf("non-founder create: %d %s", code, body)
	}
	code, body := api(t, "POST", ts.publicURL+"/v1/rooms", "fred", `{"name":"Stage","parent":"#boson","min_role":"voice","speak_role":"op"}`)
	if code != 201 || !strings.Contains(body, `"id":"boson-stage"`) {
		t.Fatalf("create: %d %s", code, body)
	}

	kicked := make(chan string, 2)
	dial := func(name string, onAudio func(wire.Downlink)) *client.Client {
		c, err := client.Dial(ctx, client.Config{Addr: ts.addr, Token: name, InsecureSkipVerify: true, OnAudio: onAudio,
			OnControl: func(m *controlpb.ControlMessage) {
				if e := m.GetError(); e != nil && e.Code == controlpb.Error_KICKED {
					kicked <- name + ": " + e.Message
				}
			}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	got := make(chan uint16, 64)
	fred := dial("fred", func(d wire.Downlink) { got <- d.SessionID })
	vic := dial("vic", nil)
	mia := dial("mia", nil)

	if _, err := mia.Join(ctx, "boson-stage"); err == nil || !strings.Contains(err.Error(), "FORBIDDEN") || !strings.Contains(err.Error(), "voice") {
		t.Fatalf("mia join: %v", err)
	}
	if _, err := fred.Join(ctx, "boson-stage"); err != nil {
		t.Fatal(err)
	}
	vs, err := vic.Join(ctx, "boson-stage")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range vs.Participants {
		if p.SessionId == vs.YourSessionId && p.Role != controlpb.Role_LISTENER {
			t.Fatalf("vic role %v, want LISTENER (speak_role op)", p.Role)
		}
	}
	// vic is a listener: his audio never reaches fred.
	for i := range 10 {
		_ = vic.SendAudio(&wire.Uplink{Audio: wire.Audio{Voice: true, Seq: uint16(i), Payload: []byte("x")}})
	}
	time.Sleep(100 * time.Millisecond)
	if len(got) != 0 {
		t.Fatal("listener audio was forwarded")
	}

	if code, body := api(t, "DELETE", ts.publicURL+"/v1/rooms/boson-stage", "vic", ""); code != 403 {
		t.Fatalf("vic delete: %d %s", code, body)
	}
	if code, body := api(t, "DELETE", ts.publicURL+"/v1/rooms/boson-stage", "fred", ""); code != 204 {
		t.Fatalf("delete: %d %s", code, body)
	}
	for range 2 {
		select {
		case k := <-kicked:
			if !strings.Contains(k, "deleted") {
				t.Fatalf("kick message %q", k)
			}
		case <-ctx.Done():
			t.Fatal("no KICKED")
		}
	}
	// Kicked clients can join another room on the same connection.
	if _, err := fred.Join(ctx, "general"); err != nil {
		t.Fatalf("join after kick: %v", err)
	}
	if _, err := vic.Join(ctx, "boson-stage"); err == nil || !strings.Contains(err.Error(), "ROOM_NOT_FOUND") {
		t.Fatalf("join deleted room: %v", err)
	}

	fams := scrape(t, ts.metricsURL)
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"quicvoice_room_changes_total", map[string]string{"op": "create", "outcome": "ok"}, 1},
		{"quicvoice_room_changes_total", map[string]string{"op": "create", "outcome": "forbidden"}, 1},
		{"quicvoice_room_changes_total", map[string]string{"op": "delete", "outcome": "ok"}, 1},
		{"quicvoice_room_leaves_total", map[string]string{"reason": "kicked"}, 2},
		{"quicvoice_rooms_defined", map[string]string{"kind": "static"}, 1},
		{"quicvoice_rooms_defined", map[string]string{"kind": "dynamic"}, 0},
	} {
		if v, ok := sample(fams, c.name, c.labels); !ok || v != c.want {
			t.Errorf("%s%v = %v (found %v), want %v", c.name, c.labels, v, ok, c.want)
		}
	}

	var list struct {
		Rooms []struct{ ID string } `json:"rooms"`
	}
	_, body = api(t, "GET", ts.publicURL+"/v1/rooms", "fred", "")
	_ = json.Unmarshal([]byte(body), &list)
	if len(list.Rooms) != 1 || list.Rooms[0].ID != "general" {
		t.Fatalf("rooms after delete: %s", body)
	}
}
