# CA rollover: A -> A+B -> B

The spike now implements and tests trust rollover for both certificate pathways. A CA
replacement is a staged trust transition, not an in-place overwrite of the signing CA.
The active leaf certificates and trust bundle are independent resources.

| Phase | Envoy server trust | Service client trust | Client / server leaves |
|---|---|---|---|
| Initial | A | A | A / A |
| Overlap | A+B | A+B | A / A |
| Client migration | A+B | A+B | B / A |
| Server migration | A+B | A+B | B / B |
| Retirement | B | B | B / B |

## Issuance

With cert-manager, create the separately named `kona-ca-next` Certificate and CA Issuer
without modifying `kona-ca`. After overlap is observed, change each active Certificate's
`issuerRef` to `kona-ca-next`. This causes cert-manager to reissue the leaves. Updating
only the old Issuer's CA Secret is not relied on to reissue existing certificates.

With certgen, create a fresh independent CA and issue B leaves; update the existing
client/server TLS Secrets only after publishing overlap. The live fixture prepares B in test-process memory, then persists B's signing
certificate/key in the release's CA Secret after the B leaves are observed. This is
necessary so a later bootstrap retry repairs missing leaves using B, not retired A.
The chart bootstrap is one-shot and does not itself
orchestrate scheduled rollover. A deployment operator/controller owns this sequence.

## Propagation and observation gates

1. Publish the concatenated PEM **A+B** in `ConfigMap/kona-ca`. EG consumes it via
   BackendTLSPolicy. The service independently mounts it at `/trust/ca.crt`.
2. Wait for the exact overlap bundle in Envoy's active validation-context material AND
   the expected bundle fingerprint at the service's loopback `/tls-state` endpoint.
   Kubernetes projected-volume delivery can lag the API update; use observation, not
   a fixed sleep. The tests also make successful direct requests with A and B clients.
3. Switch the client to B. Observe the B-signed certificate in the Kubernetes Secret and
   its serial in fresh data-service receipts from Envoy. Then switch the server to B,
   observe its serial in the projected identity, verify it using B-only roots, and fetch
   newly changed application data through Kona. This prevents cached data masking failure.
4. Publish **B-only** trust and wait for both consumers again. Verify a new client still
   works and an A client receives a TLS rejection. Compare pod UIDs and container restart
   counts; no restart is needed for these transitions.
5. Persist the completed state: certgen saves B as its active signing CA; cert-manager
   saves the B issuer selection in Helm values. Preserve B-only trust in Helm values.
   An ordinary upgrade must keep the client certificate and B-only trust unchanged.
6. In a separate negative test, reintroduce an A server after retirement. Envoy must reject
   it, the cache must expire to HTTP 503, and service must recover after restoring B.

The fixture samples application requests every 100 ms during intended rollover and fails
on a non-200 response. This is sampled availability plus fresh-data assertions, not a claim
of exhaustive handshake success under load. Negative old-server tests deliberately cause
failure and run after that measurement ends.

## Service reload behavior

`internal/tlsreload` builds an immutable TLS configuration for each handshake. It reads
trust independently from the leaf, resolves a Kubernetes Secret's `..data` symlink once
for the certificate/key pair, and rejects missing, empty or malformed material. It does
not silently retain retired roots after an invalid bundle update. An interrupted update
can therefore reject handshakes; correct ordering and atomic projected-volume publication
are part of the contract.

TLS session tickets are disabled on the fixture, so resumption cannot skip renewed client
trust checks. Responses close their connections to make every poll exercise a new
handshake. Removing a trust root does **not** revoke an already established TLS session.
Production gRPC/ConnectRPC streams require a bounded drain/reconnect policy before root
retirement can be considered complete. That streaming-drain policy is not implemented.

## Helm sequence (cert-manager)

Choose the target namespace/kubeconfig and keep a copy of the current values. In the
commands below the release is `kona`, namespace `kona`:

```sh
helm upgrade kona ./charts/kona -n kona --kubeconfig "$KONA_KUBECONFIG" \
  --reuse-values --set pki.certManager.nextCA=true --wait --wait-for-jobs
```

Wait for `Certificate/kona-ca-next` Ready. Extract only the public root `tls.crt` values
from `kona-ca` and `kona-ca-next` into `a.pem` and `b.pem`, then concatenate to `ab.pem`.
Do not include private keys. Install overlap:

```sh
helm upgrade kona ./charts/kona -n kona --kubeconfig "$KONA_KUBECONFIG" \
  --reuse-values --set-file pki.trustBundle=ab.pem --wait --wait-for-jobs
```

Apply the observation gates above, then switch leaves:

```sh
helm upgrade kona ./charts/kona -n kona --kubeconfig "$KONA_KUBECONFIG" \
  --reuse-values --set pki.certManager.leafIssuer=kona-ca-next --wait --wait-for-jobs
```

The chart switches both leaves after both peers already trust A+B. The integration test
also exercises the intermediate B-client/A-server state by switching them separately.
Only after B leaves and fresh delivery are observed should you retire A:

```sh
helm upgrade kona ./charts/kona -n kona --kubeconfig "$KONA_KUBECONFIG" \
  --reuse-values --set-file pki.trustBundle=b.pem --wait --wait-for-jobs
```

During overlap, rolling leaves back to A is possible. After retirement, **restore A+B
trust and observe both peers before rolling a leaf back to A**. Never use a blind Helm
rollback to bypass these gates. Keep old issuer material only for a deliberate rollback
window, then retire it under the deployment's key-retention policy. The fixture retains
it only until cluster cleanup, so it can run the old-server rejection test.

For automatic multi-namespace bundle propagation, trust-manager can distribute bundles;
it does not replace the observation/migration/drain sequence above. This chart does not
install trust-manager. References: [cert-manager CA Issuer](https://cert-manager.io/docs/configuration/ca/)
and [Go TLS configuration](https://pkg.go.dev/crypto/tls#Config).
