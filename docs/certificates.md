# Certificate issuance and trust

Issuance and delivery are separate responsibilities. Kona uses an Envoy-owned HTTPS
connection: the module names a cluster; EG configures that cluster; Envoy performs TLS.
No certificate or private key is passed in module configuration.

## Why EG and Kona use separate CAs

One cert-manager installation can manage certificates for both EG and Kona. The
controller manages issuance and renewal; each Certificate's issuer reference selects
its signing authority. Sharing the controller does not imply sharing a CA or leaf key.

| Connection | Signing authority | Leaf identities |
|---|---|---|
| Envoy to EG control plane (including xDS) | EG control-plane CA | EG server and Envoy control-plane client |
| Envoy/Kona to application data service | Kona application CA | Kona upstream client and data-service server |

The same Envoy process uses different client certificates for these connections.
Kona's upstream client credential is delivered through EG/SDS; it is independently
issued from the credential Envoy uses to authenticate to EG itself.

Separate CAs keep application trust changes and CA rollover independent of control-plane
trust. A compromised application signing key should not become a trusted authority for
control-plane identities. Leaf certificates also remain distinct, with the SANs, usages,
and private keys appropriate to their roles. Application authorization still verifies
Kona's exact client URI; a trusted chain alone does not grant access.

A shared CA could be configured deliberately, but would couple trust and rollover across
these connections. The spike keeps separate roots and issuers as its default boundary.
A shared cert-manager controller still has powerful access to both sets of Secrets;
separate CAs do not isolate them from compromise of that controller or its permissions.

### Using cert-manager for EG too

EG supports pre-provisioned control-plane certificates managed by cert-manager; see
its [custom control-plane certificate guide](https://gateway.envoyproxy.io/docs/install/custom-cert/).
To use this option, the installation order is:

1. Install cert-manager once.
2. Provision EG's CA, issuer, and required control-plane certificate Secrets; wait for readiness.
3. Install EG using those Secrets, following its version-specific custom-certificate guide.
4. Install Kona with `pki.mode=cert-manager`, creating its separate application CA and leaves.

The current walkthrough and live fixture use **EG's default certgen for the control
plane**. Selecting cert-manager for Kona changes only Kona's application certificates.
The all-cert-manager EG setup is an available extension, not a live-qualified mode of
this spike. Its control-plane renewal and CA rollover require their own propagation
checks; Kona's rollover tests do not establish control-plane rollover behavior.

## Path A: cert-manager (integration default)

```sh
make integration
# Equivalent issuer choice:
KONA_ISSUER=cert-manager make integration
```

The fixture installs the checksum-pinned cert-manager v1.21.2 release, then applies
[chart Certificate resources](../charts/kona/templates/certificates.yaml):

1. `Issuer/kona-selfsigned` has `spec.selfSigned: {}`.
2. `Certificate/kona-ca` has `isCA: true`. The SelfSigned Issuer signs this root using
   its own generated ECDSA P-256 key. cert-manager stores the root certificate and
   signing key in `Secret/kona-ca` in the disposable cluster.
3. `Issuer/kona-ca` has `spec.ca.secretName: kona-ca`. It uses that CA to issue leaves.
4. `Certificate/kona-server` requests the DNS SAN
   `kona-data.default.svc.cluster.local` and `server auth` usage.
5. `Certificate/kona-client` requests URI SAN `spiffe://kona.test/gateway` and
   `client auth` usage. This URI is an explicitly configured identity; the spike
   does not implement SPIFFE attestation or use SPIRE.
6. Both leaves have a one-hour lifetime, `renewBefore: 20m` and
   `privateKey.rotationPolicy: Always`. cert-manager handles subsequent renewal.
   A third client with URI `spiffe://kona.test/other` exercises authorization rejection.

The rotation test deletes only `Secret/kona-client`. cert-manager recreates it with a
new key and serial. The test observes the new serial at the data server and verifies
that the Envoy pod UID did not change. This proves reissuance/delivery, not the passage
of the scheduled renewal time. Root renewal alone is not a CA rollover; follow the [rollover procedure](ca-rollover.md).

## Path B: self-signed certgen fixture

```sh
KONA_ISSUER=certgen make integration
```

The chart's [`kona-bootstrap` Job](../cmd/bootstrap/main.go) uses
[`internal/pki`](../internal/pki/pki.go) and Go `crypto/x509` to generate a self-signed
ECDSA P-256 CA, then CA-signed leaves with the same SANs/usages as path A. Only the root
is self-signed. The chart root lives for 365 days and its leaves for 24 hours. Every
issuance uses a new key and random serial.

This is Kona's certgen, not EG's internal certgen binary. The Job creates release-owned
Secrets through the Kubernetes API, persists the root signing key for partial-bootstrap
retries, and preserves existing credentials and trust on upgrade. It never prints keys
or puts them in Helm values. The test harness explicitly signs a replacement client
under the persisted CA to exercise SDS delivery. There is no renewal controller in this
pathway; use cert-manager for unattended renewal. The separate test helper used to
prepare CA B initially keeps its signing key in test-process memory, then persists it
as the active certgen CA after migration so bootstrap retries cannot mint under retired A.

## How each side receives and verifies credentials

| Consumer | Material | Delivery and checks |
|---|---|---|
| EG/Envoy upstream | `kona-client` cert + key | `EnvoyProxy.backendTLS.clientCertificateRef` references the TLS Secret; EG delivers the credential through SDS |
| EG/Envoy upstream | CA public certificate, expected server DNS name | `ConfigMap/kona-ca` and `BackendTLSPolicy` configure trust and hostname validation |
| Data server | `kona-server` cert + key | Kubernetes projected Secret mounted at `/tls`; server reloads the leaf at each handshake |
| Data server | CA public certificate | `ConfigMap/kona-ca` at `/trust/ca.crt`; reloaded for every handshake |
| Data server | Client authorization | Requires a verified chain AND exact URI SAN `spiffe://kona.test/gateway` |

The application service exposes only HTTPS port 8443, requires TLS 1.3 and
`RequireAndVerifyClientCert`. Its mutation/receipt endpoint binds to loopback in the pod
and is reachable by the fixture's scoped port-forward. It is not exposed by the Service.

SDS is a delivery mechanism, not an issuer. These pathways use dedicated application
credentials; neither copies EG's control-plane certificate/key nor extends the trust
of EG's internal CA. The explicit trust bundle follows the
[staged CA rollover](ca-rollover.md) procedure, with observation gates at both peers. Connection reuse is disabled on fixture responses so reissued client
certificates are observable immediately on new handshakes.

## Go cgo checker: 0 versus 2

For the pinned Envoy v1.38.0 SDK the fixture sets:

```yaml
env:
- name: GODEBUG
  value: cgocheck=0
```

The SDK returns unpinned Go pointers across its C ABI. The default checker rejected
that at module configuration creation in the initial live run. `cgocheck=0` disables
runtime pointer checks; it does not fix ownership or prove memory safety.

`GODEBUG=cgocheck=2` is **not supported by modern Go** and causes a startup error.
Fuller checking uses `GOEXPERIMENT=cgocheck2` **at build time**. It is not interchangeable
with `cgocheck=0`, and the full-check experiment must not be enabled for this workaround.
A future SDK ownership fix should be qualified under that stricter build before removing
the workaround. This spike has not claimed a passing strict-check build.

References: [Go cgo pointer rules](https://pkg.go.dev/cmd/cgo#hdr-Passing_pointers),
[SDK unpinned-pointer issue](https://github.com/envoyproxy/envoy/issues/44123),
[EG dynamic-module example](https://gateway.envoyproxy.io/docs/tasks/extensibility/dynamic-modules/),
[cert-manager SelfSigned bootstrap](https://cert-manager.io/docs/configuration/selfsigned/),
[cert-manager CA Issuer](https://cert-manager.io/docs/configuration/ca/).
