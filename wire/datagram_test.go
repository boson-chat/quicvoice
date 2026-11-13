package wire

import (
	"bytes"
	"testing"
)

func TestUplinkRoundTrip(t *testing.T) {
	in := Uplink{
		Audio: Audio{
			Flags: FlagStart, Voice: true, Level: 30,
			Seq: 65535, Timestamp: 0xdeadbeef, Payload: []byte{1, 2, 3},
		},
		Target: 4,
	}
	b := AppendUplink(nil, &in)
	if len(b) != UplinkHeaderLen+3 {
		t.Fatalf("len = %d", len(b))
	}
	out, err := ParseUplink(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.Flags != in.Flags || out.Voice != in.Voice || out.Level != in.Level ||
		out.Seq != in.Seq || out.Timestamp != in.Timestamp || out.Target != in.Target ||
		!bytes.Equal(out.Payload, in.Payload) {
		t.Fatalf("got %+v want %+v", out, in)
	}
}

func TestForwardMatchesDownlink(t *testing.T) {
	up := AppendUplink(nil, &Uplink{
		Audio: Audio{Flags: FlagEnd, Level: 127, Seq: 7, Timestamp: 960, Payload: []byte("opus")},
	})
	fwd, err := ForwardUplink(nil, up, 42, TargetDirect)
	if err != nil {
		t.Fatal(err)
	}
	want := AppendDownlink(nil, &Downlink{
		Audio:      Audio{Flags: FlagEnd, Level: 127, Seq: 7, Timestamp: 960, Payload: []byte("opus")},
		TargetKind: TargetDirect,
		SessionID:  42,
	})
	if !bytes.Equal(fwd, want) {
		t.Fatalf("forward %x != downlink %x", fwd, want)
	}
	d, err := ParseDownlink(fwd)
	if err != nil || d.SessionID != 42 || string(d.Payload) != "opus" {
		t.Fatalf("parse: %+v %v", d, err)
	}
}

func TestLevelClamped(t *testing.T) {
	b := AppendUplink(nil, &Uplink{Audio: Audio{Voice: true, Level: 200}})
	u, _ := ParseUplink(b)
	if !u.Voice || u.Level != LevelSilence {
		t.Fatalf("got voice=%v level=%d", u.Voice, u.Level)
	}
}

func TestPing(t *testing.T) {
	b := AppendPing(nil, &Ping{Reply: true, ID: 9, SentUS: 123456})
	if len(b) != PingLen {
		t.Fatalf("len = %d", len(b))
	}
	p, err := ParsePing(b)
	if err != nil || !p.Reply || p.ID != 9 || p.SentUS != 123456 {
		t.Fatalf("got %+v %v", p, err)
	}
}

func TestErrors(t *testing.T) {
	if _, err := ParseUplink([]byte{0x10, 0}); err != ErrShort {
		t.Fatalf("short: %v", err)
	}
	b := AppendUplink(nil, &Uplink{})
	b[0] = 2<<4 | byte(KindUplink)
	if _, err := ParseUplink(b); err != ErrVersion {
		t.Fatalf("version: %v", err)
	}
	if _, err := ParseDownlink(AppendUplink(nil, &Uplink{Audio: Audio{Payload: []byte("xx")}})); err != ErrKind {
		t.Fatalf("kind: %v", err)
	}
}
