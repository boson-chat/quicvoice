// Package settings reads the network settings the boson auth service keeps
// (changed at runtime from the admin panel) and caches them briefly.
//
//	GET {AUTH_INTERNAL_URL}/internal/v1/settings
//	X-Internal-Token: {INTERNAL_API_TOKEN}
//	→ {"voice_channel_creation":"admins|founders|registered", ...}
//
// A failed fetch keeps the last good value; before the first one the
// configured fallback is used.
package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/boson-chat/quicvoice/server/internal/logger"
)

// VoiceCreation is who may create voice rooms.
type VoiceCreation string

const (
	// Only auth admins (voice token role moderator/admin).
	CreateAdmins VoiceCreation = "admins"
	// Admins anywhere; others only under an IRC channel they founded.
	CreateFounders VoiceCreation = "founders"
	// Any authenticated user.
	CreateRegistered VoiceCreation = "registered"
)

// ParseVoiceCreation accepts the three values; anything else is an error.
func ParseVoiceCreation(s string) (VoiceCreation, error) {
	switch v := VoiceCreation(strings.ToLower(strings.TrimSpace(s))); v {
	case CreateAdmins, CreateFounders, CreateRegistered:
		return v, nil
	}
	return CreateAdmins, fmt.Errorf("settings: unknown voice_channel_creation %q", s)
}

type SettingsServiceImpl interface {
	// VoiceChannelCreation is the current policy. It never fails: on errors
	// it returns the last known (or fallback) value.
	VoiceChannelCreation(ctx context.Context) VoiceCreation
}

type Options struct {
	// Auth service internal base URL, e.g. http://auth:8081. Empty = always Fallback.
	URL   string
	Token string
	// Used until the first successful fetch, and always when URL is empty.
	Fallback VoiceCreation
	// How long a fetched value is reused. Default 30s.
	TTL     time.Duration
	Timeout time.Duration
	HTTP    *http.Client
	Now     func() time.Time
}

type SettingsService struct {
	opts Options

	mu      sync.Mutex
	current VoiceCreation
	fresh   time.Time // current is good until then
	// fetching: one request at a time; concurrent callers get the cached value.
	fetching bool
	// failing: the last fetch failed (warn once per outage, not per retry).
	failing bool
}

func NewSettingsService(o Options) SettingsServiceImpl {
	if o.Fallback == "" {
		o.Fallback = CreateAdmins
	}
	if o.TTL <= 0 {
		o.TTL = 30 * time.Second
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
	return &SettingsService{opts: o, current: o.Fallback}
}

// SetTTL changes how long a fetched value is reused (config file reload).
// <= 0 is ignored. The current value keeps its old expiry.
func (s *SettingsService) SetTTL(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	s.opts.TTL = d
	s.mu.Unlock()
}

func (s *SettingsService) VoiceChannelCreation(ctx context.Context) VoiceCreation {
	if s.opts.URL == "" {
		return s.opts.Fallback
	}
	now := s.opts.Now()
	s.mu.Lock()
	if now.Before(s.fresh) || s.fetching {
		v := s.current
		s.mu.Unlock()
		return v
	}
	s.fetching = true
	prev := s.current
	s.mu.Unlock()

	v, err := s.fetch(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetching = false
	if err != nil {
		// Retry soon, not on every request.
		s.fresh = now.Add(min(s.opts.TTL, 5*time.Second))
		ev := logger.FromCtx(ctx).Debug()
		if !s.failing {
			ev = logger.FromCtx(ctx).Warn()
		}
		ev.Err(err).Str("using", string(s.current)).Msg("auth settings fetch failed")
		s.failing = true
		return s.current
	}
	if s.failing {
		logger.FromCtx(ctx).Info().Msg("auth settings fetch recovered")
	}
	s.failing = false
	s.current, s.fresh = v, now.Add(s.opts.TTL)
	if v != prev {
		logger.FromCtx(ctx).Info().Str("voice_channel_creation", string(v)).Msg("voice channel creation policy")
	}
	return v
}

func (s *SettingsService) fetch(ctx context.Context) (VoiceCreation, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.opts.URL+"/internal/v1/settings", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Internal-Token", s.opts.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := s.opts.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("settings: HTTP %d", resp.StatusCode)
	}
	var body struct {
		VoiceChannelCreation string `json:"voice_channel_creation"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("settings: %w", err)
	}
	if body.VoiceChannelCreation == "" {
		// Older auth without the field: keep the configured fallback.
		return s.opts.Fallback, nil
	}
	return ParseVoiceCreation(body.VoiceChannelCreation)
}
