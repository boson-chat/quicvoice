package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// JWKSAuthService verifies asymmetric tokens (EdDSA/ES256/RS256) minted by
// the boson auth service, using its published JWKS. Keys are cached, refreshed
// periodically, and refetched when a token arrives with an unknown kid (key
// rotation).
type JWKSAuthService struct {
	keys     keyfunc.Keyfunc
	issuer   string
	audience string
}

// NewJWKSAuthService starts the JWKS cache. A JWKS endpoint that is down at
// startup isn't fatal: verification fails until it can be fetched.
func NewJWKSAuthService(ctx context.Context, jwksURL, issuer, audience string) (*JWKSAuthService, error) {
	if jwksURL == "" {
		return nil, errors.New("auth: JWKS URL is empty")
	}
	k, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
	if err != nil {
		return nil, fmt.Errorf("auth: JWKS %s: %w", jwksURL, err)
	}
	return &JWKSAuthService{keys: k, issuer: issuer, audience: audience}, nil
}

func (s *JWKSAuthService) Verify(_ context.Context, token string) (*Identity, error) {
	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"EdDSA", "ES256", "RS256"}),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30 * time.Second),
	}
	if s.issuer != "" {
		opts = append(opts, jwt.WithIssuer(s.issuer))
	}
	if s.audience != "" {
		opts = append(opts, jwt.WithAudience(s.audience))
	}
	var c Claims
	_, err := jwt.ParseWithClaims(token, &c, s.keys.Keyfunc, opts...)
	return identityFromClaims(&c, err)
}

// CompositeAuthService routes by the token's alg: HS256 to the shared-secret
// verifier, anything else to JWKS. Lets a deployment move from JWT_SECRET to
// the auth service without a flag day.
type CompositeAuthService struct {
	HMAC AuthServiceImpl // may be nil
	JWKS AuthServiceImpl // may be nil
}

func (s *CompositeAuthService) Verify(ctx context.Context, token string) (*Identity, error) {
	alg := tokenAlg(token)
	switch {
	case alg == "HS256" && s.HMAC != nil:
		return s.HMAC.Verify(ctx, token)
	case alg != "HS256" && alg != "" && s.JWKS != nil:
		return s.JWKS.Verify(ctx, token)
	}
	return nil, ErrUnauthenticated
}

// tokenAlg reads the alg header without verifying anything.
func tokenAlg(token string) string {
	if strings.Count(token, ".") != 2 {
		return ""
	}
	t, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
	if err != nil {
		return ""
	}
	alg, _ := t.Header["alg"].(string)
	return alg
}
