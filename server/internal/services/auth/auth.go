// Package auth verifies the room tokens clients present in Hello.
package auth

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrUnauthenticated = errors.New("auth: invalid token")
	ErrTokenExpired    = errors.New("auth: token expired")
)

// Identity is who a connection is, as asserted by the app backend.
type Identity struct {
	UserID      string
	DisplayName string
	// Account is the IRC (Ergo) account name: the auth service's username,
	// carried in the token's `name` claim. "" = unknown (no IRC roles).
	Account string
	Role    controlpb.Role
	// Rooms the token allows joining. Empty = any room.
	Rooms []string

	// Network roles (display names, e.g. "Owner", "DJ") and permissions
	// (e.g. "voice.create") from the auth service's `roles` / `perms`
	// claims. HasRoles / HasPerms: the claim was in the token at all, even
	// as []. Tokens minted before network roles have neither.
	NetworkRoles []string
	HasRoles     bool
	Perms        []string
	HasPerms     bool
}

// Voice permissions carried in the `perms` claim. Others are ignored.
const (
	PermVoiceCreate      = "voice.create"
	PermVoiceManageAny   = "voice.manage_any"
	PermVoiceBypassLocks = "voice.bypass_locks"
)

func (i *Identity) CanJoin(room string) bool {
	return len(i.Rooms) == 0 || slices.Contains(i.Rooms, room)
}

// IsStaff: network-level admin (auth admins get voice role moderator).
func (i *Identity) IsStaff() bool {
	return i.Role >= controlpb.Role_MODERATOR
}

// HasNetworkPerms: the token carries the `perms` claim, so permission
// decisions use it instead of the legacy `role` admin check.
func (i *Identity) HasNetworkPerms() bool { return i.HasPerms }

// HasPerm: the `perms` claim lists p (exact match).
func (i *Identity) HasPerm(p string) bool { return slices.Contains(i.Perms, p) }

// HasNetworkRole: the `roles` claim lists name (case-insensitive).
func (i *Identity) HasNetworkRole(name string) bool {
	return slices.ContainsFunc(i.NetworkRoles, func(r string) bool { return strings.EqualFold(r, name) })
}

func (i *Identity) CanSpeak() bool {
	return i.Role >= controlpb.Role_SPEAKER
}

type AuthServiceImpl interface {
	Verify(ctx context.Context, token string) (*Identity, error)
}

// Claims minted by the app backend (HS256).
type Claims struct {
	Name  string   `json:"name"`
	Role  string   `json:"role"`  // listener | speaker | moderator | admin
	Rooms []string `json:"rooms"` // allowed rooms; empty = any
	// Network roles and permissions (auth service). nil = claim absent
	// (token from before network roles); a pointer to [] = present, empty.
	Roles *[]string `json:"roles,omitempty"`
	Perms *[]string `json:"perms,omitempty"`
	jwt.RegisteredClaims
}

type JWTAuthService struct {
	secret []byte
}

func NewJWTAuthService(secret string) AuthServiceImpl {
	return &JWTAuthService{secret: []byte(secret)}
}

func (s *JWTAuthService) Verify(_ context.Context, token string) (*Identity, error) {
	var c Claims
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) { return s.secret, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	return identityFromClaims(&c, err)
}

// identityFromClaims turns verified claims (or the verification error) into
// an Identity. Shared by the HMAC and JWKS verifiers.
func identityFromClaims(c *Claims, err error) (*Identity, error) {
	if errors.Is(err, jwt.ErrTokenExpired) {
		return nil, ErrTokenExpired
	}
	if err != nil || c.Subject == "" {
		return nil, ErrUnauthenticated
	}
	name := c.Name
	if name == "" {
		name = c.Subject
	}
	id := &Identity{UserID: c.Subject, DisplayName: name, Account: c.Name, Role: parseRole(c.Role), Rooms: c.Rooms}
	if c.Roles != nil {
		id.HasRoles, id.NetworkRoles = true, nonEmpty(*c.Roles)
	}
	if c.Perms != nil {
		id.HasPerms, id.Perms = true, nonEmpty(*c.Perms)
	}
	return id, nil
}

// nonEmpty copies ss without blank entries (trimmed).
func nonEmpty(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// InsecureAuthService accepts any non-empty token; the token is the user id.
// "name@role" sets the role, e.g. "alice@moderator". Dev/testing only.
type InsecureAuthService struct{}

func NewInsecureAuthService() AuthServiceImpl { return InsecureAuthService{} }

func (InsecureAuthService) Verify(_ context.Context, token string) (*Identity, error) {
	if token == "" {
		return nil, ErrUnauthenticated
	}
	user, role, _ := strings.Cut(token, "@")
	return &Identity{UserID: user, DisplayName: user, Account: user, Role: parseRole(role)}, nil
}

func parseRole(s string) controlpb.Role {
	switch strings.ToLower(s) {
	case "listener":
		return controlpb.Role_LISTENER
	case "moderator":
		return controlpb.Role_MODERATOR
	case "admin":
		return controlpb.Role_ADMIN
	default:
		return controlpb.Role_SPEAKER
	}
}
