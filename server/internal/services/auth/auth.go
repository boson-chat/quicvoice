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
}

func (i *Identity) CanJoin(room string) bool {
	return len(i.Rooms) == 0 || slices.Contains(i.Rooms, room)
}

// IsStaff: network-level admin (auth admins get voice role moderator).
func (i *Identity) IsStaff() bool {
	return i.Role >= controlpb.Role_MODERATOR
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
	return &Identity{UserID: c.Subject, DisplayName: name, Account: c.Name, Role: parseRole(c.Role), Rooms: c.Rooms}, nil
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
