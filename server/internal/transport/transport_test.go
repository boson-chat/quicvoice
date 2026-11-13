package transport_test

import (
	"context"
	"crypto/tls"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/client"
	"github.com/boson-chat/quicvoice/server/config"
	"github.com/boson-chat/quicvoice/server/internal/certs"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/transport"
	"github.com/boson-chat/quicvoice/wire"
)

func freeUDPAddr(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close()
	return addr
}

func startServer(t *testing.T) string {
	t.Helper()
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.QUICConfig.Addr = freeUDPAddr(t)
	cert, err := certs.SelfSigned("localhost")
	if err != nil {
		t.Fatal(err)
	}
	srv := transport.NewServer(cfg, &tls.Config{Certificates: []tls.Certificate{cert}},
		auth.NewInsecureAuthService(), room.NewRoomService(10))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
	time.Sleep(100 * time.Millisecond)
	return cfg.QUICConfig.Addr
}

func TestTwoClientsExchangeAudio(t *testing.T) {
	addr := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var bobGot atomic.Int32
	var fromSession atomic.Int32
	alice, err := client.Dial(ctx, client.Config{Addr: addr, Token: "alice", InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	bob, err := client.Dial(ctx, client.Config{Addr: addr, Token: "bob", InsecureSkipVerify: true,
		OnAudio: func(d wire.Downlink) {
			fromSession.Store(int32(d.SessionID))
			bobGot.Add(1)
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()

	as, err := alice.Join(ctx, "room")
	if err != nil {
		t.Fatal(err)
	}
	bs, err := bob.Join(ctx, "room")
	if err != nil {
		t.Fatal(err)
	}
	if len(bs.Participants) != 2 {
		t.Fatalf("bob sees %d participants, want 2", len(bs.Participants))
	}

	for i := range 20 {
		_ = alice.SendAudio(&wire.Uplink{Audio: wire.Audio{Seq: uint16(i), Timestamp: uint32(i * 960), Payload: []byte("x")}})
		time.Sleep(5 * time.Millisecond)
	}
	deadline := time.Now().Add(2 * time.Second)
	for bobGot.Load() < 15 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := bobGot.Load(); n < 15 {
		t.Fatalf("bob received %d/20 frames", n)
	}
	if uint32(fromSession.Load()) != as.YourSessionId {
		t.Fatalf("frames attributed to session %d, want %d", fromSession.Load(), as.YourSessionId)
	}
}

func TestBadTokenRejected(t *testing.T) {
	addr := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Dial(ctx, client.Config{Addr: addr, Token: "", InsecureSkipVerify: true}); err == nil {
		t.Fatal("empty token accepted")
	}
}
