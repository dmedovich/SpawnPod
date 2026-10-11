#!/usr/bin/env bash
# Disposable GitHub-hosted VM only. Installs K3s and changes cluster admission.
set -euo pipefail
[[ ${GITHUB_ACTIONS:-} == true && ${RUNNER_ENVIRONMENT:-} == github-hosted ]] || {
  printf 'This bootstrap is restricted to disposable GitHub-hosted runners.\n' >&2
  exit 1
}
cd "$(dirname "$0")/../.."
repo_root=$PWD
evidence="$repo_root/.local/upgrade-ci"
mkdir -p "$evidence/runtime"
chmod 700 "$repo_root/.local" "$evidence"
umask 077
previous_version=v0.1.0-rc.4
k3s_version=v1.34.12+k3s1
runner_image=data.forgejo.org/forgejo/runner@sha256:96af81b1b0cd928f3f135fb8dc021d47cff868c151a4fd28af971e892990ee7a
forgejo_image=codeberg.org/forgejo/forgejo@sha256:91a5310c86934339e16bd06b6078aada836e3d8935b2d70f6598108cbfaed5d1

for component in spawnpod spawnpod-agent spawnpod-source-proxy; do
  CGO_ENABLED=0 go build -trimpath -o "$evidence/runtime/$component" "./cmd/$component"
done
podman build -f deploy/release/runtime.Dockerfile -t localhost/spawnpod:upgrade-ci "$evidence/runtime" > "$evidence/build.log" 2>&1
podman save --format oci-archive -o "$evidence/runtime.oci.tar" localhost/spawnpod:upgrade-ci
podman build -f tests/integration/upgrade-job.Dockerfile -t localhost/spawnpod-ci-job:upgrade-ci . >> "$evidence/build.log" 2>&1
podman save --format oci-archive -o "$evidence/job.oci.tar" localhost/spawnpod-ci-job:upgrade-ci

previous_package="spawnpod_${previous_version}_linux_amd64"
release_url="https://github.com/dmedovich/SpawnPod/releases/download/$previous_version"
for asset in "$previous_package.tar.gz" "$previous_package.oci.tar" SHA256SUMS IMAGE_DIGEST; do
  curl -fsSL --retry 3 "$release_url/$asset" -o "$evidence/$asset"
done
(cd "$evidence"; awk -v package="$previous_package" '$2 == package ".tar.gz" || $2 == package ".oci.tar" || $2 == "IMAGE_DIGEST" {print}' SHA256SUMS > previous-checksums; test "$(wc -l < previous-checksums)" = 3; sha256sum -c previous-checksums)
tar -xzf "$evidence/$previous_package.tar.gz" -C "$evidence"

k3s_url="https://github.com/k3s-io/k3s/releases/download/$k3s_version"
curl -fsSL --retry 3 "$k3s_url/k3s" -o "$evidence/k3s"
curl -fsSL --retry 3 "$k3s_url/sha256sum-amd64.txt" -o "$evidence/k3s-checksums"
(cd "$evidence"; awk '$2 == "k3s" {print}' k3s-checksums > k3s-checksum; test "$(wc -l < k3s-checksum)" = 1; sha256sum -c k3s-checksum)
sudo install -m 755 "$evidence/k3s" /usr/local/bin/k3s
if ! command -v kubectl >/dev/null; then sudo ln -s /usr/local/bin/k3s /usr/local/bin/kubectl; fi
sudo k3s server --https-listen-port=744 --disable=traefik --disable=servicelb > "$evidence/k3s-private.log" 2>&1 &
k3s_process=$!
proxy_process=''
cleanup() {
  if [[ -n "$proxy_process" ]]; then kill "$proxy_process" 2>/dev/null || true; fi
  podman stop spawnpod-upgrade-ci >/dev/null 2>&1 || true
  sudo kill "$k3s_process" 2>/dev/null || true
}
trap cleanup EXIT
for attempt in $(seq 1 120); do
  if sudo k3s kubectl get nodes >/dev/null 2>&1; then break; fi
  sleep 1
done
sudo cat /etc/rancher/k3s/k3s.yaml > "$evidence/kubeconfig"
export KUBECONFIG="$evidence/kubeconfig"
kubectl wait node --all --for=condition=Ready --timeout=180s
for archive in "$evidence/runtime.oci.tar" "$evidence/job.oci.tar" "$evidence/$previous_package.oci.tar"; do
  sudo k3s ctr -n k8s.io images import --digests "$archive"
done
python3 - "$evidence" <<'PY'
import json, pathlib, tarfile, subprocess, sys
p = pathlib.Path(sys.argv[1])
for filename, name, output in [('runtime.oci.tar', 'localhost/spawnpod:upgrade-ci', 'runtime-image'), ('job.oci.tar', 'localhost/spawnpod-ci-job:upgrade-ci', 'job-image')]:
    with tarfile.open(p / filename) as archive:
        digest = json.load(archive.extractfile('index.json'))['manifests'][0]['digest']
    reference = name.split(':')[0] + '@' + digest
    subprocess.run(['sudo', 'k3s', 'ctr', '-n', 'k8s.io', 'images', 'tag', name, reference], check=True)
    (p / output).write_text(reference)
PY
previous_image=$(cat "$evidence/IMAGE_DIGEST")
sudo k3s ctr -n k8s.io images tag "localhost/spawnpod:$previous_version" "$previous_image"

# Existing admission is required by the acceptance suite so it can restore it.
kubectl apply -f deploy/security.yaml
"$evidence/runtime/spawnpod" render-admission --config deploy/config.example.yaml > "$evidence/baseline-admission.yaml"
kubectl apply -f "$evidence/baseline-admission.yaml"

podman run -d --name spawnpod-upgrade-ci -p 127.0.0.1:3000:3000 \
  -e FORGEJO__actions__ENABLED=true "$forgejo_image"
for attempt in $(seq 1 120); do
  if curl -fsS http://127.0.0.1:3000/ >/dev/null; then break; fi
  sleep 1
done
python3 tests/integration/bootstrap-forgejo.py --password-file "$evidence/forgejo-password"
podman exec --user git spawnpod-upgrade-ci forgejo admin user generate-access-token \
  --username spawnpod-admin --token-name upgrade-ci --scopes all --raw > "$evidence/forgejo-api-token"
python3 - "$evidence/runner-secret" <<'PY'
import pathlib, secrets, sys
pathlib.Path(sys.argv[1]).write_text(secrets.token_hex(20))
PY
podman exec -i --user git spawnpod-upgrade-ci forgejo forgejo-cli actions register \
  --secret-file /dev/stdin --scope spawnpod-admin --name upgrade-ci --labels spawnpod --version v13.1.0 \
  < "$evidence/runner-secret" > "$evidence/runner-uuid"
node_address=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
pod_cidr=$(kubectl get nodes -o jsonpath='{.items[0].spec.podCIDR}')
python3 tests/integration/forgejo-local-proxy.py --bind "$node_address" --pod-cidr "$pod_cidr" > "$evidence/proxy.log" 2>&1 &
proxy_process=$!
python3 - "$evidence" "$repo_root" "$node_address" <<'PY'
import pathlib, sys, uuid, yaml
p, root = map(pathlib.Path, sys.argv[1:3])
cfg = yaml.safe_load((root / 'tests/integration/acceptance.config.yaml').read_text())
cfg['kubernetes'].update(namespace='sp-acceptance-ci-upgrade', leaseNamespace='sp-acceptance-infra-upgrade', kubeconfig=str(p/'kubeconfig'))
cfg['controller'].update(socketPath=str(p/'backend.sock'), enableServices=False)
cfg['agentImage'] = (p/'runtime-image').read_text()
for profile in cfg['profiles'].values():
    profile['image'] = (p/'job-image').read_text()
    for service in profile.get('services', {}).values():
        service['image'] = profile['image']
(p/'config.yaml').write_text(yaml.safe_dump(cfg, sort_keys=False))
runner = yaml.safe_load((root/'deploy/release/runner.example.yaml').read_text())
runner['runner'].update(timeout=cfg['profiles']['default']['maxLifetime'], labels=['spawnpod:spawnpod://'+cfg['profiles']['default']['image']])
runner['server']['connections']['forgejo'] = dict(url='http://'+sys.argv[3]+':3001/', uuid=str(uuid.UUID((p/'runner-uuid').read_text().strip())), token=(p/'runner-secret').read_text().strip())
(p/'runner.yaml').write_text(yaml.safe_dump(runner, sort_keys=False))
PY
export SPAWNPOD_ACCEPTANCE_CONFIG="$evidence/config.yaml"
export SPAWNPOD_CONTROLLER_CURRENT="$evidence/runtime/spawnpod"
export SPAWNPOD_CONTROLLER_PREVIOUS="$evidence/$previous_package/spawnpod"
export SPAWNPOD_ACCEPTANCE_PREVIOUS_IMAGE="$previous_image"
export SPAWNPOD_ACCEPTANCE_RUNNER_IMAGE="$runner_image"
export SPAWNPOD_ACCEPTANCE_RUNNER_CONFIG="$evidence/runner.yaml"
export SPAWNPOD_ACCEPTANCE_FORGEJO_TOKEN="$evidence/forgejo-api-token"
go test -v -count=1 -timeout=20m ./tests/integration/acceptance \
  -run 'TestLocalClusterAcceptance/(AdmissionRoundTripDiagnostics|BundleUpgradeAndRecovery)$' | tee "$evidence/acceptance.log"
