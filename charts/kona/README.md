# Kona Helm chart

Install Envoy Gateway first with EnvoyPatchPolicy and XDSNameSchemeV2 enabled, then:

```sh
helm upgrade --install kona ./kona-0.1.0.tgz -n kona --create-namespace \
  --set pki.mode=certgen --wait --wait-for-jobs --timeout 5m
```

The default `kona-proxy:spike` and `kona-data:spike` images must already be imported into
k3d. For registry images set `images.proxy.repository/tag` and `images.data.repository/tag`.
These image names are local build outputs, not published images. The custom proxy image
contains the native Go module matched to Envoy 1.38.0; ordinary Envoy images cannot replace it.

| Value | Meaning |
|---|---|
| `pki.mode=certgen` | Bootstrap Job creates missing self-signed PKI; no ongoing renewal |
| `pki.mode=cert-manager` | Requires cert-manager; creates renewing Certificate resources |
| `pki.trustBundle` | Explicit public PEM trust bundle for staged CA rollover |
| `pki.certManager.nextCA` | Provision separately named replacement root/issuer |
| `pki.certManager.leafIssuer` | Change only after A+B trust is observed at both peers |
| `testIdentities` | Additional unauthorized test client; normally false |

The service itself requires mTLS. The module uses Envoy's upstream TLS and EG/SDS-managed
client credentials. The chart sets `GODEBUG=cgocheck=0` for the pinned Go SDK workaround.

Rollover order: publish and observe A+B trust; move leaves to B; observe fresh delivery;
remove A. Root removal does not revoke existing TLS sessions. Full procedures and tested
limits are in the source repository's `docs/helm.md` and `docs/ca-rollover.md`.

Use a dedicated namespace. The bootstrap Job has named read access and namespace-level create access to
Secrets and ConfigMaps. Generated credentials/trust survive uninstall deliberately;
remove the named resources separately when retiring the installation. A normal upgrade
preserves credentials and does not reset an existing overlap or B-only trust bundle.
