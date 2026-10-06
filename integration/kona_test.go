package integration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"github.com/dio/kona/internal/pki"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dio/egtest"
)

//go:embed testdata/helm-values.yaml
var helmValues []byte

func TestKona(t *testing.T) {
	if os.Getenv("KONA_INTEGRATION") != "1" {
		t.Skip("set KONA_INTEGRATION=1; requires built kona images and Docker")
	}
	ctx := t.Context()
	c, err := egtest.Open(ctx, egtest.Options{Prefix: "kona", EGVersion: "v1.9.1", K3SVersion: "v1.33.13-k3s2", HelmValues: helmValues})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
		b, _ := json.Marshal(c.Info())
		t.Logf("cluster cleanup evidence: %s", b)
	})
	run := func(input []byte, args ...string) []byte {
		t.Helper()
		b, e := c.Kubectl(ctx, input, args...)
		if e != nil {
			t.Fatalf("kubectl %s failed: %v", args[0], e)
		}
		return b
	}
	apply := func(b []byte) {
		t.Helper()
		if e := c.Apply(ctx, b); e != nil {
			t.Fatal(e)
		}
	}
	t.Log("EG ready; importing spike images")
	if err := c.ImportImages(ctx, "kona-proxy:spike", "kona-data:spike"); err != nil {
		t.Fatal(err)
	}

	issuerMode := os.Getenv("KONA_ISSUER")
	if issuerMode == "" {
		issuerMode = "cert-manager"
	}
	var rotateClient func()
	switch issuerMode {
	case "cert-manager":
		client := &http.Client{Timeout: 60 * time.Second}
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://github.com/cert-manager/cert-manager/releases/download/v1.21.2/cert-manager.yaml", nil)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := io.ReadAll(io.LimitReader(res.Body, 10<<20))
		res.Body.Close()
		if err != nil || res.StatusCode != 200 {
			t.Fatal("cert-manager download failed")
		}
		sum := sha256.Sum256(manifest)
		if hex.EncodeToString(sum[:]) != "e03b668ec8675214af6b0a671699d088f2601fa3878e0dbe1b41d3feafd1879f" {
			t.Fatal("cert-manager manifest checksum mismatch")
		}
		run(manifest, "apply", "--server-side", "-f", "-")
		for _, name := range []string{"cert-manager", "cert-manager-webhook", "cert-manager-cainjector"} {
			if err := c.WaitDeployment(ctx, "cert-manager", name); err != nil {
				t.Fatal(err)
			}
		}
	case "certgen":

	default:
		t.Fatal("KONA_ISSUER must be cert-manager or certgen")
	}
	t.Logf("certificate issuer pathway: %s", issuerMode)

	secret := func(name string) map[string][]byte {
		t.Helper()
		b := run(nil, "get", "secret", name, "-n", "default", "-o", "json")
		var s struct{ Data map[string]string }
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		data := map[string][]byte{}
		for k, v := range s.Data {
			data[k], err = base64.StdEncoding.DecodeString(v)
			if err != nil {
				t.Fatal(err)
			}
		}
		return data
	}
	chart, err := filepath.Abs("../charts/kona")
	if err != nil {
		t.Fatal(err)
	}
	helm := func(extra ...string) {
		info := c.Info()
		args := []string{"upgrade", "--install", "kona", chart, "--kubeconfig", info.Kubeconfig, "--kube-context", "k3d-" + info.Name, "--namespace", "default", "--set", "pki.mode=" + issuerMode, "--set", "testIdentities=true", "--set", "images.data.pullPolicy=Never", "--set", "images.proxy.pullPolicy=Never", "--wait", "--wait-for-jobs", "--timeout", "5m", "--reuse-values"}
		args = append(args, extra...)
		cmd := exec.CommandContext(ctx, "helm", args...)
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("Helm install: %v: %s", e, out)
		}
	}
	helm()
	// An ordinary chart upgrade must preserve existing certgen keys and initial trust.
	before := secret("kona-client")["tls.crt"]
	helm()
	if !bytes.Equal(before, secret("kona-client")["tls.crt"]) {
		t.Fatal("Helm upgrade unexpectedly reissued client")
	}
	if issuerMode == "cert-manager" {
		rotateClient = func() { run(nil, "delete", "secret", "kona-client") }
	} else {
		authority, e := pki.Load(secret("kona-ca"))
		if e != nil {
			t.Fatal(e)
		}
		rotateClient = func() {
			data, e := authority.Issue("", "spiffe://kona.test/gateway")
			if e != nil {
				t.Fatal(e)
			}
			apply(secretManifest(t, "kona-client", "kubernetes.io/tls", data))
		}
	}
	ca := secret("kona-ca")["tls.crt"]
	t.Log("Helm installation and idempotent upgrade passed")

	if err := c.WaitDeployment(ctx, "default", "kona-data"); err != nil {
		t.Fatal(err)
	}
	if err := c.WaitProgrammed(ctx, "default", "gateway", "kona"); err != nil {
		t.Fatal(err)
	}
	if err := c.WaitDeployment(ctx, "envoy-gateway-system", "kona-proxy"); err != nil {
		t.Fatal(err)
	}
	if err := c.WaitProgrammed(ctx, "default", "envoypatchpolicy", "kona"); err != nil {
		t.Fatal(err)
	}
	forward := func(ns, target string, port int) *egtest.Forward {
		t.Helper()
		f, e := c.PortForward(ctx, ns, target, port)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			if e := f.Close(); e != nil {
				t.Error(e)
			}
		})
		return f
	}
	proxy := forward("envoy-gateway-system", "deployment/kona-proxy", 8080)
	admin := forward("default", "deployment/kona-data", 8080)
	envoyAdmin := forward("envoy-gateway-system", "deployment/kona-proxy", 19000)
	hc := &http.Client{Timeout: 3 * time.Second}
	get := func(url string) (int, []byte) {
		r, e := hc.Get(url)
		if e != nil {
			return 0, nil
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		return r.StatusCode, b
	}
	eventually(t, 90*time.Second, func() bool { code, b := get(proxy.URL); return code == 200 && strings.Contains(string(b), `"one"`) })
	t.Log("native module returned data fetched from the deployed mTLS service")
	_, active := get(envoyAdmin.URL + "/config_dump")
	for _, needle := range []string{"kona", "httproute/default/kona-app/rule/0", "tls_certificate_sds_secret_configs", "kona-client"} {
		if !bytes.Contains(active, []byte(needle)) {
			t.Fatalf("active configuration missing %s", needle)
		}
	}
	// Never print raw config_dump: it can contain credentials.
	routeVersion := string(run(nil, "get", "httproute", "kona-app", "-o", "jsonpath={.metadata.resourceVersion}"))
	podUIDs := string(run(nil, "get", "pods", "-n", "envoy-gateway-system", "-l", "gateway.envoyproxy.io/owning-gateway-name=kona", "-o", "jsonpath={.items[*].metadata.uid}"))
	if podUIDs == "" {
		t.Fatal("missing proxy pod identity")
	}
	update := func(payload string) {
		t.Helper()
		req, _ := http.NewRequest("PUT", admin.URL+"/data", strings.NewReader(payload))
		r, e := hc.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode != 204 {
			t.Fatal("data update failed")
		}
	}
	update(`{"demo":{"message":"two"}}`)
	eventually(t, 15*time.Second, func() bool { code, b := get(proxy.URL); return code == 200 && strings.Contains(string(b), `"two"`) })
	if now := string(run(nil, "get", "httproute", "kona-app", "-o", "jsonpath={.metadata.resourceVersion}")); now != routeVersion {
		t.Fatal("route changed during data update")
	}
	t.Log("data-only update observed with unchanged HTTPRoute resourceVersion")
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("invalid CA")
	}
	creds := secret("kona-client")
	cert, err := tls.X509KeyPair(creds["tls.crt"], creds["tls.key"])
	if err != nil {
		t.Fatal(err)
	}
	directRequest := func(certs []tls.Certificate) (int, error) {
		direct := forward("default", "deployment/kona-data", 8443)
		defer direct.Close()
		tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "kona-data.default.svc.cluster.local", MinVersion: tls.VersionTLS13, GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			if len(certs) == 0 {
				return &tls.Certificate{}, nil
			}
			return &certs[0], nil
		}}}
		defer tr.CloseIdleConnections()
		cl := &http.Client{Transport: tr, Timeout: 3 * time.Second}
		r, e := cl.Get(strings.Replace(direct.URL, "http://", "https://", 1) + "/data")
		if e != nil {
			return 0, e
		}
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		return r.StatusCode, nil
	}
	if code, e := directRequest([]tls.Certificate{cert}); e != nil || code != 200 {
		t.Fatalf("valid direct mTLS failed: status=%d err=%v", code, e)
	}
	if _, e := directRequest(nil); e == nil || !strings.Contains(e.Error(), "remote error: tls:") {
		t.Fatalf("expected TLS rejection for missing certificate, got %v", e)
	}
	if _, e := directRequest([]tls.Certificate{rogueCertificate(t)}); e == nil || !strings.Contains(e.Error(), "remote error: tls:") {
		t.Fatalf("expected TLS rejection for untrusted certificate, got %v", e)
	}
	other := secret("kona-other-client")
	otherCert, err := tls.X509KeyPair(other["tls.crt"], other["tls.key"])
	if err != nil {
		t.Fatal(err)
	}
	if code, e := directRequest([]tls.Certificate{otherCert}); e != nil || code != 403 {
		t.Fatalf("wrong identity: status=%d err=%v", code, e)
	}
	t.Log("deployed service rejected missing/untrusted certificates and a trusted but unauthorized identity")
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	oldSerial := leaf.SerialNumber.String()
	rotateClient()
	var newSerial string
	eventually(t, 90*time.Second, func() bool {
		b, e := c.Kubectl(ctx, nil, "get", "secret", "kona-client", "-n", "default", "-o", "json")
		if e != nil {
			return false
		}
		var s struct{ Data map[string][]byte }
		if json.Unmarshal(b, &s) != nil {
			return false
		}
		p, _ := pem.Decode(s.Data["tls.crt"])
		if p == nil {
			return false
		}
		v, e := x509.ParseCertificate(p.Bytes)
		if e != nil {
			return false
		}
		newSerial = v.SerialNumber.String()
		return newSerial != oldSerial
	})
	eventually(t, 90*time.Second, func() bool {
		_, b := get(admin.URL + "/receipts")
		var receipts map[string]int
		return json.Unmarshal(b, &receipts) == nil && receipts[newSerial] > 0
	})
	if now := string(run(nil, "get", "pods", "-n", "envoy-gateway-system", "-l", "gateway.envoyproxy.io/owning-gateway-name=kona", "-o", "jsonpath={.items[*].metadata.uid}")); now != podUIDs {
		t.Fatal("Envoy pod replaced during certificate rotation")
	}
	t.Log("rotated client certificate reached the service through EG/SDS without proxy replacement")
	run(nil, "patch", "backendtlspolicy", "kona-data", "--type=merge", "-p", `{"spec":{"validation":{"hostname":"wrong.kona.test"}}}`)
	eventually(t, 30*time.Second, func() bool { code, _ := get(proxy.URL); return code == 503 })
	run(nil, "patch", "backendtlspolicy", "kona-data", "--type=merge", "-p", `{"spec":{"validation":{"hostname":"kona-data.default.svc.cluster.local"}}}`)
	eventually(t, 30*time.Second, func() bool { code, b := get(proxy.URL); return code == 200 && strings.Contains(string(b), `"two"`) })
	t.Log("remote channel rejected a server-name mismatch and recovered after restoring trust configuration")

	update(`{"demo":{"message":"three"}}`)
	eventually(t, 15*time.Second, func() bool { code, b := get(proxy.URL); return code == 200 && strings.Contains(string(b), `"three"`) })
	update(`null`) // Valid JSON, invalid source document: must not keep stale data fresh.
	eventually(t, 15*time.Second, func() bool { code, _ := get(proxy.URL); return code == 503 })
	t.Log("invalid remote document expired the cache and failed closed")
	update(`{"demo":{"message":"recovered"}}`)
	eventually(t, 15*time.Second, func() bool {
		code, b := get(proxy.URL)
		return code == 200 && strings.Contains(string(b), `"recovered"`)
	})
	rolloverTest{t: t, cluster: c, issuerMode: issuerMode, oldCA: ca, proxyURL: proxy.URL, adminURL: admin.URL, envoyURL: envoyAdmin.URL, apply: apply, run: run, secret: secret,
		prepareNext: func() {
			if issuerMode == "cert-manager" {
				helm("--set", "pki.certManager.nextCA=true")
			}
		},
		finish: func(bundle []byte) {
			before := secret("kona-client")["tls.crt"]
			file := filepath.Join(t.TempDir(), "public-trust.pem")
			if err := os.WriteFile(file, bundle, 0600); err != nil {
				t.Fatal(err)
			}
			flags := []string{"--set-file", "pki.trustBundle=" + file}
			if issuerMode == "cert-manager" {
				flags = append(flags, "--set", "pki.certManager.leafIssuer=kona-ca-next")
			}
			helm(flags...)
			if !bytes.Equal(before, secret("kona-client")["tls.crt"]) {
				t.Fatal("post-rollover Helm upgrade reissued client")
			}
		}}.execute()
}
func eventually(t *testing.T, timeout time.Duration, f func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	for {
		if f() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("condition did not converge")
		case <-time.After(500 * time.Millisecond):
		}
	}
}
func rogueCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "untrusted"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
