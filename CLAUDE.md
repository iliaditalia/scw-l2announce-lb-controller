# CLAUDE.md

## What this is

A **tech demo** (not production-recommended — warning at the top of README.md
stays) published **publicly on GitHub**. Single-purpose Kubernetes operator for
Scaleway Kapsule: keeps a Scaleway IPAM IP's custom-resource MAC attachment
pointed at the node holding the Cilium L2-announcement lease, so
Cilium-announced LoadBalancer VIPs are routable through the VPC gateway
(peering/interlink), not just on the local L2 segment. IPv4 only. README.md
has the full why/how; its RBAC section is the source of truth the Helm chart
must stay in sync with.

## Layout

- `cmd/scw-l2announce-lb-controller/main.go` — flags, Scaleway client checks,
  metrics/healthz server (started before and regardless of leader election),
  leader election.
- `l2lb/` — everything else, one package: `controller.go` (informers, queue,
  opt-in logic), `reconcile.go` (`syncService`, the ordered idempotent steps),
  `scaleway.go` (IPAM/Instance API use),
  `patcher.go` (strategic-merge Service patches), `logger.go` (SDK→klog
  bridge), `metrics.go`, `version.go` (ldflags).
- `charts/scw-l2announce-lb-controller/` — generic Helm chart (see below).
- Tests: fakes in `l2lb/fakes_test.go` (in-memory Scaleway APIs recording
  calls) + k8s fake clientsets; assertions center on **which mutations
  happened** (`fakeIPAM.mutations()`).

## Design decisions — do not regress

- **Leader election stays.** Run ≥2 replicas spread across nodes. The failure
  it covers is correlated: the dying node may be both lease holder and
  controller host. A DaemonSet was rejected: failover is bounded by lease
  timings (15s), extra standbys only add cluster-wide watch load.
- **Nodes are fetched on demand** (`Nodes().Get` per reconcile), not cached in
  an informer — reconciles are rare. RBAC for nodes is `get` only.
- **Single reconcile worker** serializes all Scaleway mutations.
- **Never silently re-book** an IP whose ID is in the
  `k8s.iliad.it/scw-ipam-ip-id` annotation — it may be user-provided. Crash
  recovery between BookIP and the annotation patch uses adoption by the
  `service-uid=` tag.
- **Only release IPs tagged `managed-by=scw-l2announce-lb-controller`**;
  user-provided IPs are detached, never released. Never steal an IP attached
  to a non-custom resource.
- **Mutate Scaleway only on an actual difference** (drives the
  `scw_l2lb_divergence` metric, which is the alerting signal).
- **VIPs are published via `spec.externalIPs`, never Cilium LB-IPAM pools**:
  LB-IPAM runs in the cilium-operator and ignores
  `loadBalancerClass: io.cilium/l2-announcer` unless the *operator* has
  `enable-l2-announcements`, which on Kapsule is impossible (the
  CiliumNodeConfig only reaches agents; the operator's ConfigMap is
  Scaleway-managed). The agent-side L2 announcer picks externalIPs up
  directly (chart policy sets `externalIPs: true`). Only the controller-added
  entry is managed; user entries in `externalIPs` are preserved. The
  `ipam.k8s.iliad.it/pool` label is legacy — stripped, never written. The
  controller also mirrors the VIP into `status.loadBalancer.ingress`
  (subresource patch, `services/status` RBAC) — nothing else populates it,
  and ArgoCD reports LoadBalancer Services as Progressing until it is set.
- Use non-deprecated client-go APIs: typed workqueue
  (`TypedRateLimitingInterface[string]`), `cache.NewInformerWithOptions`.
- Lease names are matched by computing `leaseNameFor(svc)`, never by parsing
  the lease name (namespace and name may both contain hyphens).

## Build / test

```sh
make test      # go test -race, fake-based; always run after changes
make compile   # binary; also run gofmt -l . and go vet ./...
```

Known macOS quirks (pre-existing, harmless): `BUILD_DATE ?= $(shell date -Is)`
fails on BSD date; version stamps read `git rev-parse HEAD`.

## CI / releases

`.github/workflows/ci.yml`: `test` (Go + gofmt + vet + the chart validation
battery below) on every push/PR; on pushes, `image` builds multi-arch
(amd64+arm64) to `ghcr.io/iliaditalia/scw-l2announce-lb-controller` — bare
commit SHA + `latest` on main, semver on `v*` tags; on `v*` tags, `chart`
packages with `version` = `appVersion` = the tag (so the chart's default
`image.tag` matches the released image) and pushes to
`oci://ghcr.io/iliaditalia/charts` (the `charts/` prefix avoids colliding
with the image package). The tag job also creates a GitHub Release: body =
`.github/release-notes-preamble.md` (`${VERSION}` substituted) + values docs
generated from `values.schema.json` by a version-pinned
`pipx run jsonschema-markdown` — which runs in the separate unprivileged
`docs` job (`permissions: {}`, `persist-credentials: false`) because it
executes third-party PyPI code; the privileged `chart` job only downloads the
artifact and attaches the chart .tgz. The Releases page is the versioned
chart-docs listing, since GHCR can't render a per-version README. Never move
third-party code execution into a job holding write tokens. The Dockerfile builds `FROM
--platform=$BUILDPLATFORM` and cross-compiles via declared
`ARG TARGETOS TARGETARCH` — keep it that way, qemu-compiled Go is slow.

## Helm chart rules

- `values.schema.json` is strict and follows the Iliad schema conventions:
  draft-07, a `definitions` block of **CamelCase-named defs** referenced via
  `$ref` (no inline nested objects unless tiny and not reused),
  `additionalProperties: false` everywhere except Kubernetes passthrough
  shapes (selectors, affinity, tolerations, securityContexts) and string
  maps, `description` on every property, loose top-level `global`.
  Keep values.yaml, the schema, and the chart README in sync.
- **`leaderElect` is not a value**: derived everywhere as
  `gt (int .Values.replicaCount) 1` (flag, election Role, NOTES).
- **Credentials are never chart values**: `scaleway.existingSecret` (required)
  carries the `SCW_*` env vars; only the non-secret `scaleway.projectID`/
  `region` may be values (rendered into a ConfigMap that takes precedence
  over the Secret). `scaleway.pnID` lives under `scaleway`, not top-level.
- Every template body starts with a `---` document marker **inside** its
  `{{- if }}` gate.
- The chart also bootstraps Cilium L2 announcement for stock Kapsule
  (`CiliumL2AnnouncementPolicy` on `^ens[0-9]+$`, `cilium.io/v2
  CiliumNodeConfig` setting `enable-l2-announcements`, and the
  `cilium-l2announce-leases` Role/RoleBinding for SA `cilium`) — gated by
  `cilium.l2AnnouncementPolicy.enabled` / `cilium.kapsuleBootstrap.enabled`,
  both default true. Agents read the CiliumNodeConfig only at pod start
  (NOTES.txt tells users to restart the cilium DaemonSet once).

Validate chart changes:

```sh
helm lint charts/scw-l2announce-lb-controller
helm template t charts/scw-l2announce-lb-controller \
  --set scaleway.pnID=pn-x --set scaleway.existingSecret=s   # must render
helm template t charts/scw-l2announce-lb-controller          # must fail (required values)
helm template t charts/scw-l2announce-lb-controller \
  --set scaleway.pnID=pn-x --set scaleway.existingSecret=s \
  --set typoKey=1                                            # must fail (strict schema)
```
