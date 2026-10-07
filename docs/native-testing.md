# Single-Envoy native tests

Following `dio/transit/examples/internal/e2etest`, `github.com/dio/kona/envoytest`
starts **one Envoy child process from a Go test**, with a temporary bootstrap,
loopback ports, bounded readiness polling, captured logs and automatic cleanup.
Application fixtures can use `httptest.Server`, including TLS, in the test process.
No Docker, Compose, Kubernetes or Envoy Gateway is involved in this path.

## Host requirements

Requires Go 1.27.1, a working cgo compiler (Xcode Command Line Tools on macOS),
and a host-native Envoy binary built at commit `f1dd21b16c24` (Envoy 1.38), matching
`go.mod`. All lanes use the same SDK; there is no separate native dependency override.
The previously qualified macOS `0a804c57cf5f` / 1.40.0-dev binary is incompatible
with this SDK and must not be used with the current module build.

```sh
make native-test ENVOY_BIN=/absolute/path/to/matching/envoy
```

Or explicitly:

```sh
make native-build
ENVOY_BIN=/absolute/path/to/matching/envoy \
KONA_MODULE="$PWD/.bin/libkona.so" \
go test -v -count=1 -timeout=60s ./native
```

The `.so` suffix is used by convention; on macOS this is a Mach-O shared library.
A Linux `.so` cannot be loaded by macOS Envoy. `make native-test` checks the expected
Envoy commit before running; direct `go test` callers must choose matching artifacts.
The runner does not silently skip a missing binary or library. The native suite skips
only when `ENVOY_BIN` is not set, so normal repository unit tests remain portable.

If a matching host binary is unavailable, use `make native-docker-test` to build and
run the suite with the pinned Linux Envoy image.

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
That historical run used a separate SDK override, which has since been removed;
it does not qualify the current host lane with the default SDK.

- `make native-test`: direct host process, matching the SDK in `go.mod`.
- `make native-docker-test`: optional single Linux environment using the existing
  Envoy 1.38 image and default SDK; Go tests launch Envoy inside that environment.
- `make integration`: existing EG/k3d lane for control-plane and PKI behavior.

Fig can import this runner without importing Kona's module implementation. Its own
specs, filters and assertions remain in Fig. Transit remains unchanged.
