package native

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dio/kona/envoytest"
)

func TestModule(t *testing.T) {
	if os.Getenv("ENVOY_BIN") == "" {
		t.Skip("set ENVOY_BIN and KONA_MODULE for native Envoy tests")
	}
	for _, source := range []string{"inline", "file", "blocked-file"} {
		t.Run(source, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "data.json")
			publish := func(value string) {
				t.Helper()
				if err := os.WriteFile(filename+".next", []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filename+".next", filename); err != nil {
					t.Fatal(err)
				}
			}
			var pipe *os.File
			if source == "blocked-file" {
				if err := exec.Command("mkfifo", filename).Run(); err != nil {
					t.Fatal(err)
				}
				var err error
				pipe, err = os.OpenFile(filename, os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { pipe.Close() })
			} else {
				publish(`{"demo":{"message":"first"}}`)
			}
			input := map[string]any{"inline": map[string]any{"demo": map[string]string{"message": "first"}}}
			if source != "inline" {
				input = map[string]any{"filename": filename}
			}
			config, _ := json.Marshal(map[string]any{"source": input, "poll_ms": 50, "max_age_ms": 500})
			filters, _ := json.Marshal([]any{
				map[string]any{"name": "kona", "typed_config": map[string]any{
					"@type":                 "type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter",
					"dynamic_module_config": map[string]any{"name": "kona", "module": map[string]any{"local": map[string]string{"filename": "{{.Module}}"}}, "do_not_close": true},
					"filter_name":           "kona", "filter_config": map[string]string{"@type": "type.googleapis.com/google.protobuf.StringValue", "value": string(config)},
				}},
				map[string]any{"name": "envoy.filters.http.router", "typed_config": map[string]string{"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}},
			})
			routes := `{"name":"test","virtual_hosts":[{"name":"all","domains":["*"],"routes":[{"match":{"prefix":"/"},"direct_response":{"status":500},"metadata":{"filter_metadata":{"kona":{"key":"demo"}}}}]}]}`
			process := envoytest.Start(t, envoytest.Options{Module: os.Getenv("KONA_MODULE"),
				Bootstrap: envoytest.HTTPBootstrap(string(filters), routes, "[]"), Env: []string{"GODEBUG=cgocheck=0"}})
			client := &http.Client{Timeout: time.Second}
			defer client.CloseIdleConnections()
			await := func(status int, body string) {
				t.Helper()
				deadline := time.NewTimer(5 * time.Second)
				defer deadline.Stop()
				tick := time.NewTicker(25 * time.Millisecond)
				defer tick.Stop()
				last := ""
				for {
					select {
					case <-deadline.C:
						t.Fatalf("response did not converge: %s", last)
					case <-tick.C:
						response, err := client.Get(process.URL)
						if err != nil {
							last = err.Error()
							continue
						}
						data, err := io.ReadAll(response.Body)
						response.Body.Close()
						last = fmt.Sprintf("%d %s (%v)", response.StatusCode, data, err)
						if err == nil && response.StatusCode == status && (body == "" || string(data) == body) {
							return
						}
					}
				}
			}
			if source == "blocked-file" {
				// The FIFO has a writer but no data or EOF. Envoy's admin
				// readiness (checked by Start) must work while the read blocks.
				await(503, "")
				publish(`{"demo":{"message":"first"}}`)
				if err := pipe.Close(); err != nil {
					t.Fatal(err)
				}
			}
			await(200, `{"message":"first"}`)
			if source == "file" {
				publish(`{"demo":{"message":"second"}}`)
				await(200, `{"message":"second"}`)
				publish(`invalid`)
				await(503, "")
				publish(`{"demo":{"message":"recovered"}}`)
				await(200, `{"message":"recovered"}`)
			}
		})
	}
}
