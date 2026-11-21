// Package jwks keeps the auth service's JWK Set cached for JWT verification
// and survives the auth service being down when this service starts.
//
// keyfunc/jwkset alone (NewDefaultCtx) fetch once at startup, then only
// every hour, plus at most once per 5 minutes on an unknown kid. When this
// service came up before auth was listening, every signed-in user's token was
// rejected for minutes. A Cache instead:
//
//   - retries the initial fetch in the background with backoff (1 s doubling
//     to 30 s) until it succeeds, without blocking or failing startup;
//   - then refreshes on the normal interval (1 h);
//   - refetches promptly when a token's kid is unknown (or nothing is loaded
//     yet), at most once per 10 s; tokens arriving during that fetch wait
//     for it instead of failing.
//
// jwkset has no initial-retry option, and its unknown-kid refresh makes
// callers wait on the rate limiter (and counts redundant refetches), so the
// Cache drives the fetches itself and uses jwkset/keyfunc for parsing,
// storage and key selection.
package jwks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog"
)

// Defaults for Options.
const (
	DefaultRefreshInterval = time.Hour
	DefaultUnknownKIDEvery = 10 * time.Second
	DefaultHTTPTimeout     = 5 * time.Second
	DefaultMinBackoff      = time.Second
	DefaultMaxBackoff      = 30 * time.Second
	DefaultSettle          = 2 * time.Second
)

// Options tune a Cache. Zero values take the defaults above.
type Options struct {
	// RefreshInterval: periodic refetch once the keys have loaded.
	RefreshInterval time.Duration
	// UnknownKIDEvery: the least time between refetches triggered by a token
	// with an unknown kid (burst 1). The background load and refresh don't
	// count against it.
	UnknownKIDEvery time.Duration
	// HTTPTimeout bounds each fetch.
	HTTPTimeout time.Duration
	// MinBackoff and MaxBackoff: retry delays while the first fetch fails.
	MinBackoff, MaxBackoff time.Duration
	// Settle: before the first load, how long a token the unknown-kid rate
	// limit turns away waits for the background retry to bring its key in.
	// Negative = don't wait.
	Settle time.Duration
	// Client makes the requests (default: a client with HTTPTimeout).
	Client *http.Client
	// Logger (default: zerolog.Ctx of the context passed to New).
	Logger *zerolog.Logger
}

// Cache is a JWK Set cache over one or more JWKS URLs (keys are matched by
// kid across all of them). Use Keyfunc with jwt.Parse.
type Cache struct {
	ctx     context.Context
	opts    Options
	log     *zerolog.Logger
	sources []*source
	kf      keyfunc.Keyfunc

	unknownMu   sync.Mutex
	lastUnknown time.Time // last unknown-kid fetch that went out
}

type source struct {
	url     string
	store   jwkset.Storage
	loaded  atomic.Bool
	fetchMu sync.Mutex
}

// New starts the cache: one background goroutine per URL that loads the
// keys (retrying until it succeeds) and then refreshes them. It never
// blocks on the network and fails only on a malformed URL. The goroutines
// end with ctx.
func New(ctx context.Context, urls []string, o Options) (*Cache, error) {
	if len(urls) == 0 {
		return nil, errors.New("jwks: no JWKS URL")
	}
	if o.RefreshInterval <= 0 {
		o.RefreshInterval = DefaultRefreshInterval
	}
	if o.UnknownKIDEvery <= 0 {
		o.UnknownKIDEvery = DefaultUnknownKIDEvery
	}
	if o.HTTPTimeout <= 0 {
		o.HTTPTimeout = DefaultHTTPTimeout
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = DefaultMinBackoff
	}
	if o.MaxBackoff < o.MinBackoff {
		o.MaxBackoff = max(DefaultMaxBackoff, o.MinBackoff)
	}
	if o.Settle < 0 {
		o.Settle = 0
	} else if o.Settle == 0 {
		o.Settle = DefaultSettle
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: o.HTTPTimeout}
	}
	lg := o.Logger
	if lg == nil {
		lg = zerolog.Ctx(ctx)
	}
	c := &Cache{
		ctx:  ctx,
		opts: o,
		log:  lg,
	}
	stores := make(map[string]jwkset.Storage, len(urls))
	for _, u := range urls {
		if _, err := url.ParseRequestURI(u); err != nil {
			return nil, fmt.Errorf("jwks: URL %q: %w", u, err)
		}
		if _, dup := stores[u]; dup {
			continue
		}
		s := &source{url: u, store: jwkset.NewMemoryStorage()}
		stores[u] = s.store
		c.sources = append(c.sources, s)
	}
	// No RefreshUnknownKID: the plain memory stores never fetch; Keyfunc
	// triggers fetches itself.
	combined, err := jwkset.NewHTTPClient(jwkset.HTTPClientOptions{HTTPURLs: stores})
	if err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	c.kf, err = keyfunc.New(keyfunc.Options{Ctx: ctx, Storage: combined})
	if err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	for _, s := range c.sources {
		go c.run(s)
	}
	return c, nil
}

// Ready reports whether every JWKS URL has loaded at least once.
func (c *Cache) Ready() bool {
	for _, s := range c.sources {
		if !s.loaded.Load() {
			return false
		}
	}
	return true
}

// Keyfunc is a jwt.Keyfunc. A token whose kid isn't cached (key rotation, or
// the keys not loaded yet) triggers a refetch, at most one per
// UnknownKIDEvery. Tokens arriving during that refetch wait for it rather
// than fail; while the first load is still pending, a token the rate limit
// turns away waits up to Settle for the background retry.
func (c *Cache) Keyfunc(t *jwt.Token) (any, error) {
	key, err := c.kf.Keyfunc(t)
	if err == nil {
		return key, nil
	}
	kid, _ := t.Header[jwkset.HeaderKID].(string)
	if kid == "" {
		return nil, err
	}
	if c.has(kid) {
		// Arrived since the first try (another token's fetch), or present
		// and rejected (e.g. alg mismatch), which a refetch won't change.
		return c.kf.Keyfunc(t)
	}
	if !c.refetchFor(kid) && !(!c.Ready() && c.await(kid)) {
		return nil, err
	}
	return c.kf.Keyfunc(t)
}

// refetchFor refetches the JWKS for an unknown kid, rate-limited, and
// reports whether kid is now known. Callers queue on unknownMu, so tokens
// arriving together share one fetch.
func (c *Cache) refetchFor(kid string) bool {
	c.unknownMu.Lock()
	defer c.unknownMu.Unlock()
	if c.has(kid) {
		return true // fetched while we queued
	}
	if !c.lastUnknown.IsZero() && time.Since(c.lastUnknown) < c.opts.UnknownKIDEvery {
		return false
	}
	for _, s := range c.sources {
		wasLoaded := s.loaded.Load()
		fetched, err := c.fetchUnless(s, kid)
		// The fetch that first loads a source is free: otherwise a token
		// racing the background load would block a key rotation for the
		// next UnknownKIDEvery. Failed fetches count, so an issuer that's
		// down isn't hit once per token.
		if fetched && (wasLoaded || err != nil) {
			c.lastUnknown = time.Now()
		}
		if err != nil {
			c.log.Debug().Err(err).Str("url", s.url).Str("kid", kid).Msg("JWKS refetch for unknown kid failed")
		}
		if c.has(kid) {
			return true
		}
	}
	return false
}

// await polls for kid for up to Settle.
func (c *Cache) await(kid string) bool {
	if c.opts.Settle <= 0 {
		return false
	}
	tick := time.NewTicker(min(50*time.Millisecond, c.opts.Settle))
	defer tick.Stop()
	deadline := time.NewTimer(c.opts.Settle)
	defer deadline.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return false
		case <-deadline.C:
			return c.has(kid)
		case <-tick.C:
			if c.has(kid) {
				return true
			}
		}
	}
}

func (c *Cache) has(kid string) bool {
	for _, s := range c.sources {
		if _, err := s.store.KeyRead(c.ctx, kid); err == nil {
			return true
		}
	}
	return false
}

// run loads s with backoff, then refreshes it every RefreshInterval.
func (c *Cache) run(s *source) {
	delay := c.opts.MinBackoff
	warned := false
	for !s.loaded.Load() {
		err := c.load(s)
		if err == nil || s.loaded.Load() {
			break
		}
		if c.ctx.Err() != nil {
			return
		}
		if !warned {
			warned = true
			c.log.Warn().Err(err).Str("url", s.url).
				Msg("JWKS not reachable yet; retrying in the background (signed-in tokens fail until it loads)")
		} else {
			c.log.Debug().Err(err).Str("url", s.url).Dur("retry_in", delay).Msg("JWKS still not reachable")
		}
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, c.opts.MaxBackoff)
	}
	tick := time.NewTicker(c.opts.RefreshInterval)
	defer tick.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-tick.C:
			if err := c.fetch(s); err != nil && c.ctx.Err() == nil {
				c.log.Warn().Err(err).Str("url", s.url).Msg("JWKS refresh failed; keeping the cached keys")
			}
		}
	}
}

// load fetches s unless it has loaded meanwhile (e.g. by a token's
// unknown-kid fetch).
func (c *Cache) load(s *source) error {
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	if s.loaded.Load() {
		return nil
	}
	return c.doFetch(s)
}

// fetch refetches s (see doFetch).
func (c *Cache) fetch(s *source) error {
	_, err := c.fetchUnless(s, "")
	return err
}

// fetchUnless is fetch, skipped (fetched=false) when kid is non-empty and
// already cached once s's in-flight fetch, if any, has finished.
func (c *Cache) fetchUnless(s *source, kid string) (fetched bool, err error) {
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	if kid != "" && c.has(kid) {
		return false, nil
	}
	return true, c.doFetch(s)
}

// doFetch downloads s's JWK Set and replaces its cached keys. An empty set
// is an error (and leaves the cache alone): the issuer has nothing to verify
// with yet. Call with s.fetchMu held.
func (c *Cache) doFetch(s *source) error {
	ctx, cancel := context.WithTimeout(c.ctx, c.opts.HTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return err
	}
	resp, err := c.opts.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%w: %d", jwkset.ErrInvalidHTTPStatusCode, resp.StatusCode)
	}
	var set jwkset.JWKSMarshal
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return fmt.Errorf("decode JWK Set: %w", err)
	}
	keys := make([]jwkset.JWK, 0, len(set.Keys))
	for _, m := range set.Keys {
		k, err := jwkset.NewJWKFromMarshal(m, jwkset.JWKMarshalOptions{}, jwkset.JWKValidateOptions{})
		switch {
		case errors.Is(err, jwkset.ErrUnsupportedKey):
			continue
		case err != nil:
			return fmt.Errorf("JWK %q: %w", m.KID, err)
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return errors.New("JWK Set has no usable keys")
	}
	if err := s.store.KeyReplaceAll(ctx, keys); err != nil {
		return err
	}
	if s.loaded.CompareAndSwap(false, true) {
		c.log.Info().Str("url", s.url).Int("keys", len(keys)).Msg("JWKS loaded")
	}
	return nil
}
