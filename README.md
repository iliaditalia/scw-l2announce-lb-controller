# scw-l2announce-lb-controller

> [!WARNING]
> This is a **tech demo**. It works, but it is not battle-tested and is not
> recommended for production use.

A single-purpose Kubernetes operator that makes **Cilium L2-announced
LoadBalancer VIPs reachable from outside the VPC** on Scaleway (across VPC
peering / an interlink), by keeping a Scaleway **IPAM IP's custom-resource MAC
attachment** pointed at the node currently holding the Cilium L2-announcement
lease.

## Why

Cilium L2 announcement (`loadBalancerClass: io.cilium/l2-announcer`) answers
ARP for the VIP on the node's private-network interface — layer 2 only.
Traffic arriving *routed* through the Scaleway VPC gateway is forwarded based
on **IPAM IP → MAC mappings** and never reaches an ARP-only VIP.

The fix: reserve the VIP as a Scaleway IPAM IP and set its
`custom_resource.mac_address` to the private-NIC MAC of the lease-holder node.
The gateway then routes the VIP to that node, which already serves it via
Cilium's LB datapath — no relay or forwarding needed. This controller automates
that mapping and tracks Cilium failover.

## How it works

Per opted-in Service, the reconcile loop:

1. Ensures an IPAM IP exists (uses the one from the
   `k8s.iliad.it/scw-ipam-ip-id` annotation, or books one on the configured
   private network and persists its ID back to that annotation).
2. Publishes that IP in the Service's `spec.externalIPs`, which Cilium's L2
   announcer picks up directly (the announcement policy must announce
   `externalIPs`), and mirrors it into `status.loadBalancer.ingress` — nothing
   else populates it here, and consumers like ArgoCD health checks (and
   `kubectl get svc`) treat a LoadBalancer Service as pending until it is set.
   Cilium LB-IPAM pools are deliberately **not** used: LB-IPAM
   runs in the cilium-operator and only serves
   `loadBalancerClass: io.cilium/l2-announcer` when the *operator* has
   `enable-l2-announcements` — on Kapsule that flag can only be delivered to
   the agents (via a CiliumNodeConfig), the operator's ConfigMap is
   Scaleway-managed, so LB-IPAM ignores these Services entirely and a pool
   never materializes an IP.
3. Watches the Cilium lease `kube-system/cilium-l2announce-<ns>-<name>` and
   resolves the holder node → Scaleway server (via `spec.providerID`) → private
   NIC MAC on the target private network.
4. Attaches the IPAM IP to that MAC (`AttachIP`), or moves it (`MoveIP`) when
   the lease holder changes. Scaleway is only called when something actually
   differs; a periodic resync (default 10m) corrects drift.
5. On Service deletion or opt-out (guarded by the
   `k8s.iliad.it/scw-ipam-cleanup` finalizer): withdraws the IP from
   `spec.externalIPs` and `status.loadBalancer.ingress`, **releases** the
   IPAM IP if the controller booked it
   (recognized by the
   `managed-by=scw-l2announce-lb-controller` tag), or merely **detaches**
   user-provided IPs. Setting `k8s.iliad.it/scw-ipam-ip-externally-managed: "true"`
   forces detach-only regardless of tags.

> [!NOTE]
> Service `spec.externalIPs` is deprecated in Kubernetes, so this is a
> workaround, not the preferred design. Should Scaleway ever ship Kapsule with
> `enable-l2-announcements` enabled in the cilium-operator's ConfigMap, switch
> back to the LB-IPAM pool implementation preserved on the
> [`cilium-ippool`](https://github.com/iliaditalia/scw-l2announce-lb-controller/tree/cilium-ippool)
> branch.

## Usage

Opt in a Service:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-vip
  annotations:
    k8s.iliad.it/scw-ipam: "enabled"
    # optional: use a pre-reserved IPAM IP instead of booking one
    # k8s.iliad.it/scw-ipam-ip-id: "11111111-2222-3333-4444-555555555555"
    # optional: mark that IP as externally managed — only ever detached on
    # cleanup, never released, even if it carries the controller's tags
    # k8s.iliad.it/scw-ipam-ip-externally-managed: "true"
    # optional: override the controller-wide private network
    # k8s.iliad.it/scw-ipam-pn-id: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
spec:
  type: LoadBalancer
  loadBalancerClass: io.cilium/l2-announcer
  ...
```

A `CiliumL2AnnouncementPolicy` matching the service and announcing
`externalIPs` must exist — the controller does not manage it, but the
[Helm chart](charts/scw-l2announce-lb-controller) creates one by default. The
cluster must not run the `DenyServiceExternalIPs` admission plugin (Kapsule
does not). Only IPv4 is supported.

Do not edit the `k8s.iliad.it/scw-ipam-ip-id` annotation or the
controller-added `spec.externalIPs` entry of a managed Service by hand
(your own additional `externalIPs` entries are left alone). Note that opting
out strips both; if you supplied your own IP ID, re-add the annotation when
re-opting in — or set `k8s.iliad.it/scw-ipam-ip-externally-managed: "true"`, which
keeps the IP ID annotation across opt-out (and requires the IP ID to be set).

## Configuration

Environment (Scaleway credentials, e.g. from a Secret):

| Variable | Purpose |
|---|---|
| `SCW_ACCESS_KEY` / `SCW_SECRET_KEY` | API credentials (IPAM read/write + Instance read) |
| `SCW_DEFAULT_PROJECT_ID` | Project to book IPAM IPs in (required) |
| `SCW_DEFAULT_REGION` | Region of the IPAM IPs, e.g. `fr-par` (required) |
| `PN_ID` | Default private network ID (alternative to the flag) |
| `POD_NAMESPACE` | Leader-election namespace (downward API) |

Flags:

| Flag | Default | Purpose |
|---|---|---|
| `-private-network-id` | `$PN_ID` | Private network VIPs are reserved from (required) |
| `-kubeconfig` | in-cluster | Path to a kubeconfig (for running out of cluster) |
| `-leader-elect` | `true` | Leader election; run ≥2 replicas spread across nodes (anti-affinity / topologySpreadConstraints) in production |
| `-leader-election-namespace` | `$POD_NAMESPACE` | Namespace of the election lease |
| `-metrics-addr` | `:8080` | `/metrics` + `/healthz` listen address |
| `-resync-period` | `10m` | Full drift-resync interval |
| `-v` | | klog verbosity |

## Metrics

| Metric | Type | Meaning |
|---|---|---|
| `scw_l2lb_reconciles_total{result}` | counter | Reconciliations by success/error |
| `scw_l2lb_scaleway_mutations_total{op}` | counter | Mutating Scaleway calls (book/attach/move/release/detach) |
| `scw_l2lb_divergence{namespace,name}` | gauge | 1 while lease-holder MAC ≠ IPAM-attached MAC. **Alert if 1 for >30s**: failover did not propagate to the VPC gateway |
| `scw_l2lb_managed_services` | gauge | Opted-in services |

## RBAC

The controller needs (cluster-wide unless noted):

```yaml
- apiGroups: [""]
  resources: [services]
  verbs: [get, list, watch, patch]
- apiGroups: [""]
  resources: [services/status]
  verbs: [patch]
- apiGroups: [""]
  resources: [nodes]
  verbs: [get]
- apiGroups: [""]
  resources: [events]
  verbs: [create, patch]
- apiGroups: [coordination.k8s.io]
  resources: [leases]
  verbs: [get, list, watch]
# Additionally, a namespaced Role in the controller's namespace (leader election):
- apiGroups: [coordination.k8s.io]
  resources: [leases]
  verbs: [get, create, update]
```

## Build & run

```sh
make test          # unit tests (fake Scaleway API + fake clientsets)
make compile       # local binary
make docker-build  # container image

# run out-of-cluster against a live cluster:
./scw-l2announce-lb-controller \
  -kubeconfig "$KUBECONFIG" -leader-elect=false \
  -private-network-id <pn-uuid>
```

## Deploy

A generic Helm chart lives in
[`charts/scw-l2announce-lb-controller`](charts/scw-l2announce-lb-controller).
On a stock Kapsule cluster it also bootstraps Cilium L2 announcements
(announcement policy, `enable-l2-announcements` CiliumNodeConfig, cilium lease
RBAC). Available versions and their values documentation are listed on the
[GitHub Releases page](https://github.com/iliaditalia/scw-l2announce-lb-controller/releases):

```sh
helm install l2lb oci://ghcr.io/iliaditalia/charts/scw-l2announce-lb-controller \
  --version <X.Y.Z> \
  --namespace scw-l2lb --create-namespace \
  --set scaleway.existingSecret=<secret-with-SCW_-vars> \
  --set scaleway.pnID=<private-network-uuid>
```

(For development, install from `./charts/scw-l2announce-lb-controller` with an
explicit `image.tag` instead.) Releases are cut by pushing a `vX.Y.Z` git tag:
CI publishes the multi-arch image `ghcr.io/iliaditalia/scw-l2announce-lb-controller:X.Y.Z`,
the chart at the same version, and a GitHub Release whose description carries
the values documentation generated from that version's `values.schema.json`;
every push to `main` additionally publishes the image tagged with the commit
SHA and `latest`.
