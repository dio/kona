# Install EG, then Kona

Starting with an empty local machine? Use the [manual local walkthrough](local-walkthrough.md).

The chart is [`charts/kona`](../charts/kona). It deploys the data service, a custom Envoy
image containing `libkona.so`, EnvoyProxy, GatewayClass/Gateway, HTTPRoute,
BackendTLSPolicy, EnvoyPatchPolicy, and certificate bootstrap resources. EG remains a
separate installation. The chart does not install or take ownership of EG or cert-manager.

## Prerequisites and images

EG v1.9.1 must have EnvoyPatchPolicy enabled and `XDSNameSchemeV2` enabled. The fixture
uses [`helm-values.yaml`](../integration/testdata/helm-values.yaml). Choose the target
kubeconfig explicitly for all commands below:

```sh
export KONA_KUBECONFIG=/path/to/target-kubeconfig
helm upgrade --install eg oci://docker.io/envoyproxy/gateway-helm \
  --version v1.9.1 --namespace envoy-gateway-system --create-namespace \
  --kubeconfig "$KONA_KUBECONFIG" \
  -f integration/testdata/helm-values.yaml --wait
```

Build `kona-proxy:spike` and `kona-data:spike` with `make images`. These are local image
names, not published registry artifacts. For k3d, import both into the chosen cluster:

```sh
k3d image import -c "$KONA_CLUSTER" kona-proxy:spike kona-data:spike
```

For another cluster, publish the two images to your registry and override
`images.proxy.repository/tag` and `images.data.repository/tag`. The proxy image and Go
SDK must remain ABI-matched. A regular Envoy image lacks the module and cannot substitute
for `kona-proxy`. The chart carries the documented `GODEBUG=cgocheck=0` SDK workaround.

## Path A: self-signed certgen, no additional controller

```sh
helm upgrade --install kona ./charts/kona \
  --namespace kona --create-namespace --kubeconfig "$KONA_KUBECONFIG" \
  --set pki.mode=certgen --wait --wait-for-jobs --timeout 5m
```

A one-shot Job uses Go `crypto/x509` to create a self-signed ECDSA P-256 CA, then separate
CA-signed client and server leaves. The root lasts 365 days and leaves 24 hours. The Job
stores signing material in `<release>-ca` and creates `<release>-client`, `<release>-server`
and the public trust ConfigMap. It retries partial bootstrap by reading the saved CA.
Unlike the isolated unit-test helper, chart certgen persists the signing key in a Secret.

The Job uses a dedicated ServiceAccount and namespace Role with named `get` access to its Secrets/ConfigMap and namespace-level
`create` on Secrets/ConfigMaps. Kubernetes cannot restrict `create` using resourceNames; use a dedicated
namespace. Existing release-owned credentials and trust are preserved on upgrade. Foreign
ownership causes failure. Private keys are generated inside the cluster, never supplied
through Helm values or printed. There is no unattended renewal controller in this mode.

## Path B: cert-manager

Install cert-manager v1.21.2 (including its CRDs) first, then:

```sh
helm upgrade --install kona ./charts/kona \
  --namespace kona --create-namespace --kubeconfig "$KONA_KUBECONFIG" \
  --set pki.mode=cert-manager --wait --wait-for-jobs --timeout 5m
```

The chart creates SelfSigned/CA Issuers and root/client/server Certificates. Leaf issuance
and renewal belong to cert-manager. The bootstrap Job waits for the initial CA and
creates only the initial public trust ConfigMap, preserving an existing rollover bundle.
Certificate resources configure one-hour leaves, renewal 20 minutes before expiry, and
new private keys at renewal. Select issuer mode at installation; changing ownership
between modes on a live release is not a supported migration.

## Rollover and upgrades

`pki.trustBundle` accepts the explicit PEM bundle for the A -> A+B -> B sequence. Use
`--set-file pki.trustBundle=...` so only public certificates enter values. For cert-manager,
`pki.certManager.nextCA=true` provisions a separately named B root/issuer, and
`pki.certManager.leafIssuer=kona-ca-next` selects it after overlap is observed.
See the exact observation gates and rollback rules in [CA rollover](ca-rollover.md).

Once a trust bundle is supplied, preserve it on subsequent upgrades. `--reuse-values`
helps retain the settings. Never collapse overlap just because a Helm command returned.
The live integration test checks actual Envoy/service state and data delivery.

Certgen-generated PKI and initial trust are intentionally not Helm-generated templates
and are retained on uninstall. cert-manager Secret retention follows its controller
owner-reference settings. The explicitly managed trust ConfigMap also has a keep policy.
To retire an installation, uninstall its release and remove its named Secrets/ConfigMap
explicitly after confirming they are no longer needed. Namespace deletion is appropriate
only when the namespace is exclusively owned by this installation.

## Package and validate

```sh
make chart                  # lint and write artifacts/kona-0.1.0.tgz
make integration            # EG + actual Helm install/upgrade + mTLS/rollover tests
KONA_ISSUER=certgen make integration
```

The fixture scopes Helm to egtest's private kubeconfig and deletes its whole cluster.
The chart serves a demonstrative in-memory data service, not a production storage system.

## Observe the running module

`helm --wait` does not prove Envoy accepted a custom filter. For the release name `kona`,
forward its EG-managed proxy and request the demonstration response:

```sh
kubectl --kubeconfig "$KONA_KUBECONFIG" -n envoy-gateway-system \
  port-forward deployment/kona-proxy 8080:8080
# In another terminal:
curl --fail http://127.0.0.1:8080/
```

The expected initial body is `{"message":"one"}`. It is read from Kona's cache after a
background mTLS call to the data service. The application data service itself exposes
HTTPS 8443; its administration endpoints are loopback-only and require a pod port-forward.
