// bootstrap creates missing fixture PKI and trust. Existing credentials are never overwritten.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/dio/kona/internal/pki"
)

type kube struct {
	client                        *http.Client
	url, token, namespace, prefix string
}
type object struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Data map[string][]byte `json:"data"`
}

func (k kube) request(ctx context.Context, method, kind, name string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, k.url+"/api/v1/namespaces/"+k.namespace+"/"+kind+name, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := k.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	return b, res.StatusCode, err
}
func (k kube) get(ctx context.Context, name string) (object, int, error) {
	b, status, err := k.request(ctx, "GET", "secrets", "/"+name, nil)
	var o object
	if err == nil && status == 200 {
		err = json.Unmarshal(b, &o)
	}
	return o, status, err
}
func (k kube) create(ctx context.Context, kind, name string, data any) error {
	plural := "secrets"
	typ := "kubernetes.io/tls"
	if kind == "ConfigMap" {
		plural = "configmaps"
		typ = ""
	}
	obj := map[string]any{"apiVersion": "v1", "kind": kind, "metadata": map[string]any{"name": name, "namespace": k.namespace, "labels": map[string]string{"app.kubernetes.io/managed-by": "Helm"}, "annotations": map[string]string{"meta.helm.sh/release-name": k.prefix, "meta.helm.sh/release-namespace": k.namespace}}, "data": data}
	if typ != "" {
		obj["type"] = typ
	}
	body, _ := json.Marshal(obj)
	_, status, err := k.request(ctx, "POST", plural, "", body)
	if err != nil {
		return err
	}
	if status == 409 {
		b, code, e := k.request(ctx, "GET", plural, "/"+name, nil)
		if e != nil {
			return e
		}
		if code != 200 {
			return fmt.Errorf("read existing %s returned %d", kind, code)
		}
		var existing struct {
			Metadata struct{ Annotations map[string]string }
		}
		if json.Unmarshal(b, &existing) != nil || existing.Metadata.Annotations["meta.helm.sh/release-name"] != k.prefix || existing.Metadata.Annotations["meta.helm.sh/release-namespace"] != k.namespace {
			return fmt.Errorf("existing %s belongs to another owner", kind)
		}
	}
	if status != 201 && status != 409 {
		return fmt.Errorf("create %s returned %d", kind, status)
	}
	return nil
}
func (k kube) run(ctx context.Context, mode string) error {
	name := k.prefix + "-ca"
	if mode == "certgen" {
		_, status, err := k.get(ctx, name)
		if err != nil {
			return err
		}
		if status == 404 {
			a, err := pki.New(name)
			if err != nil {
				return err
			}
			data, err := a.Secret()
			if err != nil {
				return err
			}
			if err := k.create(ctx, "Secret", name, data); err != nil {
				return err
			}
		} else if status != 200 {
			return fmt.Errorf("read CA returned %d", status)
		}
	}
	var root object
	for {
		o, status, err := k.get(ctx, name)
		if err != nil {
			return err
		}
		if status == 200 {
			root = o
			break
		}
		if status != 404 {
			return fmt.Errorf("read CA returned %d", status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if mode == "certgen" {
		if root.Metadata.Annotations["meta.helm.sh/release-name"] != k.prefix {
			return fmt.Errorf("CA belongs to another owner")
		}
		a, err := pki.Load(root.Data)
		if err != nil {
			return err
		}
		leaves := []struct{ name, dns, uri string }{{k.prefix + "-server", k.prefix + "-data." + k.namespace + ".svc.cluster.local", ""}, {k.prefix + "-client", "", "spiffe://kona.test/gateway"}}
		if os.Getenv("TEST_IDENTITIES") == "true" {
			leaves = append(leaves, struct{ name, dns, uri string }{k.prefix + "-other-client", "", "spiffe://kona.test/other"})
		}
		for _, leaf := range leaves {
			existing, status, err := k.get(ctx, leaf.name)
			if err != nil {
				return err
			}
			if status == 200 {
				if existing.Metadata.Annotations["meta.helm.sh/release-name"] != k.prefix || existing.Metadata.Annotations["meta.helm.sh/release-namespace"] != k.namespace {
					return fmt.Errorf("existing leaf belongs to another owner")
				}
				continue
			}
			if status != 404 {
				return fmt.Errorf("read leaf returned %d", status)
			}
			data, err := a.Issue(leaf.dns, leaf.uri)
			if err != nil {
				return err
			}
			if err := k.create(ctx, "Secret", leaf.name, data); err != nil {
				return err
			}
		}
	}
	// Do not reset an existing overlap/B-only trust bundle on an upgrade or retry.
	return k.create(ctx, "ConfigMap", name, map[string]string{"ca.crt": string(root.Data["tls.crt"])})
}
func main() {
	const dir = "/var/run/secrets/kubernetes.io/serviceaccount/"
	token, err := os.ReadFile(dir + "token")
	if err != nil {
		log.Fatal("read service-account token")
	}
	ca, err := os.ReadFile(dir + "ca.crt")
	if err != nil {
		log.Fatal("read API trust")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		log.Fatal("invalid API trust")
	}
	mode := os.Getenv("MODE")
	if mode != "certgen" && mode != "cert-manager" {
		log.Fatal("invalid issuer mode")
	}
	k := kube{client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}}, url: "https://kubernetes.default.svc", token: string(token), namespace: os.Getenv("NAMESPACE"), prefix: os.Getenv("RELEASE")}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := k.run(ctx, mode); err != nil {
		log.Fatal(err)
	}
	log.Print("PKI and trust bootstrap complete")
}
