# Kona spike

Prove that small route references can select independently refreshed application data in
an Envoy dynamic module. Request processing reads an immutable cache and never waits on
network I/O. Inline JSON bytes, a mounted JSON file, and remote HTTPS share the same store.
The spike's document is a JSON object keyed by route key; remote polling replaces one
bounded document. Incremental updates, gRPC and ConnectRPC adapters are future work.

The remote source uses the Envoy 1.38 Go SDK config-context HTTP callout, scheduled on
Envoy's main dispatcher. EG generates the upstream cluster from HTTPRoute/Service and
BackendTLSPolicy. EnvoyProxy references a client TLS Secret. EG supplies the client
credential over SDS; Envoy owns TLS and validates the server name and trust root.
cert-manager issues and renews separate client/server certificates under a disposable
application CA. We do not reuse EG's internal control-plane CA or client identity.

A direct Go TLS client would need its own credential delivery and rotation. A loopback
Envoy listener is another possible transport adapter, but config-context callouts avoid
that extra listener. Generic SDS secrets are unnecessary for this transport.

Invariants: exact one source; 1 MiB document bound; invalid refresh never replaces valid
state or renews freshness; absent/stale keys return 503; remote calls do not overlap;
config destruction stops polling; all Envoy APIs execute on their required dispatcher.
The data service requires trusted client certificates AND the authorized URI SAN.

Live qualification: EG installed using dio/egtest; module loaded; active TLS/SDS config;
correct application output; data-only updates with unchanged route resourceVersion;
missing/untrusted client rejection at deployed service; automatic client reissuance
observed by server without Envoy pod replacement; stale-source failure; cluster removal.

Limits: this is one document, one gateway identity, and in-memory data. Admin mutation
port is loopback-only and has no Service; reach only through the fixture's scoped port-forward.
It is not production authorization. Server trust and identity reload at each handshake. CA rollover uses
observed A -> A+B -> B stages; see [the procedure](ca-rollover.md). The fixture forces fresh TLS connections to observe client rotation.
Connection draining/revocation and streaming credential rotation require another test.

The pinned Go SDK requires `GODEBUG=cgocheck=0` in the Envoy process, matching EG's
[Go dynamic-module example](https://gateway.envoyproxy.io/docs/tasks/extensibility/dynamic-modules/).
Without it, this spike reproduced the unpinned-pointer crash documented in
[Envoy issue 44123](https://github.com/envoyproxy/envoy/issues/44123). This disables the
runtime pointer checker; it is an SDK limitation to revisit before production adoption.
