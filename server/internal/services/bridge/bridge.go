// Package bridge asks evrd, the IRC bridge of an add-on install (Boson in
// front of someone else's IRC server, no Ergo), whether a nick is in a
// channel: evrd's managed API GET
// /api/managed/channels/{channel}/members/{nick} (evrd >= the release with
// 4evrd/evrd#185), with evrd's managed_token (the stack's BRIDGE_TOKEN).
//
// quicvoiced uses it for IRC guests' channel membership (voice rooms with
// require_channel_membership) where there is no Ergo API to ask.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrUnavailable means evrd couldn't be asked (not configured, unreachable,
// bad token, an evrd without the endpoint, bad response). Callers fail
// closed.
var ErrUnavailable = errors.New("bridge: IRC membership lookup unavailable")

type BridgeServiceImpl interface {
	// NickIn reports whether nick is in channel right now, cached for a
	// short TTL. A channel none of evrd's connections is in is false
	// (nobody can tell).
	NickIn(ctx context.Context, nick, channel string) (bool, error)
	Enabled() bool
}

type Options struct {
	// Base URL of evrd's managed API, e.g. http://evrd-links:8098.
	URL   string
	Token string
	// How long an answer is reused. Default 15s.
	TTL     time.Duration
	Timeout time.Duration
	HTTP    *http.Client
	Now     func() time.Time
}

type BridgeService struct {
	opts Options

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	in  bool
	exp time.Time
}

// maxCache bounds the cache; on overflow expired entries are dropped, then
// everything.
const maxCache = 4096

// NewBridgeService returns a client for evrd's managed API. An empty URL or
// token returns a disabled service.
func NewBridgeService(o Options) *BridgeService {
	if o.TTL <= 0 {
		o.TTL = 15 * time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 3 * time.Second
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: o.Timeout}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	o.URL = strings.TrimRight(o.URL, "/")
	return &BridgeService{opts: o, cache: map[string]cacheEntry{}}
}

func (s *BridgeService) Enabled() bool { return s.opts.URL != "" && s.opts.Token != "" }

// SetTTL changes how long an answer is reused (config file reload). <= 0
// is ignored.
func (s *BridgeService) SetTTL(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	s.opts.TTL = d
	s.mu.Unlock()
}

// fold lowercases ASCII (and rfc1459's []\~ as {}|^), as most IRC servers
// casemap: a cache key only, evrd does its own matching.
func fold(s string) string {
	return strings.NewReplacer("[", "{", "]", "}", "\\", "|", "~", "^").Replace(strings.ToLower(s))
}

func (s *BridgeService) NickIn(ctx context.Context, nick, channel string) (bool, error) {
	if !s.Enabled() {
		return false, ErrUnavailable
	}
	nick, channel = strings.TrimSpace(nick), strings.TrimSpace(channel)
	if nick == "" || channel == "" {
		return false, nil
	}
	key := fold(channel) + " " + fold(nick)
	now := s.opts.Now()
	s.mu.Lock()
	if e, ok := s.cache[key]; ok && now.Before(e.exp) {
		s.mu.Unlock()
		return e.in, nil
	}
	s.mu.Unlock()

	in, err := s.fetch(ctx, nick, channel)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	if len(s.cache) >= maxCache {
		for k, e := range s.cache {
			if !now.Before(e.exp) {
				delete(s.cache, k)
			}
		}
		if len(s.cache) >= maxCache {
			clear(s.cache)
		}
	}
	s.cache[key] = cacheEntry{in: in, exp: now.Add(s.opts.TTL)}
	s.mu.Unlock()
	return in, nil
}

func (s *BridgeService) fetch(ctx context.Context, nick, channel string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	u := s.opts.URL + "/api/managed/channels/" + url.PathEscape(channel) + "/members/" + url.PathEscape(nick)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.opts.Token)
	resp, err := s.opts.HTTP.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		// 404 too: an evrd without the endpoint.
		return false, fmt.Errorf("%w: HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	var out struct {
		Known   *bool `json:"known"`
		Present bool  `json:"present"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Known == nil {
		return false, fmt.Errorf("%w: unexpected answer", ErrUnavailable)
	}
	return *out.Known && out.Present, nil
}
