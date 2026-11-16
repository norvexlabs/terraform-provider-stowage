# libvirtd and QEMU as Ubuntu LTS packages them, for make acc-libvirt: examples/guides/libvirt-vm
# applied against it (domain_type qemu: no KVM there), its socket bind-mounted out.
ARG BASE
FROM ${BASE}
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
	libvirt-daemon-system qemu-system-x86 qemu-utils ovmf dnsmasq-base iptables iproute2 && rm -rf /var/lib/apt/lists/*
COPY start.sh /start.sh
CMD ["/bin/sh", "/start.sh"]
