// Package certs provides the server TLS certificate: from files (reloaded when
// cert-manager rotates them) or a self-signed one for local dev.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"os"
	"sync"
	"time"
)

// FileSource reloads the key pair when the cert file's mtime changes.
type FileSource struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	modTime time.Time
	checked time.Time
}

func NewFileSource(certFile, keyFile string) (*FileSource, error) {
	s := &FileSource{certFile: certFile, keyFile: keyFile}
	if _, err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *FileSource) load() (*tls.Certificate, error) {
	st, err := os.Stat(s.certFile)
	if err != nil {
		return nil, err
	}
	if s.cert != nil && st.ModTime().Equal(s.modTime) {
		return s.cert, nil
	}
	c, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return nil, err
	}
	s.cert, s.modTime = &c, st.ModTime()
	return s.cert, nil
}

// GetCertificate is for tls.Config. It re-stats the file at most once a minute
// and keeps serving the old cert if a reload fails.
func (s *FileSource) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.checked) < time.Minute {
		return s.cert, nil
	}
	s.checked = time.Now()
	if c, err := s.load(); err == nil {
		return c, nil
	}
	return s.cert, nil
}

// SelfSigned returns a throwaway ECDSA cert for host. Dev only.
func SelfSigned(host string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
