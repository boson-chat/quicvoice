package voicechannel

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/boson-chat/quicvoice/server/internal/db/repositories/voiceroom"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/services/settings"
)

func TestSetStatic(t *testing.T) {
	ctx := context.Background()
	repo, _ := voiceroom.NewRoomRepository(t.TempDir() + "/rooms.json")
	s := newSvc(t, repo, []room.Def{{ID: "general", Name: "General"}, {ID: "music", Name: "Music"}}, settings.CreateRegistered)
	dyn, err := s.Create(ctx, &auth.Identity{UserID: "u1", Account: "u1"}, CreateInput{Name: "Hangout"})
	if err != nil {
		t.Fatal(err)
	}

	// music removed, general renamed and moved, stage added.
	removed, err := s.SetStatic(ctx, []room.Def{{ID: "stage", Name: "Stage"}, {ID: "general", Name: "Lobby"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0].ID != "music" || removed[0].Name != "Music" {
		t.Fatalf("removed = %+v", removed)
	}
	want := []room.Def{{ID: "stage", Name: "Stage"}, {ID: "general", Name: "Lobby"}, {ID: dyn.Room.ID, Name: "Hangout"}}
	if got := s.Defs(); !slices.Equal(got, want) {
		t.Fatalf("defs = %v, want %v", got, want)
	}
	if s.Has("music") {
		t.Fatal("music still defined")
	}
	// Persisted: a restart with the same list changes nothing.
	recs, _ := repo.List(ctx)
	if len(recs) != 3 {
		t.Fatalf("stored %d rooms", len(recs))
	}
	// Static rooms stay read-only through the API.
	if _, err := s.Delete(ctx, &auth.Identity{UserID: "u1", Role: controlpb.Role_MODERATOR}, "stage"); !errors.Is(err, ErrStatic) {
		t.Fatalf("delete static: %v", err)
	}

	// Unchanged list: nothing removed. Empty list: every static room goes, the dynamic one stays.
	if removed, err := s.SetStatic(ctx, []room.Def{{ID: "stage", Name: "Stage"}, {ID: "general", Name: "Lobby"}}); err != nil || len(removed) != 0 {
		t.Fatalf("no-op: %v %v", removed, err)
	}
	removed, err = s.SetStatic(ctx, nil)
	if err != nil || len(removed) != 2 {
		t.Fatalf("empty: %v %v", removed, err)
	}
	if got := s.Defs(); len(got) != 1 || got[0].ID != dyn.Room.ID {
		t.Fatalf("defs = %v", got)
	}
}

func TestSetLimits(t *testing.T) {
	ctx := context.Background()
	s := newSvc(t, nil, nil, settings.CreateRegistered)
	s.SetLimits(1, 0)
	u := &auth.Identity{UserID: "u1", Account: "u1"}
	if _, err := s.Create(ctx, u, CreateInput{Name: "One"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, u, CreateInput{Name: "Two"}); !errors.Is(err, ErrLimit) {
		t.Fatalf("over max_rooms_per_user: %v", err)
	}
	s.SetLimits(5, 1)
	if _, err := s.Create(ctx, &auth.Identity{UserID: "u2", Account: "u2"}, CreateInput{Name: "Three"}); !errors.Is(err, ErrLimit) {
		t.Fatalf("over max_rooms: %v", err)
	}
}
