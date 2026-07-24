# scw-l2announce-lb-controller Helm chart

Deploys the [scw-l2announce-lb-controller](../../README.md) and — by default —
everything a **stock Scaleway Kapsule** cluster is missing for Cilium L2
announcements to work at all:

- the controller Deployment (2 replicas, leader election, spread across nodes),
  its ServiceAccount and RBAC;
- a `CiliumL2AnnouncementPolicy` announcing LoadBalancer VIPs on the
  private-network NICs (`ens*`);
- a `CiliumNodeConfig` turning `enable-l2-announcements` on for all nodes
  (Kapsule's managed Cilium ships with it off);
- the lease Role/RoleBinding for the `cilium` ServiceAccount that the upstream
  Cilium chart would create with `l2announcements.enabled=true`.

Requires Cilium ≥ 1.18 (`cilium.io/v2` IP pools).

## Install

First create the credentials Secret. Its keys are the `SCW_*` environment
variables read by the controller — `SCW_ACCESS_KEY` and `SCW_SECRET_KEY` are
always required; `SCW_DEFAULT_PROJECT_ID` and `SCW_DEFAULT_REGION` are not
secret and can live either here or in the chart values (`scaleway.projectID` /
`scaleway.region`, which are rendered into a ConfigMap and take precedence
over the Secret):

```sh
kubectl -n scw-l2lb create secret generic scw-credentials \
  --from-literal=SCW_ACCESS_KEY=<access-key> \
  --from-literal=SCW_SECRET_KEY=<secret-key> \
  --from-literal=SCW_DEFAULT_PROJECT_ID=<project-uuid> \
  --from-literal=SCW_DEFAULT_REGION=fr-par
```

Released versions are published to GHCR as an OCI chart:

```sh
helm install l2lb oci://ghcr.io/iliaditalia/charts/scw-l2announce-lb-controller \
  --version <X.Y.Z> \
  --namespace scw-l2lb --create-namespace \
  --set scaleway.existingSecret=scw-credentials \
  --set scaleway.pnID=<private-network-uuid>
```

For development, install from the repo checkout instead:

```sh
helm install l2lb ./charts/scw-l2announce-lb-controller --set image.tag=<commit-sha> ...
```

On a stock Kapsule cluster, restart the Cilium agents once after the first
install so they pick up the `CiliumNodeConfig`:

```sh
kubectl -n kube-system rollout restart daemonset/cilium
```

Then opt Services in as described in the [project README](../../README.md#usage).

## Values

The full contract is [`values.schema.json`](values.schema.json) (strict:
unknown keys fail `helm template`). The essentials:

| Key | Default | Purpose |
|---|---|---|
| `scaleway.existingSecret` | — (required) | Secret with `SCW_ACCESS_KEY` + `SCW_SECRET_KEY` (and project/region unless set below) |
| `scaleway.pnID` | — (required) | Scaleway private network the VIPs are booked from |
| `scaleway.projectID` / `scaleway.region` | `""` (from Secret) | Non-secret settings; rendered into a ConfigMap, take precedence over the Secret |
| `image.repository` | `ghcr.io/iliaditalia/scw-l2announce-lb-controller` | Controller image |
| `image.tag` | `Chart.appVersion` | Released charts pin this to the release version; set a commit SHA for unreleased builds |
| `replicaCount` | `2` | HA: standby takes over within the 15s lease timeout. Leader election is on automatically whenever > 1 |
| `resyncPeriod` | `""` (controller default) | Full drift-resync interval |
| `cilium.l2AnnouncementPolicy.enabled` | `true` | Create the announcement policy |
| `cilium.l2AnnouncementPolicy.interfaces` | `["^ens[0-9]+$"]` | Announced interface regexes |
| `cilium.l2AnnouncementPolicy.serviceSelector` | `{}` (all) | Which Services get announced |
| `cilium.kapsuleBootstrap.enabled` | `true` | CiliumNodeConfig + cilium lease RBAC; disable if your Cilium already has L2 announcements enabled |
| `cilium.namespace` | `kube-system` | Where Cilium lives |
| `metrics.service.enabled` | `true` | ClusterIP Service for `/metrics` |
| `metrics.serviceMonitor.enabled` | `false` | Prometheus Operator ServiceMonitor |
| `podDisruptionBudget.enabled` | `true` | PDB (rendered when `replicaCount` > 1) |

Standard workload knobs (`resources`, `nodeSelector`, `tolerations`,
`affinity`, `topologySpreadConstraints`, `priorityClassName`,
`podAnnotations`, `podLabels`, `podSecurityContext`, `securityContext`,
`serviceAccount.*`, `imagePullSecrets`, `nameOverride`, `fullnameOverride`)
work as in any conventional chart.
