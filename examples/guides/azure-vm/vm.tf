# One Azure VM of the release, made from the release's registered image (cloud_image), or image_id with the cloud-config as
# its custom data.

# The VM's public address: the one public_ip_name names in the resource group (reserved
# beforehand, kept by `vm destroy`), else one made for it.
resource "azurerm_public_ip" "vm" {
  count               = var.public_ip_name == "" ? 1 : 0
  name                = "${var.name}-ip"
  location            = var.location
  resource_group_name = var.resource_group
  allocation_method   = "Static"
  sku                 = "Standard"
}


data "azurerm_public_ip" "given" {
  count               = var.public_ip_name == "" ? 0 : 1
  name                = var.public_ip_name
  resource_group_name = var.resource_group
}

locals {
  public_ip_id = var.public_ip_name == "" ? azurerm_public_ip.vm[0].id : data.azurerm_public_ip.given[0].id
  public_address = var.public_ip_name == "" ? azurerm_public_ip.vm[0].ip_address : data.azurerm_public_ip.given[0].ip_address
}

resource "azurerm_network_interface" "vm" {
  name                = "${var.name}-nic"
  location            = var.location
  resource_group_name = var.resource_group
  # The NIC's own hardware path (ConnectX, MANA), whose drivers the image carries: the size must
  # offer it.
  accelerated_networking_enabled = var.accelerated_networking

  ip_configuration {
    name                          = "primary"
    subnet_id                     = var.subnet_id
    private_ip_address_allocation = "Dynamic"
    public_ip_address_id          = local.public_ip_id
  }
}

# A network security group of what the VM's own firewall opens (stowage_vm_seed's firewall): SSH
# from its sources, each port from its own.
locals {
  rules = concat([for s in local.seed.firewall.ssh_sources : { protocol = "Tcp", port = 22, source = s }],
  flatten([for p in local.seed.firewall.open_ports : [for s in p.sources : { protocol = title(p.protocol), port = p.port, source = s }]]))
}

resource "azurerm_network_security_group" "vm" {
  name                = "${var.name}-nsg"
  location            = var.location
  resource_group_name = var.resource_group

  dynamic "security_rule" {
    for_each = { for i, r in local.rules : tostring(i) => r }
    content {
      name                       = "stowage-${security_rule.key}"
      priority                   = 1000 + tonumber(security_rule.key)
      direction                  = "Inbound"
      access                     = "Allow"
      protocol                   = security_rule.value.protocol
      source_port_range          = "*"
      destination_port_range     = tostring(security_rule.value.port)
      source_address_prefix      = security_rule.value.source
      destination_address_prefix = "*"
    }
  }
}

resource "azurerm_network_interface_security_group_association" "vm" {
  network_interface_id      = azurerm_network_interface.vm.id
  network_security_group_id = azurerm_network_security_group.vm.id
}

resource "azurerm_linux_virtual_machine" "vm" {
  name                       = var.name
  location                   = var.location
  resource_group_name        = var.resource_group
  size                       = var.vm_size
  admin_username             = var.admin_user
  network_interface_ids      = [azurerm_network_interface.vm.id]
  source_image_id            = local.image_id
  custom_data                = base64encode(local.seed.user_data)
  provision_vm_agent         = false
  allow_extension_operations = false

  admin_ssh_key {
    username   = var.admin_user
    public_key = file(pathexpand("${var.ssh_key_file}.pub"))
  }

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "StandardSSD_LRS"
    disk_size_gb         = local.disk_gib
  }

  lifecycle {
    # A later release reaches the VM as an upgrade, never as a replacement.
    ignore_changes = [source_image_id, custom_data]
  }
}

# The image `ship --disks` registered for this release's disk (cloud_image), else image_id.
data "azurerm_image" "release" {
  count               = var.image_id == "" ? 1 : 0
  name                = local.seed.disk["azure"].cloud_image
  resource_group_name = var.resource_group
}

locals {
  image_id = var.image_id != "" ? var.image_id : data.azurerm_image.release[0].id
}

locals {
  # The address SSH reaches the VM at: vm_address, else its public one.
  address = coalesce(var.vm_address, local.public_address)
}
