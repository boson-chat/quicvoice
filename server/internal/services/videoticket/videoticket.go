// Package videoticket issues the short-lived tickets voice-channel members
// use to join that channel's video room on quicvideod. Tickets are EdDSA
// JWTs; the public key is served as a JWKS (/.well-known/video-jwks.json)
// that quicvideod verifies against.
package videoticket

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Audience is quicvideod's token audience.
const Audience = "quicvideo"

// PermVideoPublish lets a ticket holder publish camera/screen.
const PermVideoPublish = "video.publish"

type VideoTicketServiceImpl interface {
	Issue(userID, name, room string, canPublish bool) (token string, expires time.Time, err error)
	JWKS() map[string]any
}

type VideoTicketService struct {
	key    ed25519.PrivateKey
	kid    string
	issuer string
	ttl    time.Duration
	now    func() time.Time
}

// Options configure the service.
type Options struct {
	// KeyFile holds the PEM Ed25519 key; created (0600) if missing. Empty =
	// an in-memory key (tickets stop verifying after a restart; dev only).
	KeyFile string
	Issuer  string        // default "voice"
	TTL     time.Duration // default 5m
	Now     func() time.Time
}

func New(o Options) (*VideoTicketService, error) {
	if o.Issuer == "" {
		o.Issuer = "voice"
	}
	if o.TTL <= 0 {
		o.TTL = 5 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	key, err := loadOrCreate(o.KeyFile)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(key.Public().(ed25519.PublicKey))
	return &VideoTicketService{key: key, kid: hex.EncodeToString(sum[:8]), issuer: o.Issuer, ttl: o.TTL, now: o.Now}, nil
}

func loadOrCreate(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		_, k, err := ed25519.GenerateKey(rand.Reader)
		return k, err
	}
	if b, err := os.ReadFile(path); err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil || blk.Type != "PRIVATE KEY" || len(blk.Bytes) != ed25519.SeedSize {
			return nil, fmt.Errorf("videoticket: %s is not an Ed25519 seed PEM", path)
		}
		return ed25519.NewKeyFromSeed(blk.Bytes), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k.Seed()})
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return nil, fmt.Errorf("videoticket: writing %s: %w", path, err)
	}
	return k, nil
}

// Claims mirror quicvideod's (name, rooms, perms).
type Claims struct {
	Name  string   `json:"name"`
	Rooms []string `json:"rooms"`
	Perms []string `json:"perms"`
	jwt.RegisteredClaims
}

// Issue signs a ticket for room (the voice channel id).
func (s *VideoTicketService) Issue(userID, name, room string, canPublish bool) (string, time.Time, error) {
	now := s.now()
	exp := now.Add(s.ttl)
	perms := []string{}
	if canPublish {
		perms = append(perms, PermVideoPublish)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, Claims{Name: name, Rooms: []string{room}, Perms: perms,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: userID, Issuer: s.issuer, Audience: jwt.ClaimStrings{Audience},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(exp),
		}})
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.key)
	return signed, exp, err
}

// JWKS is the public key set quicvideod verifies tickets with.
func (s *VideoTicketService) JWKS() map[string]any {
	return map[string]any{"keys": []map[string]string{{
		"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": s.kid,
		"x": base64.RawURLEncoding.EncodeToString(s.key.Public().(ed25519.PublicKey)),
	}}}
}
