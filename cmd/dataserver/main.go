// dataserver is an in-memory, mTLS-only fixture. Admin is reached only by test port-forward.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/dio/kona/internal/tlsreload"
)

func main() {
	dir := flag.String("tls-dir", "/tls", "mounted TLS secret")
	trustFile := flag.String("trust-file", "/trust/ca.crt", "projected client trust bundle")
	flag.Parse()
	files := tlsreload.Files{IdentityDir: *dir, TrustFile: *trustFile}
	if _, _, err := files.Load(); err != nil {
		log.Fatal(err)
	}
	var mu sync.Mutex
	document := json.RawMessage(`{"demo":{"message":"one"}}`)
	receipts := map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("/data", func(w http.ResponseWriter, r *http.Request) {
		// Trust chain alone is insufficient: authorize this application's client identity.
		peer := r.TLS.PeerCertificates[0]
		authorized := false
		for _, uri := range peer.URIs {
			if uri.String() == "spiffe://kona.test/gateway" {
				authorized = true
			}
		}
		if !authorized {
			http.Error(w, "forbidden", 403)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		receipts[peer.SerialNumber.String()]++
		w.Header().Set("Content-Type", "application/json")
		// Force fresh handshakes so the spike can observe certificate rotation promptly.
		w.Header().Set("Connection", "close")
		_, _ = w.Write(document)
	})
	admin := http.NewServeMux()
	admin.HandleFunc("/data", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			http.Error(w, "method", 405)
			return
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
		if err != nil || len(b) > 1<<20 || !json.Valid(b) {
			http.Error(w, "invalid document", 400)
			return
		}
		mu.Lock()
		document = append(document[:0], b...)
		mu.Unlock()
		w.WriteHeader(204)
	})
	admin.HandleFunc("/receipts", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(receipts)
	})
	admin.HandleFunc("/tls-state", func(w http.ResponseWriter, r *http.Request) {
		_, state, err := files.Load()
		if err != nil {
			http.Error(w, "TLS material unavailable", 503)
			return
		}
		_ = json.NewEncoder(w).Encode(state)
	})
	admin.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	go func() {
		log.Fatal((&http.Server{Addr: "127.0.0.1:8080", Handler: admin, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe())
	}()
	srv := &http.Server{Addr: ":8443", Handler: mux, ReadHeaderTimeout: 5 * time.Second, TLSConfig: files.Config()}
	log.Fatal(srv.ListenAndServeTLS("", ""))
}
