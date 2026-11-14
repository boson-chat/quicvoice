package room

import (
	"reflect"
	"testing"
)

func TestParseDefs(t *testing.T) {
	tests := []struct {
		in      string
		want    []Def
		wantErr bool
	}{
		{in: DefaultRooms, want: []Def{{"general", "General"}}},
		{in: "general:General, gaming:Gaming Night ,afk", want: []Def{{"general", "General"}, {"gaming", "Gaming Night"}, {"afk", "afk"}}},
		{in: "a-1:x:y", want: []Def{{"a-1", "x:y"}}},
		{in: "", wantErr: true},
		{in: " , ", wantErr: true},
		{in: "General:General", wantErr: true},
		{in: "has space:x", wantErr: true},
		{in: "abcdefghijklmnopqrstuvwxyz0123456:x", wantErr: true}, // 33 chars
		{in: "a:A,a:B", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseDefs(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseDefs(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseDefs(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
