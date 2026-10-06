package integration

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/dio/kona/internal/tlsreload"
)

func TestTLSReloadOverlapRetirementAndProjection(t *testing.T) {
	a, b := newFixtureCA(t), newFixtureCA(t)
	dir := t.TempDir()
	trust := filepath.Join(dir, "trust.pem")
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	generation := func(name string, data map[string][]byte) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
		write(filepath.Join(p, "tls.crt"), data["tls.crt"])
		write(filepath.Join(p, "tls.key"), data["tls.key"])
	}
	point := func(name string) {
		t.Helper()
		if err := os.Symlink(name, filepath.Join(dir, "..next")); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(dir, "..next"), filepath.Join(dir, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	generation("a", a.leaf(t, "kona-data.default.svc.cluster.local", ""))
	point("a")
	write(trust, a.pem)
	files := tlsreload.Files{IdentityDir: dir, TrustFile: trust}
	first, stateA, err := files.Load()
	if err != nil {
		t.Fatal(err)
	}
	write(trust, append(bytes.Clone(a.pem), b.pem...))
	overlap, _, err := files.Load()
	if err != nil {
		t.Fatal(err)
	}
	clientA, clientB := a.leaf(t, "", "spiffe://kona.test/gateway"), b.leaf(t, "", "spiffe://kona.test/gateway")
	verify := func(data map[string][]byte, pool *x509.CertPool) error {
		block, _ := pem.Decode(data["tls.crt"])
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return err
		}
		_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		return err
	}
	if verify(clientA, overlap.ClientCAs) != nil || verify(clientB, overlap.ClientCAs) != nil {
		t.Fatal("overlap did not trust both roots")
	}
	generation("b", b.leaf(t, "kona-data.default.svc.cluster.local", ""))
	point("b")
	write(trust, b.pem)
	retired, stateB, err := files.Load()
	if err != nil {
		t.Fatal(err)
	}
	if stateA.ServerSerial == stateB.ServerSerial {
		t.Fatal("projected identity not reloaded")
	}
	if verify(clientA, retired.ClientCAs) == nil || verify(clientB, retired.ClientCAs) != nil {
		t.Fatal("root retirement failed")
	}
	if verify(clientB, first.ClientCAs) == nil {
		t.Fatal("published config was mutated")
	}
	if !retired.SessionTicketsDisabled {
		t.Fatal("session resumption can bypass renewed trust checks")
	}
	write(trust, []byte("malformed"))
	if _, _, err := files.Load(); err == nil {
		t.Fatal("invalid trust fell back to retired roots")
	}
	if _, err := files.Config().GetConfigForClient(nil); err == nil {
		t.Fatal("handshake accepted invalid trust")
	}
}

func TestTrustBundleRejectsPartialPEM(t *testing.T) {
	ca := newFixtureCA(t)
	for _, bundle := range [][]byte{nil, append(bytes.Clone(ca.pem), []byte("garbage")...), ca.leaf(t, "example.test", "")["tls.crt"]} {
		if _, err := tlsreload.ParseRoots(bundle); err == nil {
			t.Fatal("accepted empty, partial or non-CA trust")
		}
	}
}
