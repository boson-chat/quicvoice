// Package ergo asks the Ergo IRC server (its HTTP API, Ergo >= 2.14) which
// channels an account founded and which channel privileges it holds right now.
//
// Two endpoints of Ergo 2.19's API are used, both POST with a static bearer
// token from Ergo's `api.bearer-tokens`:
//
//   - /v1/ns/info {"accountName"} → "channels": the channels whose registered
//     (ChanServ) founder is that account. Ergo fills this from
//     ChannelsForAccount, which matches on the founder, so it works whether
//     or not the founder is online.
//   - /v1/whois {"nickname"} → the live client's channels, each with its
//     highest prefix mode (q a o h v). The stack sets
//     accounts.nick-reservation.force-nick-equals-account, so a logged-in
//     user's nick is their account name; the response's "account" is checked
//     anyway. Always-on clients are present while disconnected.
//
// Guests (no account, an unverified nick) are looked up by nick with
// /v1/whois alone (NickIn): only a client that isn't logged into any
// account counts, and only for channel membership.
//
// AMODE entries (ChanServ AMODE) are applied by Ergo when the user joins the
// channel, so they show up through /v1/whois while the user is in it. Ergo's
// API has no endpoint that lists a channel's AMODEs, so a user who isn't in
// the IRC channel only counts as founder (if they are) or member.
package ergo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/boson-chat/quicvoice/server/internal/roles"
)

// ErrUnavailable means Ergo couldn't be asked (not configured, unreachable,
// bad token, bad response). Callers fail closed.
var ErrUnavailable = errors.New("ergo: IRC role lookup unavailable")

// Standing is one account's view of IRC channels at a point in time.
type Standing struct {
	// Founded: casefolded names of channels the account is the registered founder of.
	Founded map[string]bool
	// Joined: casefolded channel → highest prefix mode the live client has there.
	Joined map[string]roles.Role
}

// In reports whether the account's client is currently in channel.
func (s *Standing) In(channel string) bool {
	if s == nil {
		return false
	}
	_, ok := s.Joined[Casefold(channel)]
	return ok
}

// Role is the account's role in channel: Founder if it's the registered
// founder (or holds +q), else its live prefix mode, else Member.
func (s *Standing) Role(channel string) roles.Role {
	if s == nil {
		return roles.Member
	}
	c := Casefold(channel)
	if s.Founded[c] {
		return roles.Founder
	}
	return s.Joined[c]
}

// FoundedChannels lists the founded channels (casefolded), unordered.
func (s *Standing) FoundedChannels() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Founded))
	for c := range s.Founded {
		out = append(out, c)
	}
	return out
}

// Casefold folds a channel name the way the stack's Ergo does
// (server.casemapping: ascii).
func Casefold(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

type ErgoServiceImpl interface {
	// Standing returns account's IRC channel standing, cached for a short TTL.
	// An unknown account is an empty Standing, not an error.
	Standing(ctx context.Context, account string) (*Standing, error)
	// NickIn reports whether the live client using nick, not logged into
	// any account, is in channel right now (an IRC guest's membership),
	// cached for the same TTL. A nick that's offline or logged in is false.
	NickIn(ctx context.Context, nick, channel string) (bool, error)
	// Enabled is false when no Ergo API is configured; Standing and NickIn
	// then always fail with ErrUnavailable.
	Enabled() bool
}

type Options struct {
	// Base URL of Ergo's API listener, e.g. http://ergo:8089.
	URL   string
	Token string
	// How long a Standing is reused. Default 15s.
	TTL     time.Duration
	Timeout time.Duration
	HTTP    *http.Client
	Now     func() time.Time
}

type ErgoService struct {
	opts Options

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	s   *Standing
	exp time.Time
}

// guestKey keys a guest nick's channels in the cache, apart from accounts
// (no account name starts with a space).
func guestKey(nick string) string { return " guest " + Casefold(nick) }

// NewErgoService returns a client for Ergo's API. An empty URL returns a
// disabled service.
func NewErgoService(o Options) ErgoServiceImpl {
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
	return &ErgoService{opts: o, cache: map[string]cacheEntry{}}
}

// SetTTL changes how long a Standing is reused (config file reload). <= 0
// is ignored. Cached entries keep their old expiry.
func (s *ErgoService) SetTTL(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	s.opts.TTL = d
	s.mu.Unlock()
}

func (s *ErgoService) Enabled() bool { return s.opts.URL != "" }

// maxCache bounds the cache; on overflow expired entries are dropped, then
// everything.
const maxCache = 4096

func (s *ErgoService) Standing(ctx context.Context, account string) (*Standing, error) {
	if !s.Enabled() {
		return nil, ErrUnavailable
	}
	if Casefold(account) == "" {
		return &Standing{Founded: map[string]bool{}, Joined: map[string]roles.Role{}}, nil
	}
	return s.cached(Casefold(account), func() (*Standing, error) { return s.fetch(ctx, account) })
}

func (s *ErgoService) NickIn(ctx context.Context, nick, channel string) (bool, error) {
	if !s.Enabled() {
		return false, ErrUnavailable
	}
	if Casefold(nick) == "" || channel == "" {
		return false, nil
	}
	st, err := s.cached(guestKey(nick), func() (*Standing, error) { return s.fetchGuest(ctx, nick) })
	if err != nil {
		return false, err
	}
	return st.In(channel), nil
}

// cached returns key's entry while it's fresh, else fetches and stores it.
func (s *ErgoService) cached(key string, fetch func() (*Standing, error)) (*Standing, error) {
	now := s.opts.Now()
	s.mu.Lock()
	if e, ok := s.cache[key]; ok && now.Before(e.exp) {
		s.mu.Unlock()
		return e.s, nil
	}
	s.mu.Unlock()

	st, err := fetch()
	if err != nil {
		return nil, err
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
	s.cache[key] = cacheEntry{s: st, exp: now.Add(s.opts.TTL)}
	s.mu.Unlock()
	return st, nil
}

// Forget drops account's cached standing (e.g. after a permission-relevant
// change). Not part of the interface; used by tests.
func (s *ErgoService) Forget(account string) {
	s.mu.Lock()
	delete(s.cache, Casefold(account))
	s.mu.Unlock()
}

func (s *ErgoService) fetch(ctx context.Context, account string) (*Standing, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	st := &Standing{Founded: map[string]bool{}, Joined: map[string]roles.Role{}}

	var info struct {
		Success  bool     `json:"success"`
		Channels []string `json:"channels"`
	}
	if err := s.post(ctx, "/v1/ns/info", map[string]string{"accountName": account}, &info); err != nil {
		return nil, err
	}
	// success=false: no such (verified, unsuspended) account → no channels.
	if info.Success {
		for _, c := range info.Channels {
			st.Founded[Casefold(c)] = true
		}
	}

	who, err := s.whois(ctx, account)
	if err != nil {
		return nil, err
	}
	// Only trust the live client if it is logged into this very account.
	if who.Success && who.Present && strings.EqualFold(who.Account, account) {
		for _, c := range who.Channels {
			st.Joined[Casefold(c.Name)] = roles.FromModeLetter(c.Mode)
		}
	}
	return st, nil
}

// fetchGuest: the channels of the client using nick, when it isn't logged
// into any account (Joined only, every one as a plain member: a guest
// never holds a channel role here). A logged-in client with that nick is
// someone else.
func (s *ErgoService) fetchGuest(ctx context.Context, nick string) (*Standing, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	who, err := s.whois(ctx, nick)
	if err != nil {
		return nil, err
	}
	st := &Standing{Founded: map[string]bool{}, Joined: map[string]roles.Role{}}
	if who.Success && who.Present && who.Account == "" {
		for _, c := range who.Channels {
			st.Joined[Casefold(c.Name)] = roles.Member
		}
	}
	return st, nil
}

type whoisReply struct {
	Success  bool   `json:"success"`
	Present  bool   `json:"present"`
	Account  string `json:"account"`
	Channels []struct {
		Name string `json:"name"`
		Mode string `json:"mode"`
	} `json:"channels"`
}

func (s *ErgoService) whois(ctx context.Context, nick string) (*whoisReply, error) {
	var who whoisReply
	if err := s.post(ctx, "/v1/whois", map[string]string{"nickname": nick}, &who); err != nil {
		return nil, err
	}
	return &who, nil
}

func (s *ErgoService) post(ctx context.Context, path string, body, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.opts.URL+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.opts.Token)
	resp, err := s.opts.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s: HTTP %d", ErrUnavailable, path, resp.StatusCode)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnavailable, path, err)
	}
	return nil
}
