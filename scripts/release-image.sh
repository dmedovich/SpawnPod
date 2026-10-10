#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
release_version=${1:-$(cat VERSION)}
if [[ ! "$release_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]; then exit 1; fi
release_root="dist/$release_version"
release_package="spawnpod_${release_version}_linux_amd64"
release_stage="$release_root/$release_package"
[[ -f "$release_stage/release.json" ]] || { printf 'Build the binary package first.\n' >&2; exit 1; }
python3 - "$release_stage/release.json" "$(git rev-parse HEAD)" "$release_version" <<'PYCODE'
import json,sys
with open(sys.argv[1]) as f:r=json.load(f)
if r['revision']!=sys.argv[2] or r['version']!=sys.argv[3]:raise SystemExit('Rebuild binary package from the current revision/version first.')
PYCODE
(cd "$release_stage" && sha256sum -c BINARY_SHA256SUMS)
release_epoch=$(git show -s --format=%ct HEAD)
podman build --timestamp "$release_epoch" --build-arg "VERSION=$release_version" --build-arg "REVISION=$(git rev-parse HEAD)" \
  -f deploy/release/runtime.Dockerfile -t "localhost/spawnpod:$release_version" "$release_stage"
podman save --format oci-archive --output "$release_root/$release_package.oci.tar" "localhost/spawnpod:$release_version"
python3 - "$release_root/$release_package.oci.tar" "$release_root/IMAGE_DIGEST" <<'PY'
import json,tarfile,pathlib,sys
with tarfile.open(sys.argv[1]) as archive:
    index=json.load(archive.extractfile('index.json'))
manifest=index['manifests']
if len(manifest)!=1:raise SystemExit('expected one image manifest')
pathlib.Path(sys.argv[2]).write_text('localhost/spawnpod@'+manifest[0]['digest']+'\n')
PY
(cd "$release_root" && sha256sum "$release_package.tar.gz" "$release_package.oci.tar" IMAGE_DIGEST > SHA256SUMS)
printf 'Prepared image and complete SHA256SUMS under %s.\n' "$release_root"
