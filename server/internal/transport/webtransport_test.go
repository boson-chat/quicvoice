package transport_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/client"
	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/boson-chat/quicvoice/server/internal/origins"
	"github.com/boson-chat/quicvoice/server/internal/transport"
	"github.com/boson-chat/quicvoice/wire"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

// wtClient speaks quicvoice over a WebTransport session, as browsers do.
type wtClient struct {
	sess    *webtransport.Session
	ctrl    *webtransport.Stream
	rd      *wire.ControlReader
	welcome *controlpb.Welcome
}

func wtDial(ctx context.Context, addr, origin string) (*http.Response, *webtransport.Session, error) {
	d := &webtransport.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	return d.Dial(ctx, fmt.Sprintf("https://%s%s", addr, transport.WebTransportPath), h)
}

func dialWT(t *testing.T, addr, token, origin string) *wtClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, sess, err := wtDial(ctx, addr, origin)
	if err != nil {
		t.Fatalf("webtransport dial: %v", err)
	}
	t.Cleanup(func() { _ = sess.CloseWithError(0, "") })
	ctrl, err := sess.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := &wtClient{sess: sess, ctrl: ctrl, rd: wire.NewControlReader(ctrl)}
	c.send(t, &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Hello{Hello: &controlpb.Hello{Token: token, ClientVersion: "wt-test"}}})
	m := c.next(t, func(m *controlpb.ControlMessage) bool { return m.GetWelcome() != nil || m.GetError() != nil })
	if m.GetError() != nil {
		t.Fatalf("hello over WebTransport: %v", m.GetError())
	}
	c.welcome = m.GetWelcome()
	return c
}

func (c *wtClient) send(t *testing.T, m *controlpb.ControlMessage) {
	t.Helper()
	if err := wire.WriteControl(c.ctrl, m); err != nil {
		t.Fatal(err)
	}
}

func (c *wtClient) next(t *testing.T, pred func(*controlpb.ControlMessage) bool) *controlpb.ControlMessage {
	t.Helper()
	_ = c.ctrl.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		m, err := c.rd.Read()
		if err != nil {
			t.Fatalf("control read: %v", err)
		}
		if pred(m) {
			return m
		}
	}
}

func (c *wtClient) join(t *testing.T, roomID string) *controlpb.RoomState {
	t.Helper()
	c.send(t, &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Join{Join: &controlpb.Join{Room: roomID}}})
	return c.next(t, func(m *controlpb.ControlMessage) bool { return m.GetRoomState() != nil }).GetRoomState()
}

// A browser-style WebTransport client and a native QUIC client in one room:
// audio datagrams flow both ways, pings are echoed, Welcome caps the
// payload for WebTransport.
func TestWebTransportSession(t *testing.T) {
	ts := startServerWith(t, serverSetup{maxPayload: 1200})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var nativeGot atomic.Int32
	var nativeFrom atomic.Int32
	native, err := client.Dial(ctx, client.Config{Addr: ts.addr, Token: "native", InsecureSkipVerify: true,
		OnAudio: func(d wire.Downlink) {
			if string(d.Payload) == "from-web" {
				nativeFrom.Store(int32(d.SessionID))
				nativeGot.Add(1)
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = native.Close() })
	if _, err := native.Join(ctx, "room"); err != nil {
		t.Fatal(err)
	}

	web := dialWT(t, ts.addr, "web", "https://chat.example.test")
	if got := web.welcome.GetMaxDatagramPayload(); got != 1000 {
		t.Fatalf("WebTransport max_datagram_payload = %d, want 1000 (capped)", got)
	}
	st := web.join(t, "room")
	if len(st.GetParticipants()) != 2 {
		t.Fatalf("web sees %d participants, want 2", len(st.GetParticipants()))
	}

	// web → native
	for i := range 20 {
		b := wire.AppendUplink(nil, &wire.Uplink{Audio: wire.Audio{Voice: true, Seq: uint16(i), Timestamp: uint32(i * 960), Payload: []byte("from-web")}})
		if err := web.sess.SendDatagram(b); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitFor(t, func() bool { return nativeGot.Load() >= 15 }, "native client received web audio")
	if uint32(nativeFrom.Load()) != st.GetYourSessionId() {
		t.Fatalf("audio attributed to %d, want web's session %d", nativeFrom.Load(), st.GetYourSessionId())
	}

	// native → web
	got := make(chan wire.Downlink, 32)
	go func() {
		for {
			b, err := web.sess.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			if _, k, _ := wire.Peek(b); k == wire.KindDownlink {
				if d, err := wire.ParseDownlink(b); err == nil {
					got <- d
				}
			}
		}
	}()
	go func() {
		for i := range 20 {
			_ = native.SendAudio(&wire.Uplink{Audio: wire.Audio{Voice: true, Seq: uint16(i), Timestamp: uint32(i * 960), Payload: []byte("from-native")}})
			time.Sleep(5 * time.Millisecond)
		}
	}()
	select {
	case d := <-got:
		if string(d.Payload) != "from-native" {
			t.Fatalf("web got %q", d.Payload)
		}
	case <-ctx.Done():
		t.Fatal("web received no downlink audio")
	}

	// Ping echo over WebTransport datagrams is covered by the browser test;
	// here: a Ping control round trip on the WebTransport control stream.
	web.send(t, &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Ping{Ping: &controlpb.Ping{Id: 7}}})
	if p := web.next(t, func(m *controlpb.ControlMessage) bool { return m.GetPong() != nil }).GetPong(); p.GetId() != 7 {
		t.Fatalf("pong %d", p.GetId())
	}
}

func TestWebTransportBadTokenIsFatal(t *testing.T) {
	ts := startServerWith(t, serverSetup{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, sess, err := wtDial(ctx, ts.addr, "")
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := sess.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteControl(ctrl, &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Hello{Hello: &controlpb.Hello{Token: ""}}}); err != nil {
		t.Fatal(err)
	}
	_ = ctrl.SetReadDeadline(time.Now().Add(3 * time.Second))
	m, err := wire.NewControlReader(ctrl).Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if e := m.GetError(); e == nil || !e.GetFatal() || e.GetCode() != controlpb.Error_UNAUTHENTICATED {
		t.Fatalf("want fatal UNAUTHENTICATED, got %v", m)
	}
	select {
	case <-sess.Context().Done():
	case <-time.After(3 * time.Second):
		t.Fatal("session still open after a bad token")
	}
}

func TestWebTransportOrigins(t *testing.T) {
	ts := startServerWith(t, serverSetup{wtOrigins: "https://chat.example.test, https://other.test/"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if rsp, _, err := wtDial(ctx, ts.addr, "https://evil.test"); err == nil {
		t.Fatal("a session from a disallowed origin was accepted")
	} else if rsp != nil && rsp.StatusCode != http.StatusForbidden {
		t.Fatalf("disallowed origin: status %d", rsp.StatusCode)
	}
	dialWT(t, ts.addr, "web", "https://chat.example.test")
	dialWT(t, ts.addr, "web2", "https://other.test")
}

// WEBTRANSPORT_ORIGINS=directory: the auth service's list, fetched.
func TestWebTransportDirectoryOrigins(t *testing.T) {
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"origins":["https://chat.other.test"]}`))
	}))
	defer authSrv.Close()
	f := origins.NewFetcher(authSrv.URL, time.Hour, nil)
	p, err := origins.Parse("directory,https://chat.own.test", f)
	if err != nil {
		t.Fatal(err)
	}
	ts := startServerWith(t, serverSetup{wtOrigins: "directory,https://chat.own.test", origins: p})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Not fetched yet: the own origin only.
	if _, _, err := wtDial(ctx, ts.addr, "https://chat.other.test"); err == nil {
		t.Fatal("accepted before the list was fetched")
	}
	dialWT(t, ts.addr, "own", "https://chat.own.test")
	if _, err := f.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	dialWT(t, ts.addr, "other", "https://chat.other.test")
	if _, _, err := wtDial(ctx, ts.addr, "https://evil.test"); err == nil {
		t.Fatal("a session from an unlisted origin was accepted")
	}
}

func TestOriginAllowed(t *testing.T) {
	for _, c := range []struct {
		list, origin string
		want         bool
	}{
		{"", "https://a.test", true},
		{"*", "https://a.test", true},
		{"https://a.test", "https://a.test", true},
		{"https://a.test/", "https://A.test", true},
		{"https://a.test,https://b.test", "https://b.test", true},
		{"https://a.test", "https://b.test", false},
		{"https://a.test", "", false},
		{"https://a.test", "http://a.test", false},
		{"https://a.test", "https://a.test:443", true},
		{"directory", "https://a.test", false},
	} {
		if got := transport.OriginAllowed(c.list, c.origin); got != c.want {
			t.Errorf("OriginAllowed(%q, %q) = %v, want %v", c.list, c.origin, got, c.want)
		}
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
