// Package voiceroom persists the voice room registry.
//
// Storage is one JSON file, rewritten atomically (temp file in the same
// directory, fsync, rename, fsync the directory) on every change. The
// registry is small (tens to a few hundred rooms), has a single writer (this
// process) and is read from memory by the service, so a database would add
// a dependency and a schema without buying anything; the file is also easy
// to inspect and back up. An empty path keeps the registry in memory only.
package voiceroom

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/boson-chat/quicvoice/server/internal/roles"
)

// Record is one stored room.
type Record struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Parent IRC channel (casefolded, e.g. "#boson"); "" = none.
	Parent      string    `json:"parent,omitempty"`
	OwnerUserID string    `json:"owner_user_id,omitempty"`
	OwnerName   string    `json:"owner_name,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	Position    int       `json:"position"`
	// Static rooms come from VOICE_ROOMS and are reconciled with it at start.
	Static    bool       `json:"static,omitempty"`
	MinRole   roles.Role `json:"min_role"`
	SpeakRole roles.Role `json:"speak_role"`
	// Caller's IRC client must be in Parent to join.
	RequireChannelMembership bool `json:"require_channel_membership,omitempty"`
}

var ErrNotFound = errors.New("voiceroom: not found")

type RoomRepositoryImpl interface {
	List(ctx context.Context) ([]Record, error)
	// Put inserts or replaces by ID.
	Put(ctx context.Context, r Record) error
	Delete(ctx context.Context, id string) error
}

type fileFormat struct {
	Version int      `json:"version"`
	Rooms   []Record `json:"rooms"`
}

const formatVersion = 1

type RoomRepository struct {
	path string

	mu    sync.Mutex
	rooms map[string]Record
}

// NewRoomRepository loads path (missing file = empty registry). path "" =
// in-memory only.
func NewRoomRepository(path string) (RoomRepositoryImpl, error) {
	r := &RoomRepository{path: path, rooms: map[string]Record{}}
	if path == "" {
		return r, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("voiceroom: %w", err)
	}
	var f fileFormat
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("voiceroom: %s: %w", path, err)
	}
	if f.Version > formatVersion {
		return nil, fmt.Errorf("voiceroom: %s: format version %d is newer than this server (%d)", path, f.Version, formatVersion)
	}
	for _, rec := range f.Rooms {
		r.rooms[rec.ID] = rec
	}
	return r, nil
}

func (r *RoomRepository) List(_ context.Context) ([]Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sorted(), nil
}

func (r *RoomRepository) Put(_ context.Context, rec Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, had := r.rooms[rec.ID]
	r.rooms[rec.ID] = rec
	if err := r.save(); err != nil {
		if had {
			r.rooms[rec.ID] = old
		} else {
			delete(r.rooms, rec.ID)
		}
		return err
	}
	return nil
}

func (r *RoomRepository) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.rooms[id]
	if !ok {
		return ErrNotFound
	}
	delete(r.rooms, id)
	if err := r.save(); err != nil {
		r.rooms[id] = old
		return err
	}
	return nil
}

// Caller holds r.mu.
func (r *RoomRepository) sorted() []Record {
	out := make([]Record, 0, len(r.rooms))
	for _, rec := range r.rooms {
		out = append(out, rec)
	}
	slices.SortFunc(out, func(a, b Record) int {
		return cmp.Or(cmp.Compare(a.Position, b.Position), a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out
}

// save writes the whole registry atomically. Caller holds r.mu.
func (r *RoomRepository) save() error {
	if r.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(fileFormat{Version: formatVersion, Rooms: r.sorted()}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("voiceroom: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".rooms-*.json")
	if err != nil {
		return fmt.Errorf("voiceroom: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("voiceroom: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("voiceroom: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("voiceroom: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		return fmt.Errorf("voiceroom: %w", err)
	}
	if err := os.Rename(tmp.Name(), r.path); err != nil {
		return fmt.Errorf("voiceroom: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
