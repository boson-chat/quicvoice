package roles

import (
	"encoding/json"
	"testing"
)

func TestParseRoundTrip(t *testing.T) {
	for _, n := range Names() {
		r, err := Parse(n)
		if err != nil || r.String() != n {
			t.Fatalf("Parse(%q) = %v, %v", n, r, err)
		}
	}
	if r, err := Parse(" OP "); err != nil || r != Op {
		t.Fatalf("Parse(OP) = %v %v", r, err)
	}
	if r, err := Parse(""); err != nil || r != Member {
		t.Fatalf("empty = %v %v", r, err)
	}
	if _, err := Parse("owner"); err == nil {
		t.Fatal("owner accepted")
	}
}

func TestOrderAndModes(t *testing.T) {
	if !(Member < Voice && Voice < Halfop && Halfop < Op && Op < Admin && Admin < Founder && Founder < Oper) {
		t.Fatal("roles out of order")
	}
	for m, want := range map[string]Role{"q": Founder, "a": Admin, "o": Op, "h": Halfop, "v": Voice, "": Member, "x": Member} {
		if got := FromModeLetter(m); got != want {
			t.Errorf("%q = %v, want %v", m, got, want)
		}
	}
}

func TestJSON(t *testing.T) {
	var v struct{ R Role }
	if err := json.Unmarshal([]byte(`{"R":"halfop"}`), &v); err != nil || v.R != Halfop {
		t.Fatalf("%v %v", v, err)
	}
	b, _ := json.Marshal(v)
	if string(b) != `{"R":"halfop"}` {
		t.Fatal(string(b))
	}
	if err := json.Unmarshal([]byte(`{"R":"nope"}`), &v); err == nil {
		t.Fatal("bad role accepted")
	}
}
