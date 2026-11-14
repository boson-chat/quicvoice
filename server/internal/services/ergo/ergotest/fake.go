// Package ergotest is a fake of the two Ergo 2.19 API endpoints quicvoiced
// uses (/v1/ns/info and /v1/whois), for tests.
package ergotest

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
	// Calls counts authorized API requests.
	Calls atomic.Int64

	mu       sync.Mutex
	founders map[string]string            // channel (casefolded) → founder account (casefolded)
	online   map[string]map[string]string // account → channel → mode letter ("" = plain member)
	down     bool
}

// New starts a fake Ergo API that accepts Bearer token.
func New(t testing.TB, token string) *Fake {
	f := &Fake{Token: token, founders: map[string]string{}, online: map[string]map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

func (f *Fake) URL() string { return f.Server.URL }

// Register makes account the ChanServ founder of channel.
func (f *Fake) Register(channel, account string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.founders[strings.ToLower(channel)] = strings.ToLower(account)
}

// Join puts account's client in channel with prefix mode ("q","a","o","h","v" or "").
func (f *Fake) Join(account, channel, mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := strings.ToLower(account)
	if f.online[a] == nil {
		f.online[a] = map[string]string{}
	}
	f.online[a][strings.ToLower(channel)] = mode
}

// Part removes account's client from channel.
func (f *Fake) Part(account, channel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.online[strings.ToLower(account)], strings.ToLower(channel))
}

// SetDown makes every request fail with 500.
func (f *Fake) SetDown(down bool) {
	f.mu.Lock()
	f.down = down
	f.mu.Unlock()
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+f.Token {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	f.Calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	var req struct {
		AccountName string `json:"accountName"`
		Nickname    string `json:"nickname"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/ns/info":
		acct := strings.ToLower(req.AccountName)
		chans := []string{}
		for c, founder := range f.founders {
			if founder == acct {
				chans = append(chans, c)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": acct != "", "accountName": req.AccountName, "channels": chans})
	case "/v1/whois":
		nick := strings.ToLower(req.Nickname)
		chans, ok := f.online[nick]
		if !ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "present": false, "account": "", "channels": nil})
			return
		}
		list := []map[string]string{}
		for c, m := range chans {
			list = append(list, map[string]string{"name": c, "mode": m, "join_time": "2026-01-01T00:00:00.000Z"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "present": true, "nickname": req.Nickname,
			"account": req.Nickname, "channels": list, "session_count": 1})
	default:
		http.NotFound(w, r)
	}
}
