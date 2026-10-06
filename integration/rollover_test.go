package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dio/egtest"
	"github.com/dio/kona/internal/tlsreload"
)

//go:embed testdata/next-ca.yaml
var nextCA []byte

type rolloverTest struct {
	t                            *testing.T
	cluster                      *egtest.Cluster
	issuerMode                   string
	oldCA                        []byte
	proxyURL, adminURL, envoyURL string
	apply                        func([]byte)
	run                          func([]byte, ...string) []byte
	secret                       func(string) map[string][]byte
	prepareNext                  func()
	finish                       func([]byte)
}

func (r rolloverTest) execute() {
	t := r.t
	oldClient := r.secret("kona-client")
	oldServer := r.secret("kona-server")
	var rootB []byte
	var probeB map[string][]byte
	var switchClient, switchServer, restoreOldServer func()
	persistIssuer := func() {}
	if r.prepareNext != nil {
		r.prepareNext()
	}
	if r.issuerMode == "cert-manager" {
		r.apply(nextCA)
		r.run(nil, "wait", "--for=condition=Ready", "certificate/kona-ca-next", "certificate/kona-client-next", "--timeout=180s")
		rootB = r.secret("kona-ca-next")["tls.crt"]
		probeB = r.secret("kona-client-next")
		changeIssuer := func(name, issuer string) {
			patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"issuerRef": map[string]string{"name": issuer}}})
			r.run(nil, "patch", "certificate", name, "--type=merge", "-p", string(patch))
		}
		switchClient = func() { changeIssuer("kona-client", "kona-ca-next") }
		switchServer = func() { changeIssuer("kona-server", "kona-ca-next") }
		restoreOldServer = func() { changeIssuer("kona-server", "kona-ca") }
	} else {
		next := newFixtureCA(t)
		rootB = next.pem
		probeB = next.leaf(t, "", "spiffe://kona.test/gateway")
		persistIssuer = func() {
			key, err := x509.MarshalPKCS8PrivateKey(next.key)
			if err != nil {
				t.Fatal(err)
			}
			r.apply(secretManifest(t, "kona-ca", "kubernetes.io/tls", map[string][]byte{"tls.crt": next.pem, "tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})}))
		}
		switchClient = func() {
			r.apply(secretManifest(t, "kona-client", "kubernetes.io/tls", next.leaf(t, "", "spiffe://kona.test/gateway")))
		}
		switchServer = func() {
			r.apply(secretManifest(t, "kona-server", "kubernetes.io/tls", next.leaf(t, "kona-data.default.svc.cluster.local", "")))
		}
		restoreOldServer = func() { r.apply(secretManifest(t, "kona-server", "kubernetes.io/tls", oldServer)) }
	}
	overlap := append(bytes.Clone(r.oldCA), rootB...)
	// Gate 1: both trust consumers have observed A+B before either active leaf moves.
	// Keep a continuous application probe running through all intended rollover phases.
	stopMonitor := r.monitor()
	defer stopMonitor()
	beforeProxy := r.pods("envoy-gateway-system", "gateway.envoyproxy.io/owning-gateway-name=kona")
	beforeServer := r.pods("default", "app=kona-data")
	r.publishTrust(overlap)
	r.awaitTrust(overlap)
	r.expectClient(oldClient, overlap, 200)
	r.expectClient(probeB, overlap, 200)
	t.Log("CA overlap observed in active Envoy trust and projected service trust; A and B clients accepted")

	// Gate 2: move the client first; this exercises B client -> A server.
	switchClient()
	var activeClient map[string][]byte
	eventually(t, 90*time.Second, func() bool { activeClient = r.secret("kona-client"); return signedBy(activeClient, rootB) })
	r.awaitReceipt(activeClient)
	r.expectClient(activeClient, overlap, 200)
	// Then move the server. Wait for the projected leaf AND fresh remote delivery.
	switchServer()
	var activeServer map[string][]byte
	eventually(t, 90*time.Second, func() bool { activeServer = r.secret("kona-server"); return signedBy(activeServer, rootB) })
	r.awaitServer(activeServer)
	r.expectClient(activeClient, rootB, 200)
	r.freshData("ca-b-overlap")
	t.Log("both peers switched to B-signed leaves; fresh data delivered with overlapping trust")

	// Preserve B as the certgen repair issuer before retiring A.
	persistIssuer()
	// Gate 3: retire A only after both B leaves are observed.
	r.publishTrust(rootB)
	r.awaitTrust(rootB)
	r.expectClient(activeClient, rootB, 200)
	r.freshData("ca-b-only")
	stopMonitor()
	if r.pods("envoy-gateway-system", "gateway.envoyproxy.io/owning-gateway-name=kona") != beforeProxy || r.pods("default", "app=kona-data") != beforeServer {
		t.Fatal("pod identity/restart count changed during CA rollover")
	}
	if _, err := r.probe(oldClient, rootB); err == nil || !strings.Contains(err.Error(), "remote error: tls:") {
		t.Fatalf("retired-CA client was not rejected with TLS alert: %v", err)
	}
	t.Log("A retired from both trust bundles; old client rejected; no sampled request failures or pod restarts during rollover")

	// Deliberately reintroduce an A server after retirement: Envoy must fail closed.
	// This negative phase is excluded from availability measurement.
	restoreOldServer()
	var retiredServer map[string][]byte
	eventually(t, 90*time.Second, func() bool { retiredServer = r.secret("kona-server"); return signedBy(retiredServer, r.oldCA) })
	r.awaitServer(retiredServer)
	eventually(t, 30*time.Second, func() bool { code, _ := r.get(r.proxyURL); return code == 503 })
	switchServer()
	eventually(t, 90*time.Second, func() bool { activeServer = r.secret("kona-server"); return signedBy(activeServer, rootB) })
	r.awaitServer(activeServer)
	r.freshData("ca-rollover-recovered")
	t.Log("Envoy rejected a retired-CA server and recovered after restoring a B-signed server")
	if r.finish != nil {
		r.finish(rootB)
		r.awaitTrust(rootB)
		r.freshData("post-upgrade-b-only")
		if r.pods("envoy-gateway-system", "gateway.envoyproxy.io/owning-gateway-name=kona") != beforeProxy || r.pods("default", "app=kona-data") != beforeServer {
			t.Fatal("post-rollover Helm upgrade restarted a peer")
		}
		t.Log("post-rollover Helm upgrade preserved B-only trust, B credentials and data delivery")
	}
}
func (r rolloverTest) get(url string) (int, []byte) {
	c := &http.Client{Timeout: 3 * time.Second}
	res, err := c.Get(url)
	if err != nil {
		return 0, nil
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	return res.StatusCode, b
}
func (r rolloverTest) publishTrust(bundle []byte) {
	b, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]string{"name": "kona-ca", "namespace": "default"}, "data": map[string]string{"ca.crt": string(bundle)}})
	r.apply(b)
}
func (r rolloverTest) awaitTrust(bundle []byte) {
	sum := sha256.Sum256(bundle)
	expected := hex.EncodeToString(sum[:])
	eventually(r.t, 150*time.Second, func() bool {
		code, b := r.get(r.adminURL + "/tls-state")
		var state tlsreload.State
		if code != 200 || json.Unmarshal(b, &state) != nil || state.TrustSHA256 != expected {
			return false
		}
		code, b = r.get(r.envoyURL + "/config_dump")
		return code == 200 && containsTrustBundle(b, bundle)
	})
}

// Match the exact public CA bundle in Envoy's active validation-context material.
// Never print config_dump, which can also contain sensitive configuration.
func containsTrustBundle(dump, bundle []byte) bool {
	var doc any
	if json.Unmarshal(dump, &doc) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			if ca, ok := x["trusted_ca"].(map[string]any); ok {
				if s, ok := ca["inline_string"].(string); ok && bytes.Equal(bytes.TrimSpace([]byte(s)), bytes.TrimSpace(bundle)) {
					return true
				}
				if s, ok := ca["inline_bytes"].(string); ok {
					b, e := base64.StdEncoding.DecodeString(s)
					if e == nil && bytes.Equal(bytes.TrimSpace(b), bytes.TrimSpace(bundle)) {
						return true
					}
				}
			}
			for name, child := range x {
				if strings.Contains(name, "warming") || strings.Contains(name, "draining") || name == "error_state" {
					continue
				}
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range x {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(doc)
}
func signedBy(data map[string][]byte, ca []byte) bool {
	block, _ := pem.Decode(data["tls.crt"])
	root, _ := pem.Decode(ca)
	if block == nil || root == nil {
		return false
	}
	leaf, e := x509.ParseCertificate(block.Bytes)
	if e != nil {
		return false
	}
	issuer, e := x509.ParseCertificate(root.Bytes)
	return e == nil && leaf.CheckSignatureFrom(issuer) == nil
}
func leafSerial(data map[string][]byte) string {
	b, _ := pem.Decode(data["tls.crt"])
	if b == nil {
		return ""
	}
	c, e := x509.ParseCertificate(b.Bytes)
	if e != nil {
		return ""
	}
	return c.SerialNumber.String()
}
func (r rolloverTest) awaitServer(data map[string][]byte) {
	want := leafSerial(data)
	eventually(r.t, 150*time.Second, func() bool {
		_, b := r.get(r.adminURL + "/tls-state")
		var state tlsreload.State
		return json.Unmarshal(b, &state) == nil && state.ServerSerial == want
	})
}
func (r rolloverTest) awaitReceipt(data map[string][]byte) {
	serial := leafSerial(data)
	eventually(r.t, 90*time.Second, func() bool {
		_, b := r.get(r.adminURL + "/receipts")
		var receipts map[string]int
		return json.Unmarshal(b, &receipts) == nil && receipts[serial] > 0
	})
}
func (r rolloverTest) probe(data map[string][]byte, bundle []byte) (int, error) {
	f, err := r.cluster.PortForward(r.t.Context(), "default", "deployment/kona-data", 8443)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	cert, err := tls.X509KeyPair(data["tls.crt"], data["tls.key"])
	if err != nil {
		return 0, err
	}
	roots, err := tlsreload.ParseRoots(bundle)
	if err != nil {
		return 0, err
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "kona-data.default.svc.cluster.local", GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cert, nil }}}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	res, err := c.Get(strings.Replace(f.URL, "http://", "https://", 1) + "/data")
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	_, err = io.Copy(io.Discard, res.Body)
	return res.StatusCode, err
}
func (r rolloverTest) expectClient(data map[string][]byte, bundle []byte, status int) {
	r.t.Helper()
	code, e := r.probe(data, bundle)
	if e != nil || code != status {
		r.t.Fatalf("client probe status=%d err=%v", code, e)
	}
}
func (r rolloverTest) freshData(message string) {
	r.t.Helper()
	payload, _ := json.Marshal(map[string]any{"demo": map[string]string{"message": message}})
	req, _ := http.NewRequest("PUT", r.adminURL+"/data", bytes.NewReader(payload))
	c := &http.Client{Timeout: 3 * time.Second}
	res, err := c.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 204 {
		r.t.Fatal("data update failed")
	}
	eventually(r.t, 15*time.Second, func() bool { code, b := r.get(r.proxyURL); return code == 200 && bytes.Contains(b, []byte(message)) })
}
func (r rolloverTest) pods(ns, selector string) string {
	// Identity plus restart counts: pod UID alone cannot detect an in-place container restart.
	return string(r.run(nil, "get", "pods", "-n", ns, "-l", selector, "-o", `jsonpath={range .items[*]}{.metadata.uid}:{range .status.containerStatuses[*]}{.restartCount},{end}{end}`))
}
func (r rolloverTest) monitor() func() {
	ctx, cancel := context.WithCancel(r.t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	var failures, requests int
	go func() {
		defer wg.Done()
		for {
			code, _ := r.get(r.proxyURL)
			requests++
			if code != 200 {
				failures++
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			wg.Wait()
			r.t.Logf("CA rollover availability probes: requests=%d failures=%d", requests, failures)
			if failures > 0 {
				r.t.Error(fmt.Sprintf("CA rollover had %d failed application probes", failures))
			}
		})
	}
}
