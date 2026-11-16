# What a release runs, read without managing it, on the provider's cluster.
data "stowage_kubernetes_install" "notes" {
  installer = "registry.example.com/notes/notes-installer@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
  namespace = "notes"
}
