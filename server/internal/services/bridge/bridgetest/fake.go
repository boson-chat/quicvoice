// Package bridgetest is a fake of evrd's managed API channel member query
// (GET /api/managed/channels/{channel}/members/{nick}), for tests.
package bridgetest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type Fake struct {
	Server *httptest.Server
	Token  string
	// Calls counts authorized requests.
	Calls atomic.Int64

	mu      sync.Mutex
	known   map[string]bool            // channels evrd is in (lowercased)
	members map[string]map[string]bool // channel → nick (lowercased)
	status  int                        // != 0: every request answers it
}

// New starts a fake evrd managed API that accepts Bearer token.
func New(t testing.TB, token string) *Fake {
	f := &Fake{Token: token, known: map[string]bool{}, members: map[string]map[string]bool{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

func (f *Fake) URL() string { return f.Server.URL }

// Watch makes evrd one of channel's members (it knows who's there).
func (f *Fake) Watch(channel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.known[strings.ToLower(channel)] = true
}

// Join puts nick in channel (and evrd with it).
func (f *Fake) Join(nick, channel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := strings.ToLower(channel)
	f.known[c] = true
	if f.members[c] == nil {
		f.members[c] = map[string]bool{}
	}
	f.members[c][strings.ToLower(nick)] = true
}

// Part takes nick out of channel.
func (f *Fake) Part(nick, channel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.members[strings.ToLower(channel)], strings.ToLower(nick))
}

// SetStatus makes every request answer code (0: back to normal).
func (f *Fake) SetStatus(code int) {
	f.mu.Lock()
	f.status = code
	f.mu.Unlock()
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	f.Calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status != 0 {
		http.Error(w, "boom", f.status)
		return
	}
	// /api/managed/channels/{channel}/members/{nick}, the channel's # escaped
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/managed/channels/"), "/members/")
	if r.Method != http.MethodGet || len(parts) != 2 || !strings.HasPrefix(r.URL.Path, "/api/managed/channels/") || !strings.HasPrefix(parts[0], "#") {
		http.NotFound(w, r)
		return
	}
	c, n := strings.ToLower(parts[0]), strings.ToLower(parts[1])
	present := f.members[c][n]
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"channel": parts[0], "nick": parts[1], "known": f.known[c], "present": present, "prefix": "", "mode": ""})
}
