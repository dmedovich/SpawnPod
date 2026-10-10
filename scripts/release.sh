#!/usr/bin/env bash
# Build locally; never publishes or deploys. Run checks before invoking.
set -euo pipefail
cd "$(dirname "$0")/.."
release_version=${1:-$(cat VERSION)}
if [[ ! "$release_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]; then
  printf 'Invalid release version: %s\n' "$release_version" >&2; exit 1
fi
if [[ "$release_version" != "$(cat VERSION)" ]]; then
  printf 'Release version must match VERSION.\n' >&2; exit 1
fi
release_dirty=false
if [[ -n "$(git status --porcelain)" ]]; then
  release_dirty=true
  if [[ ${SPAWNPOD_ALLOW_DIRTY:-0} != 1 ]]; then
    printf 'Commit the release sources first, or set SPAWNPOD_ALLOW_DIRTY=1 for a local preview.\n' >&2; exit 1
  fi
fi
release_revision=$(git rev-parse HEAD)
release_epoch=$(git show -s --format=%ct HEAD)
release_root="dist/$release_version"
release_package="spawnpod_${release_version}_linux_amd64"
release_stage="$release_root/$release_package"
mkdir -p "$release_stage"
for component in spawnpod spawnpod-agent spawnpod-source-proxy; do
  env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath \
    -ldflags "-s -w -X main.version=$release_version" \
    -o "$release_stage/$component" "./cmd/$component"
done
cp LICENSE README.md VERSION go.mod go.sum "$release_stage/"
cp -R docs "$release_stage/"
mkdir -p "$release_stage/deploy" "$release_stage/third-party"
cp deploy/security.yaml deploy/network-default-deny.yaml deploy/config.example.yaml "$release_stage/deploy/"
cp -R deploy/release "$release_stage/deploy/"
install -m 0644 "$(go env GOROOT)/LICENSE" "$release_stage/third-party/Go-LICENSE"
cp internal/adapter/forgejo/proto/LICENSE "$release_stage/third-party/forgejo-plugin-LICENSE"
go list -m -json all > "$release_root/modules.json"
python3 - "$release_root" "$release_stage" "$release_version" "$release_revision" "$release_dirty" <<'PY'
import json, pathlib, sys, shutil
root,stage=map(pathlib.Path,sys.argv[1:3])
raw=(root/'modules.json').read_text();decoder=json.JSONDecoder(); modules=[]
while raw.strip():
    raw=raw.lstrip();m,end=decoder.raw_decode(raw);raw=raw[end:];modules.append(m)
inventory=[]
for m in modules:
    item={k:m[k] for k in ('Path','Version','Sum') if k in m}
    directory=pathlib.Path(m.get('Dir','/nonexistent'))
    licenses=[p for p in directory.iterdir() if p.is_file() and p.name.lower().startswith(('license','copying','notice'))] if directory.is_dir() else []
    if not m.get('Main'):
        target=stage/'third-party'/m['Path'].replace('/','_');target.mkdir(exist_ok=True)
        for license_file in licenses:
            pending=target/(license_file.name+'.tmp')
            shutil.copyfile(license_file,pending);pending.chmod(0o644)
            pending.replace(target/license_file.name)
        item['license_files']=[p.name for p in licenses]
    inventory.append(item)
(stage/'dependencies.json').write_text(json.dumps(inventory,indent=2)+'\n')
(stage/'release.json').write_text(json.dumps({'version':sys.argv[3],'revision':sys.argv[4],'dirty':sys.argv[5]=='true','platform':'linux/amd64','runner':'v13.1.0','stage':'release-candidate'},indent=2)+'\n')
PY
(cd "$release_stage" && sha256sum spawnpod spawnpod-agent spawnpod-source-proxy > BINARY_SHA256SUMS)
tar --sort=name --mtime="@$release_epoch" --owner=0 --group=0 --numeric-owner \
  -czf "$release_root/$release_package.tar.gz" -C "$release_root" "$release_package"
(cd "$release_root" && sha256sum "$release_package.tar.gz" > SHA256SUMS)
printf 'Prepared %s (%s, dirty=%s). No upload performed.\n' "$release_root" "$release_revision" "$release_dirty"
