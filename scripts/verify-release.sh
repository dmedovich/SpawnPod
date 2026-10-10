#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
release_version=${1:-$(cat VERSION)}
release_root="dist/$release_version"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

(cd "$release_root" && sha256sum -c SHA256SUMS)
for arch in amd64 arm64; do
  package="spawnpod_${release_version}_linux_${arch}"
  archive="$release_root/$package.tar.gz"
  [[ -f "$archive" ]] || { printf 'Missing %s\n' "$archive" >&2; exit 1; }
  tar -xzf "$archive" -C "$work"
  stage="$work/$package"
  (cd "$stage" && sha256sum -c BINARY_SHA256SUMS)
  python3 - "$stage/release.json" "$release_version" "$arch" "$(git rev-parse HEAD)" "${SPAWNPOD_ALLOW_DIRTY:-0}" <<'PY'
import json, sys
with open(sys.argv[1]) as file:
    release = json.load(file)
if (release.get('version'), release.get('platform'), release.get('revision'), release.get('dirty')) != (sys.argv[2], 'linux/' + sys.argv[3], sys.argv[4], sys.argv[5] == '1'):
    raise SystemExit('Archive release.json does not match tag, architecture and commit')
PY
  for component in spawnpod spawnpod-agent spawnpod-source-proxy; do
    machine=$(readelf -h "$stage/$component" | sed -n 's/^[[:space:]]*Machine:[[:space:]]*//p')
    case "$arch:$machine" in
      amd64:*X86-64*|arm64:*AArch64*) ;;
      *) printf 'Wrong ELF architecture: %s (%s)\n' "$stage/$component" "$machine" >&2; exit 1 ;;
    esac
    if [[ "$(go env GOARCH)" == "$arch" ]]; then
      [[ "$("$stage/$component" version)" == "$release_version" ]]
    fi
  done
done
printf 'Verified release archives for amd64 and arm64.\n'
