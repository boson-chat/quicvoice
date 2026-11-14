// Package roles defines the privilege levels voice rooms can be locked to.
//
// They are IRC channel privilege levels, not a separate role system: the
// source of truth is Ergo (ChanServ registration and live channel modes,
// which Ergo sets from AMODE entries on join). Oper is the network level:
// auth-service admins (voice token role moderator or admin).
package roles

import (
	"fmt"
	"strings"
)

// Role is ordered: a higher value includes every lower one.
type Role uint8

const (
	Member  Role = iota // anyone (in or out of the channel)
	Voice               // +v
	Halfop              // %h
	Op                  // @o
	Admin               // &a (channel admin, "protected")
	Founder             // ~q, or the ChanServ-registered founder
	Oper                // network level: auth admin / voice moderator+
	numRoles
)

var names = [numRoles]string{"member", "voice", "halfop", "op", "admin", "founder", "oper"}

func (r Role) String() string {
	if r < numRoles {
		return names[r]
	}
	return fmt.Sprintf("role(%d)", r)
}

// MarshalText makes Role a JSON string.
func (r Role) MarshalText() ([]byte, error) { return []byte(r.String()), nil }

// UnmarshalText parses the names String returns.
func (r *Role) UnmarshalText(b []byte) error {
	p, err := Parse(string(b))
	if err != nil {
		return err
	}
	*r = p
	return nil
}

// Parse accepts the names String returns (case-insensitive). "" is Member.
func Parse(s string) (Role, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return Member, nil
	}
	for i, n := range names {
		if n == s {
			return Role(i), nil
		}
	}
	return Member, fmt.Errorf("unknown role %q (want one of %s)", s, strings.Join(names[:], ", "))
}

// FromModeLetter maps an IRC channel-user mode letter (q a o h v) to a Role.
// Anything else is Member.
func FromModeLetter(m string) Role {
	switch m {
	case "q":
		return Founder
	case "a":
		return Admin
	case "o":
		return Op
	case "h":
		return Halfop
	case "v":
		return Voice
	}
	return Member
}

// Names lists every role, lowest first.
func Names() []string { return append([]string(nil), names[:]...) }
