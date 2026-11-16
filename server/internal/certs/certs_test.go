package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func writePair(t *testing.T, certFile, keyFile string) {
	t.Helper()
	c, err := SelfSigned("video.test")
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(c.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	// Key first, like certsync: a reader that sees the cert sees its key.
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]}), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The certificate shows up after startup (certsync on a fresh stack):
// wait for it instead of failing.
func TestWaitForFilesThenLoad(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	var waits atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- WaitForFiles(context.Background(), certFile, keyFile, 10*time.Millisecond, func(error) { waits.Add(1) })
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("returned before the files existed: %v", err)
	default:
	}
	writePair(t, certFile, keyFile)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("didn't notice the files")
	}
	if waits.Load() != 1 {
		t.Fatalf("logged the wait %d times, want once (then at most once a minute)", waits.Load())
	}
	src, err := NewFileSource(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := src.GetCertificate(nil); c == nil {
		t.Fatal("no certificate served")
	}
}

func TestWaitForFilesStopsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if err := WaitForFiles(ctx, "/nonexistent/tls.crt", "/nonexistent/tls.key", 10*time.Millisecond, nil); err == nil {
		t.Fatal("returned nil after cancel")
	}
}
