package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CA mints leaf certificates for intercepted hostnames. The CA private key is
// the skeleton key for every device that trusts it: keep it 0600 and back it up
// offline, never in the repo.
type CA struct {
	cert     *x509.Certificate
	key      *ecdsa.PrivateKey
	leafDays int

	mu    sync.Mutex
	cache map[string]cachedLeaf
}

type cachedLeaf struct {
	cert    *tls.Certificate
	renewAt time.Time
}

// maxLeafCache bounds memory: SNI is client-controlled, so the cache must not
// grow without limit.
const maxLeafCache = 4096

func loadCA(certPath, keyPath string, leafDays int) (*CA, error) {
	cb, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}
	kb, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read CA key: %w", err)
	}
	cp, _ := pem.Decode(cb)
	if cp == nil || cp.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s is not a PEM certificate", certPath)
	}
	cert, err := x509.ParseCertificate(cp.Bytes)
	if err != nil {
		return nil, err
	}
	kp, _ := pem.Decode(kb)
	if kp == nil {
		return nil, fmt.Errorf("%s is not a PEM key", keyPath)
	}
	var key *ecdsa.PrivateKey
	switch kp.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(kp.Bytes)
	case "PRIVATE KEY":
		var k any
		k, err = x509.ParsePKCS8PrivateKey(kp.Bytes)
		if err == nil {
			var ok bool
			key, ok = k.(*ecdsa.PrivateKey)
			if !ok {
				err = fmt.Errorf("CA key must be ECDSA")
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("CA certificate is not a CA")
	}
	return &CA{cert: cert, key: key, leafDays: leafDays, cache: map[string]cachedLeaf{}}, nil
}

func (c *CA) leaf(host string) (*tls.Certificate, error) {
	now := time.Now()
	c.mu.Lock()
	if lc, ok := c.cache[host]; ok && now.Before(lc.renewAt) {
		c.mu.Unlock()
		return lc.cert, nil
	}
	c.mu.Unlock()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	notAfter := now.AddDate(0, 0, c.leafDays)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             now.Add(-2 * time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	lc := &tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key}
	c.mu.Lock()
	if len(c.cache) >= maxLeafCache {
		c.cache = map[string]cachedLeaf{}
	}
	// Re-mint at half-life so a long-running proxy never serves a cached leaf
	// past its NotAfter (leaf_days is 7; the router routinely stays up longer).
	c.cache[host] = cachedLeaf{cert: lc, renewAt: now.Add(notAfter.Sub(now) / 2)}
	c.mu.Unlock()
	return lc, nil
}

// initCA writes a fresh ECDSA P-256 CA. Refuses to overwrite an existing key.
func initCA(dir string) error {
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	if _, err := os.Stat(keyPath); err == nil {
		return fmt.Errorf("%s already exists — refusing to overwrite", keyPath)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Kids Filter CA",
			Organization: []string{"Home Router"},
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	return os.Chmod(keyPath, 0o600)
}

func (c *CA) pem() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})
}

func readCACert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, _ := pem.Decode(b)
	if p == nil || p.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s is not a PEM certificate", path)
	}
	return x509.ParseCertificate(p.Bytes)
}

func (c *CA) fingerprint() string {
	return fmt.Sprintf("%X", c.cert.SerialNumber)
}
