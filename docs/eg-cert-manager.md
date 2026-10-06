# Install EG and Kona with one cert-manager controller

Use this optional path on a fresh local cluster **instead of step 4** of the
[manual walkthrough](local-walkthrough.md). Complete steps 1–3 first, preserving
`KONA_KUBECONFIG` and importing both Kona images. All commands run from the repository root.

This configures cert-manager for EG's control-plane certificates as well as Kona's
application certificates, with [separate CAs and leaf identities](certificates.md#why-eg-and-kona-use-separate-cas).
The manifest targets EG v1.9.1, namespace `envoy-gateway-system`, and domain `cluster.local`.
It is a fresh-install procedure; it does not migrate an already running EG CA.

## 1. Install cert-manager with Helm

```sh
helm upgrade --install cert-manager oci://quay.io/jetstack/charts/cert-manager \
  --version v1.21.2 --namespace cert-manager --create-namespace \
  --kubeconfig "$KONA_KUBECONFIG" --set crds.enabled=true \
  --wait --wait-for-jobs --timeout 5m
```

## 2. Issue EG's certificates before installing EG

```sh
kubectl --kubeconfig "$KONA_KUBECONFIG" apply -f examples/eg-cert-manager.yaml
kubectl --kubeconfig "$KONA_KUBECONFIG" -n envoy-gateway-system \
  wait certificate/envoy-gateway-ca issuer/eg-issuer \
  --for=condition=Ready --timeout=180s
kubectl --kubeconfig "$KONA_KUBECONFIG" -n envoy-gateway-system \
  wait certificate/envoy-gateway certificate/envoy certificate/envoy-rate-limit \
  --for=condition=Ready --timeout=180s
kubectl --kubeconfig "$KONA_KUBECONFIG" -n envoy-gateway-system get certificates
```

The manifest creates a local self-signed root, a CA Issuer, and EG's three expected TLS
Secrets: `envoy-gateway`, `envoy`, and `envoy-rate-limit`. Private keys stay in Kubernetes
Secrets. It also provisions the rate-limit identity expected by EG, although this spike
does not enable or qualify the rate-limit service.

The EG root lasts one year; leaves last 24 hours and renew eight hours before expiry.
Leaf keys rotate on reissuance. Root key reuse is explicitly configured to avoid treating
a scheduled root renewal as an automatic signing-key rollover. Neither this setting nor
cert-manager alone implements coordinated control-plane CA rollover.

## 3. Install EG using the pre-created Secrets

```sh
helm upgrade --install eg oci://docker.io/envoyproxy/gateway-helm \
  --version v1.9.1 --namespace envoy-gateway-system \
  --kubeconfig "$KONA_KUBECONFIG" \
  -f integration/testdata/helm-values.yaml \
  --set kubernetesClusterDomain=cluster.local --wait --timeout 5m
kubectl --kubeconfig "$KONA_KUBECONFIG" -n envoy-gateway-system \
  rollout status deployment/envoy-gateway --timeout=180s
```

Keep EG's certgen hook enabled and do not pass an overwrite flag. The hook preserves
these existing TLS Secrets and creates EG's other required material, such as its OIDC
HMAC Secret. EG's [custom certificate guide](https://gateway.envoyproxy.io/docs/install/custom-cert/)
describes this contract. Changing the cluster domain requires matching changes to the
controller certificate's DNS names and the Helm value.

## 4. Install Kona with its own cert-manager issuer

```sh
helm upgrade --install kona ./charts/kona \
  --namespace kona --create-namespace --kubeconfig "$KONA_KUBECONFIG" \
  --set pki.mode=cert-manager \
  --set images.proxy.pullPolicy=Never --set images.data.pullPolicy=Never \
  --wait --wait-for-jobs --timeout 5m
kubectl --kubeconfig "$KONA_KUBECONFIG" -n kona \
  wait gateway/kona --for=condition=Programmed --timeout=180s
kubectl --kubeconfig "$KONA_KUBECONFIG" -n envoy-gateway-system \
  rollout status deployment/kona-proxy --timeout=180s
```

Continue at **step 6** of the [manual walkthrough](local-walkthrough.md#6-reach-the-proxy-and-data-service-admin)
for requests, data updates, positive/negative mTLS checks, and cleanup. Do not repeat its
EG installation or certificate-choice steps.

## 5. Inspect the separate authorities

```sh
kubectl --kubeconfig "$KONA_KUBECONFIG" -n envoy-gateway-system get certificates,issuers
kubectl --kubeconfig "$KONA_KUBECONFIG" -n kona get certificates,issuers
kubectl --kubeconfig "$KONA_KUBECONFIG" -n envoy-gateway-system get secret envoy-gateway-ca \
  -o go-template='{{index .data "tls.crt" | base64decode}}' \
  | openssl x509 -noout -subject -fingerprint -sha256
kubectl --kubeconfig "$KONA_KUBECONFIG" -n kona get secret kona-ca \
  -o go-template='{{index .data "tls.crt" | base64decode}}' \
  | openssl x509 -noout -subject -fingerprint -sha256
```

Expected: both namespaces have Ready Certificates issued by their respective CA Issuers,
and the two CA fingerprints differ. Only public certificates are printed.

Certificate issuance and Secret renewal are distinct from consumers adopting the renewed
material. Do not infer uninterrupted control-plane renewal or root rollover from Ready
Certificates or initial traffic success. Kona's [application CA rollover](ca-rollover.md)
tests concern its data channel; they do not qualify EG's control-plane rollover.

## Local qualification

On 2026-10-06 this sequence passed on a fresh k3d/k3s cluster on macOS/arm64:
cert-manager and EG Helm installation, all EG Certificates Ready, Kona installation,
Gateway programmed, an actual `{"message":"one"}` response, authenticated data-service
receipts, and different EG/Kona root fingerprints. The three EG TLS Secrets retained
cert-manager's `eg-issuer` annotation. The disposable cluster was then removed.
This verifies initial operation; scheduled control-plane renewal and CA rollover remain
unqualified. The rate-limit service was not deployed.
