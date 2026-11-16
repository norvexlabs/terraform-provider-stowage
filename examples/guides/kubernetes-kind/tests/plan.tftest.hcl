# Plans the root under mocked providers (`tofu test` in this directory, the stowage provider
# installed): the release after its prerequisite, the chart's values given. Nothing runs.

mock_provider "kind" {}
mock_provider "helm" {}
mock_provider "stowage" {}

variables {
  installer = "registry.example.com/notes/notes-installer@sha256:0000000000000000000000000000000000000000000000000000000000000000"
}

run "release" {
  command = plan
  assert {
    condition     = stowage_kubernetes_install.app.namespace == "notes" && yamldecode(stowage_kubernetes_install.app.values).ingress.className == "nginx"
    error_message = "the release in its namespace, with the chart's values"
  }
}
