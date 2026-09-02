# Example Ansible deployment

This directory contains an **example** deployment for Kula. It is a starting
point, not a universal production configuration: review the inventory, role
defaults, configuration template, SSH access, and privilege-escalation settings
for your environment before running it.

The inventory is deliberately not included because it is specific to each
environment and may contain sensitive connection details. You must create a
file named `hosts` in this directory before deploying. The file is ignored by
Git.

## What the playbook does

`kula.yaml` targets every machine in the inventory (`hosts: all`) and applies
the `kula` role. On each supported target, the role:

1. Copies and installs `kula.deb` on Debian and Ubuntu systems, or `kula.rpm` on
   Red Hat-family systems (Red Hat, Fedora, CentOS, Rocky Linux, AlmaLinux, and
   Amazon Linux).
2. Renders `roles/kula/templates/config.yaml.j2` as
   `/etc/kula/config.yaml`, owned by the `kula` user and group with mode `0600`.
3. Enables the `kula` systemd service and restarts it so the deployed
   configuration takes effect.

The helper script `populate_files.sh` downloads the latest amd64/x86_64 Debian
and RPM release packages, verifies their SHA-256 checksums, and places them in
`roles/kula/files/`. Package download is not part of the playbook itself.

## Requirements

- Ansible installed on the control machine.
- SSH access from the control machine to every target.
- Permission to become root on each target for package installation,
  configuration, and systemd service management.
- A supported systemd-based Debian, Ubuntu, or Red Hat-family x86_64 target.
- `curl` or `wget`, plus `sha256sum`, when using `populate_files.sh`.

## Create the inventory

Create `addons/ansible/hosts` and list the machines that should receive Kula.
For example:

```ini
[kula_servers]
server-one ansible_host=192.0.2.10 ansible_user=deploy
server-two ansible_host=192.0.2.11 ansible_user=deploy
```

Replace the example addresses and SSH user with real values. Configure SSH
keys and Ansible privilege escalation as required by your environment. Avoid
putting plaintext credentials in the inventory; use Ansible Vault or another
secret-management mechanism when credentials are necessary.

Check connectivity before deployment:

```bash
cd addons/ansible
ansible -i hosts all -m ping
```

## Deploy

From the repository root:

```bash
cd addons/ansible
./populate_files.sh
./deploy.sh
```

`deploy.sh` stops before making changes if `hosts` is missing, is not a readable
regular file, is empty, cannot be parsed with the playbook, or provides no hosts
to the playbook's `all` group.

The main role defaults are in `roles/kula/defaults/main.yaml`:

```yaml
kula_show_system_info: true
kula_show_version: true
kula_port: 27960
```

Override these through inventory variables, or adapt the configuration template
for settings that are not exposed as role variables. Review planned changes
with Ansible's check mode when appropriate:

```bash
ansible-playbook -i hosts kula.yaml --check --diff
```
