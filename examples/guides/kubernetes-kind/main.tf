# A Kubernetes cluster (kind, on this machine's Docker: kind's own need, not the Stowage
# provider's), its prerequisites by helm_release (an ingress controller), and a Stowage product's
# helm release by stowage_kubernetes_install. The installer runs on this machine — its image's
# entry for this machine's platform, read from the registry, with helm and kubectl on PATH — never
# in Docker. Any other cluster takes kind_cluster's place: give both providers its credentials.

resource "kind_cluster" "this" {
  name           = var.name
  wait_for_ready = true

  kind_config {
    kind        = "Cluster"
    api_version = "kind.x-k8s.io/v1alpha4"

    node {
      role = "control-plane"
      labels = {
        "ingress-ready" = "true"
      }
      extra_port_mappings {
        container_port = 80
        host_port      = 8080
      }
      extra_port_mappings {
        container_port = 443
        host_port      = 8443
      }
    }
  }
}

# The release's prerequisite: the ingress controller its chart's ingress names (className nginx).
resource "helm_release" "ingress_nginx" {
  name             = "ingress-nginx"
  repository       = "https://kubernetes.github.io/ingress-nginx"
  chart            = "ingress-nginx"
  version          = "4.13.3"
  namespace        = "ingress-nginx"
  create_namespace = true
  wait             = true

  set = [
    { name = "controller.hostPort.enabled", value = "true" },
    { name = "controller.service.type", value = "ClusterIP" },
    { name = "controller.nodeSelector.ingress-ready", value = "true", type = "string" },
  ]
}

resource "stowage_kubernetes_install" "app" {
  installer = var.installer
  namespace = var.namespace

  inputs                   = var.inputs
  secret_inputs_wo         = var.secret_inputs
  secret_inputs_wo_version = 1
  values                   = yamlencode(var.values)

  deletion_protection = var.deletion_protection

  depends_on = [helm_release.ingress_nginx]
}

output "version" {
  value = stowage_kubernetes_install.app.version
}
