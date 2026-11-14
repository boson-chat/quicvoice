package room

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Def is a configured room: a stable id clients join by, and the name shown
// in room lists.
type Def struct {
	ID   string
	Name string
}

// DefaultRooms is the room list used when VOICE_ROOMS is unset.
const DefaultRooms = "general:General"

var (
	ErrNoRooms     = errors.New("room: no rooms configured")
	roomIDPattern  = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	maxDisplayName = 64
)

// ParseDefs parses a VOICE_ROOMS value: comma-separated `id:Display Name`
// entries, in display order. A bare `id` uses the id as its name. Ids are
// lowercase [a-z0-9-]{1,32} and must be unique.
func ParseDefs(s string) ([]Def, error) {
	var defs []Def
	seen := map[string]bool{}
	for entry := range strings.SplitSeq(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, name, _ := strings.Cut(entry, ":")
		id, name = strings.TrimSpace(id), strings.TrimSpace(name)
		if !roomIDPattern.MatchString(id) {
			return nil, fmt.Errorf("room: invalid room id %q (want lowercase [a-z0-9-]{1,32})", id)
		}
		if seen[id] {
			return nil, fmt.Errorf("room: duplicate room id %q", id)
		}
		seen[id] = true
		if name == "" {
			name = id
		}
		if len(name) > maxDisplayName {
			return nil, fmt.Errorf("room: name for %q is longer than %d bytes", id, maxDisplayName)
		}
		defs = append(defs, Def{ID: id, Name: name})
	}
	if len(defs) == 0 {
		return nil, ErrNoRooms
	}
	return defs, nil
}
