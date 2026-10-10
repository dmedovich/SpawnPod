#!/usr/bin/env bash
# Build locally; never publishes or deploys. Run checks before invoking.
set -euo pipefail
cd "$(dirname "$0")/.."
release_version=${1:-$(cat VERSION)}
release_arch=${2:-amd64}
if [[ "$release_arch" != amd64 && "$release_arch" != arm64 ]]; then
  printf 'Unsupported release architecture: %s\n' "$release_arch" >&2; exit 1
fi
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
release_package="spawnpod_${release_version}_linux_${release_arch}"
mkdir -p "$release_root"
release_work=$(mktemp -d "$release_root/.build.XXXXXX")
trap 'rm -rf "$release_work"' EXIT
release_stage="$release_work/$release_package"
mkdir -p "$release_stage"
for component in spawnpod spawnpod-agent spawnpod-source-proxy; do
  env CGO_ENABLED=0 GOOS=linux GOARCH="$release_arch" go build -buildvcs=false -trimpath \
    -ldflags "-s -w -X main.version=$release_version" \
    -o "$release_stage/$component" "./cmd/$component"
done
cp LICENSE README.md VERSION go.mod go.sum "$release_stage/"
cp -R docs "$release_stage/"
mkdir -p "$release_stage/deploy" "$release_stage/third-party"
cp deploy/security.yaml deploy/network-default-deny.yaml deploy/config.example.yaml "$release_stage/deploy/"
cp -R deploy/release "$release_stage/deploy/"
go_license=''
for candidate in "$(go env GOROOT)/LICENSE" /usr/share/licenses/golang/LICENSE /usr/share/licenses/golang-src/LICENSE; do
  if [[ -f "$candidate" ]]; then go_license=$candidate; break; fi
done
if [[ -z "$go_license" ]]; then
  printf 'Go LICENSE not found in GOROOT or Fedora license directories.\n' >&2; exit 1
fi
install -m 0644 "$go_license" "$release_stage/third-party/Go-LICENSE"
cp internal/adapter/forgejo/proto/LICENSE "$release_stage/third-party/forgejo-plugin-LICENSE"
go list -m -json all > "$release_root/modules.json"
go list -deps -f '{{if .Module}}{{.Module.Path}}{{end}}' ./cmd/... > "$release_root/linked-modules.txt"
python3 - "$release_root" "$release_stage" "$release_version" "$release_revision" "$release_dirty" "$release_arch" <<'PY'
import json, pathlib, sys, shutil
root,stage=map(pathlib.Path,sys.argv[1:3])
raw=(root/'modules.json').read_text();decoder=json.JSONDecoder(); modules=[]
while raw.strip():
    raw=raw.lstrip();m,end=decoder.raw_decode(raw);raw=raw[end:];modules.append(m)
inventory=[]
linked=set((root/'linked-modules.txt').read_text().splitlines())
for m in modules:
    item={k:m[k] for k in ('Path','Version','Sum') if k in m}
    item['linked']=m['Path'] in linked
    directory=pathlib.Path(m.get('Dir','/nonexistent'))
    licenses=[p for p in directory.iterdir() if p.is_file() and p.name.lower().startswith(('license','copying','notice'))] if directory.is_dir() else []
    if not m.get('Main') and item['linked']:
        if not licenses:
            raise SystemExit('Missing license for linked module: '+m['Path'])
        target=stage/'third-party'/m['Path'].replace('/','_');target.mkdir(exist_ok=True)
        for license_file in licenses:
            pending=target/(license_file.name+'.tmp')
            shutil.copyfile(license_file,pending);pending.chmod(0o644)
            pending.replace(target/license_file.name)
        item['license_files']=[p.name for p in licenses]
    inventory.append(item)
(stage/'dependencies.json').write_text(json.dumps(inventory,indent=2)+'\n')
(stage/'release.json').write_text(json.dumps({'version':sys.argv[3],'revision':sys.argv[4],'dirty':sys.argv[5]=='true','platform':'linux/'+sys.argv[6],'runner':'v13.1.0','stage':'release-candidate' if '-' in sys.argv[3] else 'stable'},indent=2)+'\n')
PY
(cd "$release_stage" && sha256sum spawnpod spawnpod-agent spawnpod-source-proxy > BINARY_SHA256SUMS)
tar --sort=name --mtime="@$release_epoch" --owner=0 --group=0 --numeric-owner \
  -czf "$release_root/$release_package.tar.gz" -C "$release_work" "$release_package"
# Replace the complete staging directory; deleted source files must not survive
# a repeated build in the next archive or OCI image.
rm -rf "$release_root/$release_package"
mv "$release_stage" "$release_root/$release_package"
(cd "$release_root" && sha256sum spawnpod_"$release_version"_linux_*.tar.gz > SHA256SUMS)
printf 'Prepared %s (%s, dirty=%s). No upload performed.\n' "$release_root" "$release_revision" "$release_dirty"
