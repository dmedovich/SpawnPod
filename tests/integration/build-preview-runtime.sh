#!/usr/bin/env bash
# Build a static, non-published runtime preview with the release Dockerfile.
set -euo pipefail
cd "$(dirname "$0")/../.."
preview_directory=${1:-.local/rc7-validation/runtime-static}
preview_image=${2:-localhost/spawnpod:rc7-static-preview}
mkdir -p "$preview_directory"
for component in spawnpod spawnpod-agent spawnpod-source-proxy; do
  env CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -trimpath \
    -ldflags '-s -w -X main.version=dev' \
    -o "$preview_directory/$component" "./cmd/$component"
  chmod 755 "$preview_directory/$component"
  if readelf -l "$preview_directory/$component" | rg -q 'Requesting program interpreter'; then
    printf 'Preview runtime must be static: %s\n' "$component" >&2
    exit 1
  fi
done
(cd "$preview_directory" && sha256sum spawnpod spawnpod-agent spawnpod-source-proxy > BINARY_SHA256SUMS)
preview_revision=$(git rev-parse HEAD)
if [[ -n "$(git status --porcelain)" ]]; then preview_revision="$preview_revision-dirty"; fi
podman build --network=none --build-arg VERSION=dev --build-arg "REVISION=$preview_revision" \
  -f deploy/release/runtime.Dockerfile -t "$preview_image" "$preview_directory"
printf 'Built local preview %s; no publication performed.\n' "$preview_image"
