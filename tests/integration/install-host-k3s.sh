#!/usr/bin/env bash
# One-time, root-only installation for the first local smoke environment.
# Review before running: it changes firewalld and installs a systemd service.
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  printf 'Run this script as root with sudo.\n' >&2
  exit 1
fi
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
installer="$repo_root/.local/get-k3s.sh"
expected='e5cc3b3d9dfc1662c2d9be6da5abc9a4cd317d6abc3a5ffc02e3dd3248207fee'
actual=$(sha256sum "$installer" | cut -d ' ' -f 1)
if [ "$actual" != "$expected" ]; then
  printf 'K3s installer checksum differs; stop and review the downloaded script.\n' >&2
  exit 1
fi
if command -v k3s >/dev/null 2>&1; then
  printf 'K3s is already installed; inspect the existing installation first.\n' >&2
  exit 1
fi
if ss -ltnH | awk '{print $4}' | grep -Eq '(^|:)744$'; then
  printf 'TCP port 744 is already in use.\n' >&2
  exit 1
fi
if [ "$(firewall-cmd --state)" != running ]; then
  printf 'firewalld is not running; review network exposure before installing K3s.\n' >&2
  exit 1
fi

# FedoraWorkstation currently allows all TCP ports >=1025 from the LAN. Use
# 744 for the local API and trust only K3s pod/service CIDRs for cluster traffic.
firewall-cmd --permanent --zone=trusted --add-source=10.42.0.0/16
firewall-cmd --permanent --zone=trusted --add-source=10.43.0.0/16
firewall-cmd --reload

env INSTALL_K3S_VERSION='v1.34.12+k3s1' sh "$installer" server \
  --https-listen-port=744 \
  --disable=traefik \
  --disable=servicelb

printf 'K3s installed. Check firewall zone, node readiness, and listener address before applying manifests.\n'
