# Manual local walkthrough, starting from an empty machine

This starts with no Kubernetes cluster, EG, certificates, or prebuilt Kona images.
It leaves a running cluster for inspection; nothing invokes the integration test runner.
The primary path is macOS (Apple Silicon or Intel), with Linux prerequisites below.
The existing live qualification was on macOS/arm64 with OrbStack; this document's
steps have been checked against the chart and CLI, but not replayed on a fresh OS.

## 1. Install tools and obtain the source

On macOS:

1. Install [Homebrew](https://brew.sh/) using its installer and follow its printed
   shell setup instructions. Install the Xcode command-line tools if prompted.
2. Install and launch [Docker Desktop](https://docs.docker.com/desktop/setup/install/mac-install/).
   Wait until its engine is running. An existing OrbStack Docker engine also works.
3. Install the remaining tools:

   ```sh
   brew install git make k3d kubectl helm
   ```

On Linux, install Git, Make, curl and OpenSSL with your distribution's package manager,
then [Docker Engine](https://docs.docker.com/engine/install/),
[k3d](https://k3d.io/stable/), [kubectl](https://kubernetes.io/docs/tasks/tools/),
and [Helm](https://helm.sh/docs/intro/install/) using their installation instructions.
Ensure your user can run Docker. The remaining commands use Bash on either OS.

Check the tools before continuing:

```sh
docker info
k3d version
kubectl version --client
helm version
make --version
curl --version
openssl version
```

Go is compiled inside Docker; a host Go installation is unnecessary for this manual
path. Native Go tests separately require the version in `go.mod`.

Clone the public source repository, including the chart and EG values:

```sh
git clone https://github.com/dio/kona.git
cd kona
```

Run subsequent commands from this directory, in the same Bash terminal unless stated.
Internet access is required for base images, Go dependencies, and the EG Helm chart.

## 2. Create an isolated local cluster

Use a new name if `kona-manual` already exists; do not overwrite another cluster.
The private kubeconfig below leaves your default context unchanged.

```sh
export KONA_CLUSTER=kona-manual
export KONA_RUN_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kona-manual.XXXXXX")"
export KONA_KUBECONFIG="$KONA_RUN_DIR/kubeconfig"
umask 077

k3d cluster create "$KONA_CLUSTER" \
  --image rancher/k3s:v1.33.13-k3s2 \
  --servers 1 --agents 0 \
  --k3s-arg '--disable=traefik@server:0' \
  --kubeconfig-update-default=false --kubeconfig-switch-context=false \
  --wait --timeout 5m
k3d kubeconfig get "$KONA_CLUSTER" > "$KONA_KUBECONFIG"
kubectl --kubeconfig "$KONA_KUBECONFIG" get nodes
```

Expected: one Ready node. Keep the value of `KONA_RUN_DIR` if you close the terminal.

## 3. Build and import the two images

```sh
make images
k3d image import -c "$KONA_CLUSTER" kona-proxy:spike kona-data:spike
```

The first build downloads the toolchain and dependencies. Images use the host's native
architecture. The proxy image includes `libkona.so` and pins Envoy to its Go SDK ABI.
The data image also contains the in-cluster certificate bootstrap binary.

## 4. Install Envoy Gateway

```sh
helm upgrade --install eg oci://docker.io/envoyproxy/gateway-helm \
  --version v1.9.1 --namespace envoy-gateway-system --create-namespace \
  --kubeconfig "$KONA_KUBECONFIG" \
  -f integration/testdata/helm-values.yaml --wait --timeout 5m
kubectl --kubeconfig "$KONA_KUBECONFIG" -n envoy-gateway-system get pods
```

The supplied values enable EnvoyPatchPolicy and XDSNameSchemeV2, both required by this
spike. EG will create the actual proxy after Kona creates its Gateway.

## 5. Choose certificate issuance, then install Kona

### A. Certgen: simplest local path

```sh
export KONA_ISSUER=certgen
```

No certificate controller is needed. The bootstrap Job generates a self-signed CA and
separate CA-signed client/server leaves inside the cluster. It persists keys in Secrets.
Leaves last 24 hours; this mode has no automatic renewal. Reinstalling/upgrading preserves
existing credentials rather than renewing them.

### B. Cert-manager: managed leaf renewal

Instead of A, install cert-manager using its [official Helm chart](https://cert-manager.io/docs/installation/helm/) before Kona. This installs its CRDs as well:

```sh
helm upgrade --install cert-manager oci://quay.io/jetstack/charts/cert-manager \
  --version v1.21.2 --namespace cert-manager --create-namespace \
  --kubeconfig "$KONA_KUBECONFIG" --set crds.enabled=true \
  --wait --wait-for-jobs --timeout 5m
for deployment in cert-manager cert-manager-cainjector cert-manager-webhook; do
  kubectl --kubeconfig "$KONA_KUBECONFIG" -n cert-manager \
    rollout status "deployment/$deployment" --timeout=180s
done
export KONA_ISSUER=cert-manager
```

The Kona chart creates its own SelfSigned/CA Issuers and leaf Certificates. It does not reuse
EG's control-plane CA. See [certificate details](certificates.md) for lifetimes and trust.

### Install (either choice)

```sh
helm upgrade --install kona ./charts/kona \
  --namespace kona --create-namespace --kubeconfig "$KONA_KUBECONFIG" \
  --set pki.mode="$KONA_ISSUER" \
  --set images.proxy.pullPolicy=Never --set images.data.pullPolicy=Never \
  --wait --wait-for-jobs --timeout 5m
kubectl --kubeconfig "$KONA_KUBECONFIG" -n kona \
  wait gateway/kona --for=condition=Programmed --timeout=180s
kubectl --kubeconfig "$KONA_KUBECONFIG" -n envoy-gateway-system \
  rollout status deployment/kona-proxy --timeout=180s
kubectl --kubeconfig "$KONA_KUBECONFIG" -n kona get pods,jobs,gateway,httproute
```

Expected: data Deployment ready, bootstrap Job complete, Gateway programmed. The chart
uses the pinned SDK's documented `GODEBUG=cgocheck=0` workaround. This is a spike, not a
strict-cgo-qualified build. Do not switch certificate modes on an existing release.

## 6. Reach the proxy and data-service admin

Start both forwards in the same terminal; preserve their PIDs for cleanup:

```sh
kubectl --kubeconfig "$KONA_KUBECONFIG" -n envoy-gateway-system \
  port-forward deployment/kona-proxy 18080:8080 > "$KONA_RUN_DIR/proxy-forward.log" 2>&1 &
KONA_PROXY_FORWARD_PID=$!
kubectl --kubeconfig "$KONA_KUBECONFIG" -n kona \
  port-forward deployment/kona-data 18081:8080 > "$KONA_RUN_DIR/admin-forward.log" 2>&1 &
KONA_ADMIN_FORWARD_PID=$!
cat "$KONA_RUN_DIR/proxy-forward.log" "$KONA_RUN_DIR/admin-forward.log"
```

Wait until both logs report `Forwarding from 127.0.0.1`. If a port is occupied, change
its local number consistently below. Then:

```sh
curl --fail http://127.0.0.1:18080/
curl --fail http://127.0.0.1:18081/receipts
curl --fail http://127.0.0.1:18081/tls-state
```

Expected proxy body: `{"message":"one"}`. Receipts show client certificate serials
and request counts, proving that the deployed service receives authenticated calls.
`tls-state` describes the service's current certificate and trust. Admin HTTP is bound
only to pod loopback and reached through kubectl; service port 8443 requires mTLS.
The user-facing demonstration listener is HTTP; the independent **data channel** is mTLS.

## 7. Change app data without changing the route

```sh
KONA_ROUTE_BEFORE=$(kubectl --kubeconfig "$KONA_KUBECONFIG" -n kona \
  get httproute kona-app -o jsonpath='{.metadata.resourceVersion}')
curl --fail -X PUT http://127.0.0.1:18081/data \
  -H 'Content-Type: application/json' --data '{"demo":{"message":"two"}}'
curl --fail http://127.0.0.1:18080/
KONA_ROUTE_AFTER=$(kubectl --kubeconfig "$KONA_KUBECONFIG" -n kona \
  get httproute kona-app -o jsonpath='{.metadata.resourceVersion}')
test "$KONA_ROUTE_BEFORE" = "$KONA_ROUTE_AFTER" && echo 'HTTPRoute unchanged'
```

Repeat the proxy curl until it returns `{"message":"two"}` (poll interval 500 ms).
The document maps route keys to JSON values. The module returns the `demo` value.
The data is in memory; restarting the data pod resets it.

To observe stale-data failure, PUT valid JSON of the wrong shape (`[]`), then repeat
the proxy request after the three-second freshness window:

```sh
curl --fail -X PUT http://127.0.0.1:18081/data \
  -H 'Content-Type: application/json' --data '[]'
curl -i http://127.0.0.1:18080/
```

Expected after cache expiry: HTTP 503. Restore the object from the preceding example
and observe recovery. Invalid JSON itself is rejected by the admin endpoint.

## 8. Confirm that the data service requires a client certificate

Extract only the public root, then forward HTTPS:

```sh
kubectl --kubeconfig "$KONA_KUBECONFIG" -n kona get configmap kona-ca \
  -o go-template='{{index .data "ca.crt"}}' > "$KONA_RUN_DIR/ca.pem"
kubectl --kubeconfig "$KONA_KUBECONFIG" -n kona \
  port-forward service/kona-data 18443:8443 > "$KONA_RUN_DIR/tls-forward.log" 2>&1 &
KONA_TLS_FORWARD_PID=$!
```

After the log shows forwarding, run:

```sh
curl --verbose --noproxy '*' --cacert "$KONA_RUN_DIR/ca.pem" \
  --resolve kona-data.kona.svc.cluster.local:18443:127.0.0.1 \
  https://kona-data.kona.svc.cluster.local:18443/data
```

Expected: TLS rejection because no client certificate was supplied. Require an actual
TLS certificate-required alert; connection-refused or an untrusted-server error does
not prove client authentication. The failed handshake can stop kubectl's forward.
The successful proxy call and receipts in step 6 are the positive mTLS check.

## 9. Upgrade and explore CA rollover

```sh
helm upgrade kona ./charts/kona -n kona --kubeconfig "$KONA_KUBECONFIG" \
  --reuse-values --wait --wait-for-jobs --timeout 5m
curl --fail http://127.0.0.1:18080/
```

For CA migration, follow [CA rollover](ca-rollover.md): publish A+B, observe both
consumers, migrate leaves, observe fresh delivery, then retire A. That guide provides
cert-manager Helm commands and the required observation gates. Certgen rollover is
currently exercised by the integration fixture; there is no one-command manual certgen
rollover tool. Do not replace a CA Secret alone and expect a coordinated rollover.

## 10. Troubleshooting and cleanup

For a failed installation:

```sh
kubectl --kubeconfig "$KONA_KUBECONFIG" -n kona get events --sort-by=.lastTimestamp
kubectl --kubeconfig "$KONA_KUBECONFIG" -n kona get jobs,pods
kubectl --kubeconfig "$KONA_KUBECONFIG" -n kona describe gateway kona
kubectl --kubeconfig "$KONA_KUBECONFIG" -n kona get envoypatchpolicy -o yaml
```

- `ErrImageNeverPull`: repeat step 3's import into this exact cluster; an earlier local
  import reported success without placing the images on the node.
- Bootstrap failure: inspect its Job logs (`kubectl ... logs job/<name>`).
- 503: inspect data-service logs and receipts, TLS Secrets, BackendTLSPolicy conditions,
  and whether the three-second cache freshness window has expired.
- Gateway ready but no module response: inspect EnvoyPatchPolicy status and proxy logs.
  `helm --wait` alone does not prove filter acceptance.
- After certificate/trust updates, projected volumes may lag; use observed state.

When finished, delete only this walkthrough's cluster and temporary files:

```sh
kill "$KONA_PROXY_FORWARD_PID" "$KONA_ADMIN_FORWARD_PID" 2>/dev/null || true
if [ -n "${KONA_TLS_FORWARD_PID:-}" ]; then
  kill "$KONA_TLS_FORWARD_PID" 2>/dev/null || true
fi
k3d cluster delete "$KONA_CLUSTER"
rm -f "$KONA_KUBECONFIG" "$KONA_RUN_DIR/ca.pem" \
  "$KONA_RUN_DIR/proxy-forward.log" "$KONA_RUN_DIR/admin-forward.log" \
  "$KONA_RUN_DIR/tls-forward.log"
rmdir "$KONA_RUN_DIR"
k3d cluster list
```

The two Docker images remain for reuse. Full cluster deletion also removes retained
certificate Secrets; Helm uninstall alone deliberately retains some PKI material.
