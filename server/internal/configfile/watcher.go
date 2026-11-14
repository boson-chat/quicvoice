// Package configfile watches the boson stack config file (BOSON_CONFIG,
// e.g. /config/boson.yaml) and hands each new version to a Loader.
//
// The file is replaced atomically by the admin panel (tmp file + rename in
// the same dir), which swaps the inode, so the watcher listens on the
// directory with fsnotify and also polls the file's mtime/size every
// PollInterval as a fallback (Docker Desktop bind mounts, missed events).
// Bursts of events are debounced. An invalid file keeps the last good
// config; Status reports what happened.
package configfile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"
)

const (
	DefaultPollInterval = 10 * time.Second
	DefaultDebounce     = 250 * time.Millisecond
)

// Loader parses, validates and applies one version of the file. data is nil
// when the file doesn't exist (the service then runs on env and defaults).
// On an error nothing may have been applied. restartRequired lists the keys
// that changed but only take effect after a restart.
type Loader func(ctx context.Context, data []byte) (restartRequired []string, err error)

// Status is GET /internal/config/status.
type Status struct {
	// Path is BOSON_CONFIG ("" = no config file: env and defaults only).
	Path string `json:"path"`
	// LoadedAt: when the config in use was loaded (nil = never).
	LoadedAt *time.Time `json:"loaded_at"`
	// FileMtime of the file last read (nil = missing).
	FileMtime *time.Time `json:"file_mtime"`
	// OK is false when the file on disk is invalid (the last good config
	// stays in use).
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// ErrorLine/ErrorColumn locate Error in the file when known (1-based).
	ErrorLine   int `json:"error_line,omitempty"`
	ErrorColumn int `json:"error_column,omitempty"`
	// RestartRequired: keys that changed but need a restart.
	RestartRequired []string `json:"restart_required"`
}

// PosError is a validation error at a place in the file.
type PosError struct {
	Line, Column int
	Msg          string
}

func (e *PosError) Error() string {
	switch {
	case e.Line > 0 && e.Column > 0:
		return fmt.Sprintf("line %d, column %d: %s", e.Line, e.Column, e.Msg)
	case e.Line > 0:
		return fmt.Sprintf("line %d: %s", e.Line, e.Msg)
	}
	return e.Msg
}

// Errorf makes a PosError.
func Errorf(line, column int, format string, args ...any) error {
	return &PosError{Line: line, Column: column, Msg: fmt.Sprintf(format, args...)}
}

var lineRe = regexp.MustCompile(`line (\d+)(?:, column (\d+))?`)

// Position finds the line (and column) of err: a PosError, or a YAML
// parser/decoder message ("yaml: line 3: ...").
func Position(err error) (line, column int) {
	var pe *PosError
	if errors.As(err, &pe) {
		return pe.Line, pe.Column
	}
	if m := lineRe.FindStringSubmatch(err.Error()); m != nil {
		line, _ = strconv.Atoi(m[1])
		column, _ = strconv.Atoi(m[2])
	}
	return line, column
}

type Watcher struct {
	path         string
	load         Loader
	log          zerolog.Logger
	pollInterval time.Duration
	debounce     time.Duration

	reloadMu sync.Mutex // one reload at a time
	mu       sync.Mutex
	status   Status
	applied  [32]byte // hash of the content last applied
	hasApply bool
	seen     fileStamp // what the poller last saw
}

type fileStamp struct {
	exists bool
	mtime  time.Time
	size   int64
}

type Option func(*Watcher)

func WithPollInterval(d time.Duration) Option { return func(w *Watcher) { w.pollInterval = d } }
func WithDebounce(d time.Duration) Option     { return func(w *Watcher) { w.debounce = d } }
func WithLogger(l zerolog.Logger) Option      { return func(w *Watcher) { w.log = l } }

// New watches path ("" = no config file: Reload applies an empty config
// once and Run does nothing).
func New(path string, load Loader, opts ...Option) *Watcher {
	w := &Watcher{path: path, load: load, log: zerolog.Nop(), pollInterval: DefaultPollInterval, debounce: DefaultDebounce}
	for _, o := range opts {
		o(w)
	}
	w.status = Status{Path: path, OK: true, RestartRequired: []string{}}
	return w
}

// Status returns a copy of the current status.
func (w *Watcher) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.status
	st.RestartRequired = append([]string{}, st.RestartRequired...)
	return st
}

func stamp(fi fs.FileInfo) fileStamp {
	return fileStamp{exists: true, mtime: fi.ModTime(), size: fi.Size()}
}

// Reload reads the file and, when its content changed since the last
// successful load, hands it to the Loader. It returns the Loader's error
// (or the read error).
func (w *Watcher) Reload(ctx context.Context) error {
	w.reloadMu.Lock()
	defer w.reloadMu.Unlock()

	var data []byte
	var st fileStamp
	if w.path != "" {
		b, err := os.ReadFile(w.path)
		switch {
		case err == nil:
			data = b
			if data == nil {
				data = []byte{}
			}
			if fi, err := os.Stat(w.path); err == nil {
				st = stamp(fi)
			}
		case errors.Is(err, fs.ErrNotExist):
		default:
			w.fail(st, err)
			return err
		}
	}
	sum := [32]byte{} // missing file
	if data != nil {
		sum = sha256.Sum256(data)
	}

	w.mu.Lock()
	w.seen = st
	unchanged := w.hasApply && bytes.Equal(sum[:], w.applied[:]) && w.status.OK
	if unchanged {
		w.status.FileMtime = mtimePtr(st)
	}
	w.mu.Unlock()
	if unchanged {
		return nil
	}

	restart, err := w.load(ctx, data)
	if err != nil {
		w.fail(st, err)
		return err
	}
	now := time.Now().UTC()
	if restart == nil {
		restart = []string{}
	}
	w.mu.Lock()
	first := !w.hasApply
	w.applied, w.hasApply = sum, true
	w.status = Status{Path: w.path, LoadedAt: &now, FileMtime: mtimePtr(st), OK: true, RestartRequired: restart}
	w.mu.Unlock()
	ev := w.log.Info().Str("path", w.path).Strs("restart_required", restart).Bool("file", data != nil)
	if first {
		ev.Msg("config loaded")
	} else {
		ev.Msg("config reloaded")
	}
	return nil
}

func (w *Watcher) fail(st fileStamp, err error) {
	line, col := Position(err)
	w.mu.Lock()
	w.status.OK = false
	w.status.Error = err.Error()
	w.status.ErrorLine, w.status.ErrorColumn = line, col
	w.status.FileMtime = mtimePtr(st)
	w.mu.Unlock()
	w.log.Error().Err(err).Str("path", w.path).Int("line", line).Int("column", col).
		Msg("config file invalid; keeping the last good config")
}

func mtimePtr(st fileStamp) *time.Time {
	if !st.exists {
		return nil
	}
	t := st.mtime.UTC()
	return &t
}

// Run watches the file until ctx is done. Call Reload once first.
func (w *Watcher) Run(ctx context.Context) {
	if w.path == "" {
		return
	}
	dir, base := filepath.Dir(w.path), filepath.Base(w.path)
	var events <-chan fsnotify.Event
	var errs <-chan error
	if fw, err := fsnotify.NewWatcher(); err != nil {
		w.log.Warn().Err(err).Msg("config file: fsnotify unavailable; polling only")
	} else {
		defer fw.Close()
		if err := fw.Add(dir); err != nil {
			w.log.Warn().Err(err).Str("dir", dir).Msg("config file: can't watch the directory; polling only")
		} else {
			events, errs = fw.Events, fw.Errors
		}
	}
	poll := time.NewTicker(w.pollInterval)
	defer poll.Stop()
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	defer debounce.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if filepath.Base(ev.Name) == base {
				debounce.Reset(w.debounce)
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			w.log.Warn().Err(err).Msg("config file watch")
		case <-debounce.C:
			_ = w.Reload(ctx)
		case <-poll.C:
			if w.changedOnDisk() {
				_ = w.Reload(ctx)
			}
		}
	}
}

func (w *Watcher) changedOnDisk() bool {
	var now fileStamp
	if fi, err := os.Stat(w.path); err == nil {
		now = stamp(fi)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return now != w.seen
}
