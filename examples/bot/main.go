// Command bot is a headless quicvoice client for testing and load tests.
// It joins a room, sends synthetic 20 ms "Opus" frames (random bytes) and
// reports what it receives from the other participants.
//
//	go run ./examples/bot -addr localhost:7700 -insecure -token alice -room general
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/boson-chat/quicvoice/client"
	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/boson-chat/quicvoice/wire"
)

func main() {
	addr := flag.String("addr", "localhost:7700", "voice server host:port")
	token := flag.String("token", "bot", "auth token (with AUTH_INSECURE, any name)")
	room := flag.String("room", "general", "room to join")
	insecure := flag.Bool("insecure", false, "skip TLS verification")
	listen := flag.Bool("listen", false, "only receive, don't send audio")
	frameBytes := flag.Int("frame-bytes", 120, "fake opus payload size (~48 kbps at 120)")
	duration := flag.Duration("duration", 0, "exit after this long (0 = until ctrl-c)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	var mu sync.Mutex
	perSender := map[uint16]int{}
	names := map[uint16]string{}

	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	c, err := client.Dial(dctx, client.Config{
		Addr:               *addr,
		Token:              *token,
		InsecureSkipVerify: *insecure,
		OnAudio: func(d wire.Downlink) {
			mu.Lock()
			perSender[d.SessionID]++
			mu.Unlock()
		},
		OnControl: func(m *controlpb.ControlMessage) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case m.GetParticipantJoined() != nil:
				p := m.GetParticipantJoined().GetParticipant()
				names[uint16(p.GetSessionId())] = p.GetDisplayName()
				log.Printf("+ %s (session %d)", p.GetDisplayName(), p.GetSessionId())
			case m.GetParticipantLeft() != nil:
				p := m.GetParticipantLeft()
				log.Printf("- %s (%s)", names[uint16(p.GetSessionId())], p.GetReason())
			case m.GetError() != nil:
				log.Printf("server error: %s", m.GetError().GetMessage())
			}
		},
	})
	cancel()
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	jctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	state, err := c.Join(jctx, *room)
	cancel()
	if err != nil {
		log.Fatal(err)
	}
	mu.Lock()
	for _, p := range state.GetParticipants() {
		names[uint16(p.GetSessionId())] = p.GetDisplayName()
	}
	mu.Unlock()
	log.Printf("joined %q as session %d with %d participant(s)", state.GetRoom(), state.GetYourSessionId(), len(state.GetParticipants()))

	frames := time.NewTicker(20 * time.Millisecond)
	report := time.NewTicker(2 * time.Second)
	defer frames.Stop()
	defer report.Stop()

	payload := make([]byte, *frameBytes)
	var seq uint16
	ts := randUint32()
	start := true

	for {
		select {
		case <-ctx.Done():
			printReport(c, &mu, perSender, names)
			return
		case <-c.Done():
			log.Fatal("connection closed by server")
		case <-frames.C:
			if *listen {
				continue
			}
			_, _ = rand.Read(payload)
			u := &wire.Uplink{Audio: wire.Audio{Voice: true, Level: 30, Seq: seq, Timestamp: ts, Payload: payload}}
			if start {
				u.Flags = wire.FlagStart
				start = false
			}
			if err := c.SendAudio(u); err != nil {
				log.Printf("send: %v", err)
			}
			seq++
			ts += 960
		case <-report.C:
			_ = c.Ping()
			printReport(c, &mu, perSender, names)
		}
	}
}

func printReport(c *client.Client, mu *sync.Mutex, perSender map[uint16]int, names map[uint16]string) {
	s := c.Stats()
	mu.Lock()
	defer mu.Unlock()
	line := fmt.Sprintf("sent=%d recv=%d rtt=%s", s.Sent, s.Received, s.RTT.Round(100*time.Microsecond))
	for sid, n := range perSender {
		line += fmt.Sprintf(" | %s=%d", names[sid], n)
	}
	log.Print(line)
}

func randUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
