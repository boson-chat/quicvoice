package configfile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorder struct {
	mu    sync.Mutex
	loads []string // "<missing>" for nil
}

func (r *recorder) load(_ context.Context, data []byte) ([]string, error) {
	s := string(data)
	if data == nil {
		s = "<missing>"
	}
	if strings.Contains(s, "bad") {
		return nil, Errorf(2, 5, "bad value")
	}
	r.mu.Lock()
	r.loads = append(r.loads, s)
	r.mu.Unlock()
	if strings.Contains(s, "port") {
		return []string{"x.port"}, nil
	}
	return nil, nil
}

func (r *recorder) last() (string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.loads) == 0 {
		return "", 0
	}
	return r.loads[len(r.loads)-1], len(r.loads)
}

func replace(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", what)
}

func TestWatcherReloadsOnAtomicReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boson.yaml")
	replace(t, path, "a")
	r := &recorder{}
	// Long poll: changes must arrive through fsnotify.
	w := New(path, r.load, WithDebounce(10*time.Millisecond), WithPollInterval(time.Hour))
	if err := w.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	replace(t, path, "b")
	waitFor(t, "reload b", func() bool { s, _ := r.last(); return s == "b" })
	st := w.Status()
	if !st.OK || st.LoadedAt == nil || st.FileMtime == nil || len(st.RestartRequired) != 0 {
		t.Fatalf("status %+v", st)
	}

	// Invalid: the last good config stays, the status has the position.
	replace(t, path, "bad")
	waitFor(t, "status not ok", func() bool { return !w.Status().OK })
	st = w.Status()
	if st.Error != "line 2, column 5: bad value" || st.ErrorLine != 2 || st.ErrorColumn != 5 || st.LoadedAt == nil {
		t.Fatalf("invalid status %+v", st)
	}
	if s, _ := r.last(); s != "b" {
		t.Fatalf("last applied %q", s)
	}

	// Fixed, with a restart-only change.
	replace(t, path, "port")
	waitFor(t, "reload port", func() bool { s, _ := r.last(); return s == "port" })
	waitFor(t, "status ok", func() bool { return w.Status().OK })
	if st := w.Status(); st.Error != "" || len(st.RestartRequired) != 1 || st.RestartRequired[0] != "x.port" {
		t.Fatalf("status %+v", st)
	}

	// Same content again (touch): not re-applied.
	_, n := r.last()
	replace(t, path, "port")
	time.Sleep(100 * time.Millisecond)
	if _, n2 := r.last(); n2 != n {
		t.Fatalf("unchanged content applied again (%d loads, was %d)", n2, n)
	}

	// Removed: the loader gets nil (env and defaults).
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "missing", func() bool { s, _ := r.last(); return s == "<missing>" })
	if st := w.Status(); !st.OK || st.FileMtime != nil {
		t.Fatalf("status %+v", st)
	}
}

func TestWatcherPollFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "boson.yaml")
	replace(t, path, "a")
	r := &recorder{}
	// Debounce longer than the test: only the poller can see the change.
	w := New(path, r.load, WithDebounce(time.Hour), WithPollInterval(20*time.Millisecond))
	_ = w.Reload(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	time.Sleep(30 * time.Millisecond)
	replace(t, path, "bb")
	waitFor(t, "poll reload", func() bool { s, _ := r.last(); return s == "bb" })
}

func TestWatcherNoPath(t *testing.T) {
	r := &recorder{}
	w := New("", r.load)
	if err := w.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s, _ := r.last(); s != "<missing>" {
		t.Fatalf("loaded %q", s)
	}
	if st := w.Status(); !st.OK || st.Path != "" || st.LoadedAt == nil {
		t.Fatalf("status %+v", st)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(ctx) // returns at once
}

func TestWatcherInvalidAtStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boson.yaml")
	replace(t, path, "bad")
	w := New(path, (&recorder{}).load)
	if err := w.Reload(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if st := w.Status(); st.OK || st.LoadedAt != nil || st.ErrorLine != 2 {
		t.Fatalf("status %+v", st)
	}
}

func TestPosition(t *testing.T) {
	for _, c := range []struct {
		err       error
		line, col int
	}{
		{Errorf(3, 4, "x"), 3, 4},
		{errors.New("yaml: line 7: did not find expected key"), 7, 0},
		{errors.New("yaml: unmarshal errors:\n  line 2: cannot unmarshal"), 2, 0},
		{errors.New("nothing"), 0, 0},
	} {
		if l, c2 := Position(c.err); l != c.line || c2 != c.col {
			t.Fatalf("%v: %d:%d", c.err, l, c2)
		}
	}
}
