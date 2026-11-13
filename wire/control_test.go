package wire

import (
	"bytes"
	"testing"

	"github.com/boson-chat/quicvoice/gen/controlpb"
)

func TestControlRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	for _, room := range []string{"a", "lobby"} {
		if err := WriteControl(&buf, &controlpb.ControlMessage{Msg: &controlpb.ControlMessage_Join{Join: &controlpb.Join{Room: room}}}); err != nil {
			t.Fatal(err)
		}
	}
	rd := NewControlReader(&buf)
	for _, want := range []string{"a", "lobby"} {
		m, err := rd.Read()
		if err != nil {
			t.Fatal(err)
		}
		if m.GetJoin().GetRoom() != want {
			t.Fatalf("got %q want %q", m.GetJoin().GetRoom(), want)
		}
	}
}
