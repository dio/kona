// Package envoytest runs one local Envoy process for Go integration tests.
// Inspired by dio/transit's examples/internal/e2etest; builds remain caller-owned.
package envoytest

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"
)

type Options struct {
	Binary string
	Module string
	// Bootstrap is a text/template accepting Ports and Module below.
	Bootstrap string
	// Env is explicit: callers of affected SDKs must opt into GODEBUG=cgocheck=0.
	Env []string
}
type Config struct {
	ProxyPort, AdminPort int
	Module               string
}
type Process struct{ URL, AdminURL string }

// Start fails the test on missing prerequisites or startup failure. It never skips.
// Cleanup terminates/reaps Envoy before deleting its temporary config and log files.
func Start(t testing.TB, options Options) *Process {
	t.Helper()
	if options.Binary == "" {
		options.Binary = os.Getenv("ENVOY_BIN")
	}
	if options.Binary == "" {
		t.Fatal("envoytest: Binary or ENVOY_BIN required")
	}
	binary, err := exec.LookPath(options.Binary)
	if err != nil {
		t.Fatal(err)
	}
	module, err := filepath.Abs(options.Module)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(module)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatal("module must be a regular file")
	}
	// Reserve both ports together, then release immediately before process startup.
	// A close-and-bind race remains; startup failure is reported with Envoy logs.
	proxy := reserve(t)
	admin := reserve(t)
	defer proxy.Close()
	defer admin.Close()
	cfg := Config{ProxyPort: proxy.Addr().(*net.TCPAddr).Port, AdminPort: admin.Addr().(*net.TCPAddr).Port, Module: module}
	parsed, err := template.New("envoy").Option("missingkey=error").Parse(options.Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	var content bytes.Buffer
	if err := parsed.Execute(&content, cfg); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "envoy.yaml")
	if err := os.WriteFile(path, content.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	logfile, err := os.Create(filepath.Join(dir, "envoy.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-c", path, "--concurrency", "2", "--log-level", "warning")
	cmd.Env = append(os.Environ(), options.Env...)
	cmd.Stdout = logfile
	cmd.Stderr = logfile
	proxy.Close()
	admin.Close()
	if err := cmd.Start(); err != nil {
		logfile.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	var stop sync.Once
	cleanup := func() {
		stop.Do(func() {
			select {
			case <-done:
			default:
				_ = cmd.Process.Signal(os.Interrupt)
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					_ = cmd.Process.Kill()
					<-done
				}
			}
			_ = logfile.Close()
			if t.Failed() {
				data, _ := os.ReadFile(logfile.Name())
				t.Logf("Envoy log:\n%s", data)
			}
		})
	}
	t.Cleanup(cleanup)
	process := &Process{URL: fmt.Sprintf("http://127.0.0.1:%d", cfg.ProxyPort), AdminURL: fmt.Sprintf("http://127.0.0.1:%d", cfg.AdminPort)}
	client := &http.Client{Timeout: 500 * time.Millisecond}
	defer client.CloseIdleConnections()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			t.Fatalf("Envoy exited before readiness: %v", waitErr)
		case <-timer.C:
			t.Fatal("Envoy readiness timed out")
		case <-ticker.C:
			response, err := client.Get(process.AdminURL + "/ready")
			if err != nil {
				continue
			}
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return process
			}
		}
	}
}
func reserve(t testing.TB) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

// HTTPBootstrap returns a single-listener bootstrap with a caller-supplied HTTP
// filter (JSON/YAML fragment), route config and optional clusters fragment.
// Fragments must be trusted test configuration. The template is rendered by Start.
func HTTPBootstrap(filter, routes, clusters string) string {
	return strings.NewReplacer("FILTER", filter, "ROUTES", routes, "CLUSTERS", clusters).Replace(`
admin:
  address: {socket_address: {address: 127.0.0.1, port_value: {{.AdminPort}}}}
static_resources:
  listeners:
  - name: test
    address: {socket_address: {address: 127.0.0.1, port_value: {{.ProxyPort}}}}
    per_connection_buffer_limit_bytes: 16384
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          '@type': type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: test
          request_timeout: 10s
          stream_idle_timeout: 10s
          route_config: ROUTES
          http_filters: FILTER
  clusters: CLUSTERS
`)
}
