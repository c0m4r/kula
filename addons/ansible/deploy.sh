#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
readonly INVENTORY_FILE="${SCRIPT_DIR}/hosts"
readonly PLAYBOOK_FILE="${SCRIPT_DIR}/kula.yaml"

die() {
    printf 'Error: %s\n' "$*" >&2
    exit 1
}

[[ -e "$INVENTORY_FILE" ]] || \
    die "inventory file not found: $INVENTORY_FILE (create it before deploying; see README.md)"
[[ -f "$INVENTORY_FILE" ]] || \
    die "inventory path is not a regular file: $INVENTORY_FILE"
[[ -r "$INVENTORY_FILE" ]] || \
    die "inventory file is not readable: $INVENTORY_FILE"
[[ -s "$INVENTORY_FILE" ]] || \
    die "inventory file is empty: $INVENTORY_FILE"

command -v ansible-playbook >/dev/null 2>&1 || \
    die "ansible-playbook is not installed or is not in PATH"

cd "$SCRIPT_DIR"

if ! inventory_check=$(
    ansible-playbook -i "$INVENTORY_FILE" "$PLAYBOOK_FILE" --list-hosts 2>&1
); then
    printf '%s\n' "$inventory_check" >&2
    die "Ansible could not validate the inventory and playbook"
fi

if [[ "$inventory_check" =~ hosts[[:space:]]+\(([0-9]+)\): ]]; then
    host_count=${BASH_REMATCH[1]}
else
    printf '%s\n' "$inventory_check" >&2
    die "could not determine the number of hosts in the inventory"
fi

if (( host_count == 0 )); then
    die "inventory contains no hosts matched by the playbook's 'all' group"
fi

ansible-playbook -i "$INVENTORY_FILE" "$PLAYBOOK_FILE"
