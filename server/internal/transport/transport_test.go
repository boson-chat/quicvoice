package transport_test

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/client"
	"github.com/boson-chat/quicvoice/server/config"
	qvhttp "github.com/boson-chat/quicvoice/server/http"
	"github.com/boson-chat/quicvoice/server/internal/certs"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/transport"
	"github.com/boson-chat/quicvoice/server/metrics"
	"github.com/boson-chat/quicvoice/wire"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
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

type testServer struct {
	addr string
	// metricsURL is the HTTP router's /metrics, backed by the real provider.
	metricsURL string
}

func startServer(t *testing.T) testServer {
	t.Helper()
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.QUICConfig.Addr = freeUDPAddr(t)
	cfg.TelemetryConfig.QUICStatsInterval = 50 * time.Millisecond
	cert, err := certs.SelfSigned("localhost")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	provider, err := metrics.NewProvider(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := metrics.New(provider.MeterProvider())
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(qvhttp.NewRouter("test", provider.Handler()))
	t.Cleanup(httpSrv.Close)

	srv := transport.NewServer(cfg, &tls.Config{Certificates: []tls.Certificate{cert}},
		auth.NewInsecureAuthService(), room.NewRoomService(10, m), m)
	go func() { _ = srv.Serve(ctx) }()
	time.Sleep(100 * time.Millisecond)
	return testServer{addr: cfg.QUICConfig.Addr, metricsURL: httpSrv.URL + "/metrics"}
}

func TestTwoClientsExchangeAudio(t *testing.T) {
	ts := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exchangeAudio(ctx, t, ts.addr)
}

// exchangeAudio connects alice and bob to one room and has alice send 20
// frames to bob. Both stay connected until the test ends.
func exchangeAudio(ctx context.Context, t *testing.T, addr string) {
	t.Helper()
	var bobGot atomic.Int32
	var fromSession atomic.Int32
	alice, err := client.Dial(ctx, client.Config{Addr: addr, Token: "alice", InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = alice.Close() })
	bob, err := client.Dial(ctx, client.Config{Addr: addr, Token: "bob", InsecureSkipVerify: true,
		OnAudio: func(d wire.Downlink) {
			fromSession.Store(int32(d.SessionID))
			bobGot.Add(1)
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bob.Close() })

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
		_ = alice.SendAudio(&wire.Uplink{Audio: wire.Audio{
			Voice: true, Seq: uint16(i), Timestamp: uint32(i * 960), Payload: []byte("x"),
		}})
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
	ts := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Dial(ctx, client.Config{Addr: ts.addr, Token: "", InsecureSkipVerify: true}); err == nil {
		t.Fatal("empty token accepted")
	}
}

// TestMetricsEndpoint drives real traffic through the server and checks the
// key series on GET /metrics, as Prometheus would scrape them.
func TestMetricsEndpoint(t *testing.T) {
	ts := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := client.Dial(ctx, client.Config{Addr: ts.addr, Token: "", InsecureSkipVerify: true}); err == nil {
		t.Fatal("empty token accepted")
	}
	exchangeAudio(ctx, t, ts.addr)
	time.Sleep(150 * time.Millisecond) // let a QUIC stats sample land

	fams := scrape(t, ts.metricsURL)
	want := []struct {
		name   string
		labels map[string]string
		min    float64
	}{
		{"quicvoice_connections", nil, 2},
		{"quicvoice_sessions", map[string]string{"client_version": "quicvoice-go/0.1"}, 2},
		{"quicvoice_handshakes_total", map[string]string{"outcome": "ok"}, 2},
		{"quicvoice_handshakes_total", map[string]string{"outcome": "bad_token"}, 1},
		{"quicvoice_handshake_duration_seconds", nil, 2},
		{"quicvoice_room_joins_total", map[string]string{"outcome": "ok"}, 2},
		{"quicvoice_room_size", nil, 2},
		{"quicvoice_rooms", nil, 1},
		{"quicvoice_room_size_max", nil, 2},
		{"quicvoice_participants", map[string]string{"role": "speaker"}, 2},
		{"quicvoice_control_messages_total", map[string]string{"type": "join"}, 2},
		{"quicvoice_datagrams_received_total", map[string]string{"kind": "uplink"}, 15},
		{"quicvoice_datagrams_sent_total", map[string]string{"kind": "downlink"}, 15},
		{"quicvoice_datagrams_io_bytes_total", map[string]string{"network_io_direction": "transmit"}, 15 * 13},
		{"quicvoice_audio_frames_forwarded_total", nil, 15},
		{"quicvoice_audio_speech_seconds_total", nil, 15 * 0.02},
		{"quicvoice_datagrams_dropped_total", map[string]string{"reason": "rate_limited"}, 0},
		{"quicvoice_quic_rtt_seconds", nil, 1},
		{"quicvoice_quic_packets_total", map[string]string{"network_io_direction": "receive"}, 1},
		{"target_info", map[string]string{"service_name": "quicvoiced", "deployment_environment": "development"}, 1},
		{"go_goroutine_count", nil, 1},
	}
	for _, w := range want {
		v, ok := sample(fams, w.name, w.labels)
		if !ok {
			t.Errorf("%s%v: series missing", w.name, w.labels)
			continue
		}
		if v < w.min {
			t.Errorf("%s%v = %v, want >= %v", w.name, w.labels, v, w.min)
		}
	}
	if t.Failed() {
		for name := range fams {
			t.Log("exposed:", name)
		}
	}
}

func scrape(t *testing.T, url string) map[string]*dto.MetricFamily {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics: %s", resp.Status)
	}
	p := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := p.TextToMetricFamilies(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return fams
}

// sample returns the value of the first series in family name whose labels
// include want. Histograms report their sample count.
func sample(fams map[string]*dto.MetricFamily, name string, want map[string]string) (float64, bool) {
	f, ok := fams[name]
	if !ok {
		return 0, false
	}
next:
	for _, m := range f.GetMetric() {
		have := map[string]string{}
		for _, l := range m.GetLabel() {
			have[l.GetName()] = l.GetValue()
		}
		for k, v := range want {
			if have[k] != v {
				continue next
			}
		}
		switch {
		case m.Counter != nil:
			return m.Counter.GetValue(), true
		case m.Gauge != nil:
			return m.Gauge.GetValue(), true
		case m.Histogram != nil:
			return float64(m.Histogram.GetSampleCount()), true
		case m.Untyped != nil:
			return m.Untyped.GetValue(), true
		}
	}
	return 0, false
}
