# Single-Envoy native tests

Following `dio/transit/examples/internal/e2etest`, `github.com/dio/kona/envoytest`
starts **one Envoy child process from a Go test**, with a temporary bootstrap,
loopback ports, bounded readiness polling, captured logs and automatic cleanup.
Application fixtures can use `httptest.Server`, including TLS, in the test process.
No Docker, Compose, Kubernetes or Envoy Gateway is involved in this path.

## macOS arm64

Requirements: Go 1.27.1, a working cgo compiler (Xcode Command Line Tools), and the
native Envoy arm64 binary at commit `0a804c57cf5f`. The current `native.mod` uses the
matching `dio/envoy` SDK revision without changing the 1.38 Docker/EG dependency.
The newer SDK is a test-lane override, not a validated production upgrade.

```sh
make native-test ENVOY_BIN="$HOME/.tetrate/bin/envoy"
```

Or explicitly:

```sh
make native-build
ENVOY_BIN="$HOME/.tetrate/bin/envoy" \
KONA_MODULE="$PWD/.bin/libkona.so" \
go test -modfile=native.mod -v -count=1 -timeout=60s ./native
```

The `.so` suffix is used by convention; on macOS this is a Mach-O arm64 shared library.
A Linux `.so` cannot be loaded by macOS Envoy. `make native-test` checks the expected
Envoy commit before running; direct `go test` callers must choose matching artifacts.
The runner does not silently skip a missing binary or library. The native suite skips
only when `ENVOY_BIN` is not set, so normal repository unit tests remain portable.

If the binary is not installed, download the published macOS 15+ arm64 build:

```sh
mkdir -p .bin
curl -fL --retry 3 \
  https://github.com/dio/envoy-builder/releases/download/envoy-0a804c57-macos15/envoy-darwin-arm64 \
  -o .bin/envoy.download
printf '%s  %s\n' \
  7ca52fc807b9dc0c95303b50ed5bab65df945a2ceb4ebc0440b88377d4fcb76d \
  .bin/envoy.download | shasum -a 256 -c - && \
  chmod +x .bin/envoy.download && mv .bin/envoy.download .bin/envoy
make native-test ENVOY_BIN="$PWD/.bin/envoy"
```

This uses the release's published SHA-256 and a separate download file. Do not replace
an existing working binary without intending to do so. The installed binary was used
for the qualification below; the download recipe itself was not re-executed.

## Reuse from another module

```go
process := envoytest.Start(t, envoytest.Options{
    Binary:    os.Getenv("ENVOY_BIN"),
    Module:    "/absolute/path/to/libmodule.so",
    Bootstrap: bootstrapTemplate,
    Env:       []string{"GODEBUG=cgocheck=0"},
})
// Send requests to process.URL; inspect process.AdminURL when needed.
```

Templates receive `.ProxyPort`, `.AdminPort`, and `.Module`. The caller owns config
semantics, module compilation, protocol fixtures and traffic assertions. The optional
`HTTPBootstrap` helper builds a simple HCM around trusted test config fragments.
The harness imports no Envoy SDK. Keep fixture listeners on loopback.

Ports are reserved together and released immediately before launch. A small
close-and-bind race remains, as in Transit; failed startup reports Envoy's log. Cleanup
interrupts Envoy, waits up to two seconds, then kills/reaps it if necessary. No background
process is intentionally left behind. The caller explicitly opts into the pinned
SDK's `GODEBUG=cgocheck=0` workaround; the harness does not change it globally.

## Qualification and lanes

On 2026-10-06, `TestModule/inline` and `TestModule/file` passed against the native
macOS arm64 `0a804c57cf5f` / 1.40.0-dev Envoy. The file case qualified initial load,
atomic replacement, stale rejection after invalid content, and recovery. These are
module-behavior tests; they do not qualify remote mTLS, SDS or certificate rollover.

- `make native-test`: direct host process, matching newer SDK override.
- `make native-docker-test`: optional single Linux environment using the existing
  Envoy 1.38 image and default SDK; Go tests launch Envoy inside that environment.
- `make integration`: existing EG/k3d lane for control-plane and PKI behavior.

Fig can import this runner without importing Kona's module implementation. Its own
specs, filters and assertions remain in Fig. Transit remains unchanged.
