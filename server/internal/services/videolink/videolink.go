// Package videolink keeps quicvideod's video rooms in step with voice
// channels: it implements room.Observer, sends leaves and closed channels
// to quicvideod's control API as they happen (batched, retried), and a full
// membership snapshot every SyncInterval so anything missed heals.
package videolink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/boson-chat/quicvoice/server/internal/logger"
)

type event struct {
	Type string `json:"type"` // "leave" | "close"
	Room string `json:"room"`
	User string `json:"user,omitempty"`
}

// Options configure the link.
type Options struct {
	URL          string // quicvideod control listener, e.g. http://quicvideod:8091
	Token        string
	SyncInterval time.Duration // default 30s
	Snapshot     func() map[string][]string
	Client       *http.Client
}

type VideoLinkService struct {
	o      Options
	mu     sync.Mutex
	queue  []event
	notify chan struct{}
}

func New(o Options) *VideoLinkService {
	if o.SyncInterval <= 0 {
		o.SyncInterval = 30 * time.Second
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 5 * time.Second}
	}
	o.URL = strings.TrimRight(o.URL, "/")
	return &VideoLinkService{o: o, notify: make(chan struct{}, 1)}
}

func (l *VideoLinkService) push(e event) {
	l.mu.Lock()
	if len(l.queue) < 10_000 { // bounded: the snapshot heals anything dropped
		l.queue = append(l.queue, e)
	}
	l.mu.Unlock()
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

// MemberLeft implements room.Observer.
func (l *VideoLinkService) MemberLeft(room, userID string) {
	l.push(event{Type: "leave", Room: room, User: userID})
}

// RoomClosed implements room.Observer.
func (l *VideoLinkService) RoomClosed(room string) { l.push(event{Type: "close", Room: room}) }

// Run sends events and snapshots until ctx ends.
func (l *VideoLinkService) Run(ctx context.Context) {
	log := logger.FromCtx(ctx)
	tick := time.NewTicker(l.o.SyncInterval)
	defer tick.Stop()
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.notify:
		case <-tick.C:
			if l.o.Snapshot != nil {
				if err := l.send(ctx, http.MethodPut, "/v1/control/rooms", map[string]any{"rooms": l.o.Snapshot()}); err != nil {
					log.Warn().Err(err).Msg("video link: snapshot")
				}
			}
		}
		l.mu.Lock()
		batch := l.queue
		l.queue = nil
		l.mu.Unlock()
		if len(batch) == 0 {
			continue
		}
		if err := l.send(ctx, http.MethodPost, "/v1/control/events", map[string]any{"events": batch}); err != nil {
			log.Warn().Err(err).Int("events", len(batch)).Msg("video link: events (retrying)")
			l.mu.Lock()
			l.queue = append(batch, l.queue...)
			l.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			select {
			case l.notify <- struct{}{}:
			default:
			}
			continue
		}
		backoff = time.Second
	}
}

func (l *VideoLinkService) send(ctx context.Context, method, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, l.o.URL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.o.Token)
	resp, err := l.o.Client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return nil
}
