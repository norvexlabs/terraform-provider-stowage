# Kubernetes (kind)

A kind cluster on this machine, its prerequisite (ingress-nginx) by `helm_release`, and a Stowage
product's helm release by `stowage_kubernetes_install`. kind runs on this machine's Docker: that is
kind's need, not the Stowage provider's, which runs the installer natively (its image's entry for
this machine's platform) with `helm` (3.14 or later) and `kubectl` on `PATH`. Any other cluster
takes `kind_cluster`'s place: give the `helm` and `stowage` providers its credentials.

**Preview**: not applied yet (`tofu test` at `command = plan` passes under mocked providers).

| Resource | Import ID |
|---|---|
| `kind_cluster.this` | not importable |
| `helm_release.ingress_nginx` | `ingress-nginx/ingress-nginx` |
| `stowage_kubernetes_install.app` | `<namespace>/<release>` |
