package voiceroom_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/server/internal/db/repositories/voiceroom"
	"github.com/boson-chat/quicvoice/server/internal/roles"
)

func TestPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "rooms.json")
	repo, err := voiceroom.NewRoomRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(100, 0).UTC()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(repo.Put(ctx, voiceroom.Record{ID: "b", Name: "B", Position: 1, CreatedAt: t0}))
	must(repo.Put(ctx, voiceroom.Record{ID: "a", Name: "A", Position: 1, CreatedAt: t0.Add(-time.Second), Parent: "#x", MinRole: roles.Op, SpeakRole: roles.Voice}))
	must(repo.Put(ctx, voiceroom.Record{ID: "g", Name: "General", Position: 0, Static: true, CreatedAt: t0}))
	must(repo.Delete(ctx, "b"))
	if err := repo.Delete(ctx, "b"); !errors.Is(err, voiceroom.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}

	again, err := voiceroom.NewRoomRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := again.List(ctx)
	if len(got) != 2 || got[0].ID != "g" || got[1].ID != "a" || got[1].MinRole != roles.Op || got[1].SpeakRole != roles.Voice || got[1].Parent != "#x" {
		t.Fatalf("reloaded = %+v", got)
	}
	// No temp files left behind.
	ents, _ := os.ReadDir(filepath.Dir(path))
	if len(ents) != 1 {
		t.Fatalf("dir = %v", ents)
	}
}

func TestBadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rooms.json")
	_ = os.WriteFile(path, []byte("{nope"), 0o600)
	if _, err := voiceroom.NewRoomRepository(path); err == nil {
		t.Fatal("corrupt file accepted")
	}
	_ = os.WriteFile(path, []byte(`{"version":99,"rooms":[]}`), 0o600)
	if _, err := voiceroom.NewRoomRepository(path); err == nil {
		t.Fatal("future version accepted")
	}
}

func TestInMemory(t *testing.T) {
	repo, err := voiceroom.NewRoomRepository("")
	if err != nil {
		t.Fatal(err)
	}
	_ = repo.Put(context.Background(), voiceroom.Record{ID: "x"})
	if l, _ := repo.List(context.Background()); len(l) != 1 {
		t.Fatal(l)
	}
}

func TestAllowedRolesRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rooms.json")
	repo, err := voiceroom.NewRoomRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(100, 0).UTC()
	want := voiceroom.Record{ID: "booth", Name: "Booth", CreatedAt: t0, AllowedRoles: []string{"DJ", "Owner"}}
	if err := repo.Put(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err := repo.Put(ctx, voiceroom.Record{ID: "open", Name: "Open", CreatedAt: t0, Position: 1}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Count(string(b), "allowed_roles") != 1 {
		t.Fatalf("allowed_roles should be omitted when empty:\n%s", b)
	}
	again, err := voiceroom.NewRoomRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := again.List(ctx)
	if len(got) != 2 || !got[0].Equal(want) || got[1].AllowedRoles != nil {
		t.Fatalf("reloaded = %+v", got)
	}
}

// Files written before allowed_roles existed load unchanged.
func TestLoadsFileWithoutAllowedRoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rooms.json")
	old := `{"version":1,"rooms":[{"id":"boson-stage","name":"Stage","parent":"#boson","owner_user_id":"usr_1","owner_name":"fred",` +
		`"created_at":"2026-11-13T21:04:05.123Z","position":1,"min_role":"voice","speak_role":"op"}]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, err := voiceroom.NewRoomRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := repo.List(context.Background())
	if len(got) != 1 || got[0].ID != "boson-stage" || got[0].MinRole != roles.Voice || got[0].SpeakRole != roles.Op || got[0].AllowedRoles != nil {
		t.Fatalf("loaded %+v", got)
	}
}

func TestRecordEqual(t *testing.T) {
	t0 := time.Unix(100, 0)
	a := voiceroom.Record{ID: "a", CreatedAt: t0, AllowedRoles: []string{"DJ"}}
	b := a
	b.CreatedAt = t0.UTC()
	if !a.Equal(b) {
		t.Fatal("same instant in another location should be equal")
	}
	b.AllowedRoles = []string{"dj"}
	if a.Equal(b) {
		t.Fatal("allowed_roles differ")
	}
}
