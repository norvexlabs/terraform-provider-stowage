# Docker on EC2

A Docker host on EC2 (Debian, Docker installed by cloud-init) and a Stowage product installed on
it over SSH by `stowage_docker_install`. The host's SSH key is made here (`tls_private_key`) and
given to the instance by cloud-init, so every connection pins it from the first: the install's
`known_hosts_line`, and the `remote-exec` that waits for cloud-init (`host_key`). The key's private
half is in state: encrypt state. The installer runs on the host (`docker run`), never on this
machine.

**Preview**: not applied on AWS yet (`tofu test` at `command = plan` passes under mocked providers).

| Resource | Import ID |
|---|---|
| `aws_instance.docker` | the instance id |
| `aws_security_group.docker` | the group id |
| `stowage_docker_install.app` | `<address>:/home/admin/<name>/<short_name>-stow` |
