# Local validation — 2026-10-06

Host: macOS/arm64 with OrbStack. Native module and data service: Linux/arm64 Docker
images. Envoy v1.38.0, SDK commit f1dd21b16c24, Go 1.27.1, EG v1.9.1,
k3s v1.33.13-k3s2, cert-manager v1.21.2. Local checkout; no CI qualification claimed.

## Local checks

- Native c-shared module and data-service/bootstrap image builds passed.
- Race tests passed for `./internal/...`, `./integration` (live gate disabled), and
  `./cmd/bootstrap`. Coverage includes bootstrap retries, ownership rejection,
  preservation of rollover trust, repairing leaves using the persisted B issuer,
  immutable TLS configurations, projected Secret replacement, and malformed trust.
- Go vet passed for the application and integration packages.
- Helm lint/package passed. Alternate release/namespace rendering retained the module
  name and selected the corresponding EG cluster; invalid issuer mode was rejected.

## Live Helm qualification

Both issuer pathways use a disposable egtest k3d cluster and the actual Kona chart:

```sh
KONA_INTEGRATION=1 KONA_ISSUER=certgen go test -v -count=1 -timeout 25m ./integration
KONA_INTEGRATION=1 KONA_ISSUER=cert-manager go test -v -count=1 -timeout 25m ./integration
```

The fixture checks:

- Installation and idempotent Helm upgrade preserve client credentials.
- The native module returns data fetched from the deployed mTLS service. Data-only
  updates change responses without changing the HTTPRoute resourceVersion.
- Missing and untrusted client certificates produce TLS errors; a trusted but
  unauthorized URI SAN produces HTTP 403. A server-name mismatch fails and recovers
  when the configured identity is restored.
- Client leaf rotation reaches the service through EG/SDS without replacing Envoy.
- Invalid source data expires the cache to HTTP 503; valid data restores delivery.
- A -> A+B -> B CA rollover is gated on active Envoy trust and projected service
  trust, then observed client/server leaf identities. Old client certificates are
  rejected after retirement. Restoring an A-signed server causes cache expiry/503;
  restoring the B server recovers delivery.
- A final Helm upgrade preserves B-only trust, B credentials, data delivery, and
  peer pod identities/restart counts.

Availability is sampled every 100 ms during the planned CA migration, ending before
intentional rejection tests. Zero sampled failures is not a continuous availability
or load guarantee. Long-lived streams are outside this qualification.

| Final run | TestKona duration | Rollover probes | Failures |
|---|---:|---:|---:|
| certgen | 705.50 seconds | 1,591 | 0 |
| cert-manager | 497.13 seconds | 1,532 | 0 |

Both passed, including the post-rollover upgrade. Both fixture clusters and private
kubeconfigs were removed with cleanup verified; `k3d cluster list` showed no remaining
clusters. Logs are saved in ignored `artifacts/helm-certgen.log` and
`artifacts/helm-cert-manager.log`; they contain no private keys or raw Envoy configuration.

## Setup findings and scope

The final certgen run needed an explicit repeat of `k3d image import`: the initial
import reported success but the node lacked both images. Reimporting recovered setup;
no application changes were needed. An earlier complete Helm/certgen run passed
without this intervention (476.62 seconds, 1,655 rollover probes, zero failures).

The initial spike exposed the pinned SDK's cgo pointer-check crash; the deployment
uses the documented `GODEBUG=cgocheck=0` workaround. Direct TLS probes use fresh
port-forwards and require actual TLS alerts, since a TLS rejection can terminate a
forward and an arbitrary network error would not prove certificate rejection.

HTTPS polling is live-qualified; file replacement and inline/store behavior have local
coverage. No gRPC/ConnectRPC adapter, delta feed, throughput/load qualification, strict
cgo-check build, or automated production CA rollover controller is claimed. Cert-manager
scheduled renewal is configured, but live tests force reissuance instead of waiting for
expiry. Certgen has no unattended renewal. Removing a CA does not revoke an established
connection; production stream draining remains unimplemented.

Before the spike there were no k3d clusters. The authorized unused-image prune reclaimed
1.174 GB and preserved images referenced by existing containers. These results predate publication of the source to `dio/kona`. Images and the packaged
chart remain local artifacts; they have not been published to an image/chart registry.
