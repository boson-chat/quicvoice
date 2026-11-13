package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/golang-jwt/jwt/v5"
)

func sign(t *testing.T, secret string, c Claims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJWTVerify(t *testing.T) {
	svc := NewJWTAuthService("s3cret")
	ctx := context.Background()
	exp := jwt.NewNumericDate(time.Now().Add(time.Minute))

	id, err := svc.Verify(ctx, sign(t, "s3cret", Claims{
		Name: "Alice", Role: "moderator", Rooms: []string{"lobby"},
		RegisteredClaims: jwt.RegisteredClaims{Subject: "u1", ExpiresAt: exp},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if id.UserID != "u1" || id.DisplayName != "Alice" || id.Role != controlpb.Role_MODERATOR {
		t.Fatalf("bad identity: %+v", id)
	}
	if !id.CanJoin("lobby") || id.CanJoin("other") {
		t.Fatal("room restriction not applied")
	}

	if _, err := svc.Verify(ctx, sign(t, "wrong", Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "u1", ExpiresAt: exp}})); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("wrong secret: got %v", err)
	}
	if _, err := svc.Verify(ctx, sign(t, "s3cret", Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "u1"}})); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("missing exp should fail: got %v", err)
	}
	past := jwt.NewNumericDate(time.Now().Add(-time.Minute))
	if _, err := svc.Verify(ctx, sign(t, "s3cret", Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "u1", ExpiresAt: past}})); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired: got %v", err)
	}
}
