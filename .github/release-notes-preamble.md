# scw-l2announce-lb-controller chart ${VERSION}

> [!WARNING]
> This is a tech demo. It works, but it is not battle-tested and is not
> recommended for production use.

Makes Cilium L2-announced LoadBalancer VIPs on Scaleway reachable through the
VPC gateway. Ships the controller image
`ghcr.io/iliaditalia/scw-l2announce-lb-controller:${VERSION}` and, by default,
the Cilium L2-announcement bootstrap for stock Kapsule clusters.

```sh
helm install l2lb oci://ghcr.io/iliaditalia/charts/scw-l2announce-lb-controller \
  --version ${VERSION} \
  --namespace scw-l2lb --create-namespace \
  --set scaleway.existingSecret=<secret-with-SCW_-vars> \
  --set scaleway.pnID=<private-network-uuid>
```

Full documentation:
[chart README](https://github.com/iliaditalia/scw-l2announce-lb-controller/blob/v${VERSION}/charts/scw-l2announce-lb-controller/README.md)
(pinned to this version).

The values reference below is generated from this version's
`values.schema.json`.

