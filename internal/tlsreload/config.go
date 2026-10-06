// Package tlsreload reads independently projected identity and trust material per handshake.
package tlsreload

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
)

type Files struct{ IdentityDir, TrustFile string }
type State struct {
	TrustSHA256  string `json:"trust_sha256"`
	ServerSerial string `json:"server_serial"`
}

// Load returns a new immutable TLS configuration. Missing or malformed material
// fails closed; an invalid trust update never silently extends retired trust.
func (f Files) Load() (*tls.Config, State, error) {
	var state State
	dir := f.IdentityDir
	// Kubernetes projects cert and key through one ..data generation. Resolve it
	// once so a symlink swap cannot pair a new certificate with an old key.
	generation, err := filepath.EvalSymlinks(filepath.Join(dir, "..data"))
	if err == nil {
		dir = generation
	} else if !os.IsNotExist(err) {
		return nil, state, err
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		return nil, state, err
	}
	bundle, err := os.ReadFile(f.TrustFile)
	if err != nil {
		return nil, state, err
	}
	roots, err := ParseRoots(bundle)
	if err != nil {
		return nil, state, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, state, err
	}
	sum := sha256.Sum256(bundle)
	state = State{TrustSHA256: hex.EncodeToString(sum[:]), ServerSerial: leaf.SerialNumber.String()}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, SessionTicketsDisabled: true}, state, nil
}
func (f Files) Config() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, SessionTicketsDisabled: true, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { c, _, err := f.Load(); return c, err }}
}
func ParseRoots(bundle []byte) (*x509.CertPool, error) {
	roots := x509.NewCertPool()
	count := 0
	for len(bytes.TrimSpace(bundle)) > 0 {
		bundle = bytes.TrimSpace(bundle)
		if !bytes.HasPrefix(bundle, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, errors.New("invalid trust PEM")
		}
		block, rest := pem.Decode(bundle)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, errors.New("invalid trust PEM")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		if !cert.IsCA {
			return nil, errors.New("trust bundle contains non-CA certificate")
		}
		roots.AddCert(cert)
		count++
		bundle = rest
	}
	if count == 0 {
		return nil, errors.New("empty trust bundle")
	}
	return roots, nil
}
