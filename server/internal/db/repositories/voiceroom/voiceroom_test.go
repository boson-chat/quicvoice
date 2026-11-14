package voiceroom_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
