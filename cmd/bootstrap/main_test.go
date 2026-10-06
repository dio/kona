package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"github.com/dio/kona/internal/pki"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBootstrapPreservesKeysAndTrustAndRejectsOtherOwner(t *testing.T) {
	objects := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/test/")
		if r.Method == "GET" {
			if b, ok := objects[path]; ok {
				_, _ = w.Write(b)
			} else {
				w.WriteHeader(404)
			}
			return
		}
		if r.Method != "POST" {
			t.Errorf("unexpected mutating method %s", r.Method)
			w.WriteHeader(405)
			return
		}
		var obj map[string]any
		if json.NewDecoder(r.Body).Decode(&obj) != nil {
			w.WriteHeader(400)
			return
		}
		metadata := obj["metadata"].(map[string]any)
		key := path + "/" + metadata["name"].(string)
		if _, ok := objects[key]; ok {
			w.WriteHeader(409)
			return
		}
		b, _ := json.Marshal(obj)
		objects[key] = b
		w.WriteHeader(201)
	}))
	defer server.Close()
	k := kube{client: server.Client(), url: server.URL, namespace: "test", prefix: "kona"}
	if err := k.run(context.Background(), "certgen"); err != nil {
		t.Fatal(err)
	}
	if len(objects) != 4 {
		t.Fatalf("expected CA, two leaves, trust; got %d", len(objects))
	}
	old := string(objects["secrets/kona-client"])
	var cm map[string]any
	_ = json.Unmarshal(objects["configmaps/kona-ca"], &cm)
	cm["data"] = map[string]string{"ca.crt": "deliberate-overlap-bundle"}
	objects["configmaps/kona-ca"], _ = json.Marshal(cm)
	if err := k.run(context.Background(), "certgen"); err != nil {
		t.Fatal(err)
	}
	if string(objects["secrets/kona-client"]) != old {
		t.Fatal("upgrade replaced existing client")
	}
	if !strings.Contains(string(objects["configmaps/kona-ca"]), "deliberate-overlap-bundle") {
		t.Fatal("upgrade reset trust bundle")
	}
	var leaf map[string]any
	_ = json.Unmarshal(objects["secrets/kona-client"], &leaf)
	leaf["metadata"].(map[string]any)["annotations"] = map[string]string{"meta.helm.sh/release-name": "other"}
	objects["secrets/kona-client"], _ = json.Marshal(leaf)
	if err := k.run(context.Background(), "certgen"); err == nil {
		t.Fatal("reused foreign credentials")
	}
	// After rollover, persist B as the repair issuer. A missing leaf must use B.
	b, err := pki.New("replacement")
	if err != nil {
		t.Fatal(err)
	}
	data, err := b.Secret()
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	_ = json.Unmarshal(objects["secrets/kona-ca"], &root)
	root["data"] = data
	objects["secrets/kona-ca"], _ = json.Marshal(root)
	delete(objects, "secrets/kona-client")
	if err := k.run(context.Background(), "certgen"); err != nil {
		t.Fatal(err)
	}
	var repaired object
	if err := json.Unmarshal(objects["secrets/kona-client"], &repaired); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(repaired.Data["tls.crt"])
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.CheckSignatureFrom(b.Certificate); err != nil {
		t.Fatal("repair used retired CA")
	}

}
