# The issuer proves site B took its kit: `kit check second-site site-b`, tried until it answers.
resource "stowage_kit_check" "site_b" {
  handoff  = stowage_kit.site_b.handoff
  instance = stowage_kit.site_b.instance
  triggers = { kit = stowage_kit.site_b.id }
  timeout  = "10m"

  kubernetes {
    installer = stowage_kubernetes_install.issuer.installer
    namespace = stowage_kubernetes_install.issuer.namespace
  }

  lifecycle {
    replace_triggered_by = [stowage_kubernetes_install.site_b] # a replaced target keeps its id
  }
}
