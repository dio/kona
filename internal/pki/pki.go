// Package pki provides the disposable certgen issuer used by the Helm bootstrap job.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"time"
)

type Authority struct {
	Certificate *x509.Certificate
	Key         *ecdsa.PrivateKey
	PEM         []byte
}

func New(name string) (*Authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := serial()
	if err != nil {
		return nil, err
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &Authority{cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}
func Load(data map[string][]byte) (*Authority, error) {
	certBlock, _ := pem.Decode(data["tls.crt"])
	keyBlock, _ := pem.Decode(data["tls.key"])
	if certBlock == nil || keyBlock == nil {
		return nil, errors.New("missing CA material")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok || !cert.IsCA {
		return nil, errors.New("expected ECDSA CA")
	}
	public, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !public.Equal(&ec.PublicKey) {
		return nil, errors.New("CA key mismatch")
	}
	return &Authority{cert, ec, data["tls.crt"]}, nil
}
func (a *Authority) Secret() (map[string][]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(a.Key)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{"tls.crt": a.PEM, "tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}, nil
}
func (a *Authority) Issue(dns, identity string) (map[string][]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := serial()
	if err != nil {
		return nil, err
	}
	cert := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	if dns != "" {
		cert.DNSNames = []string{dns}
		cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	if identity != "" {
		u, err := url.Parse(identity)
		if err != nil {
			return nil, err
		}
		cert.URIs = []*url.URL{u}
		cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, a.Certificate, &key.PublicKey, a.Key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), "ca.crt": a.PEM}, nil
}
func serial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	return n.Add(n, big.NewInt(1)), nil
}
