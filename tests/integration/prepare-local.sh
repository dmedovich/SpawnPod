#!/usr/bin/env bash
set -euo pipefail

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
local_dir="$repo_root/.local"
mkdir -p "$local_dir"
chmod 700 "$local_dir"
mkdir -p "$local_dir/bin"

(cd "$repo_root" && env TMPDIR=/var/tmp go build -o "$local_dir/bin/spawnpod" ./cmd/spawnpod)
(cd "$repo_root" && env TMPDIR=/var/tmp go build -o "$local_dir/bin/spawnpod-agent" ./cmd/spawnpod-agent)
(cd "$repo_root" && env TMPDIR=/var/tmp go build -o "$local_dir/bin/spawnpod-recovery-client" ./tests/integration/recovery-client.go)
runner_source=${SPAWNPOD_RUNNER_SOURCE:-}
if [ -n "$runner_source" ] && [ -f "$runner_source/go.mod" ]; then
  (cd "$runner_source" && env TMPDIR=/var/tmp go build -o "$local_dir/bin/forgejo-runner" .)
fi

podman build -f "$repo_root/tests/integration/agent.Dockerfile" -t localhost/spawnpod-agent:smoke "$repo_root"
podman pull docker.io/library/alpine:3.22
podman save --format oci-archive --output "$local_dir/spawnpod-agent.oci.tar" localhost/spawnpod-agent:smoke

agent_digest=$(tar -xOf "$local_dir/spawnpod-agent.oci.tar" index.json | python3 -c 'import json,sys; print(json.load(sys.stdin)["manifests"][0]["digest"])')
job_digest=$(podman image inspect docker.io/library/alpine:3.22 --format '{{.Digest}}')
export SPAWNPOD_SMOKE_ROOT="$repo_root"
export SPAWNPOD_SMOKE_AGENT_DIGEST="$agent_digest"
export SPAWNPOD_SMOKE_JOB_DIGEST="$job_digest"
python3 - "$repo_root/deploy/config.example.yaml" "$local_dir/config.yaml" "$local_dir/runner.generated.yaml" <<'PY'
import os
import pathlib
import sys

source = pathlib.Path(sys.argv[1]).read_text()
root = os.environ["SPAWNPOD_SMOKE_ROOT"]
agent = "localhost/spawnpod-agent@" + os.environ["SPAWNPOD_SMOKE_AGENT_DIGEST"]
job = "docker.io/library/alpine@" + os.environ["SPAWNPOD_SMOKE_JOB_DIGEST"]
source = source.replace('kubeconfig: ""', 'kubeconfig: ' + root + '/.local/kubeconfig')
source = source.replace('socketPath: /run/spawnpod/backend.sock', 'socketPath: ' + root + '/.local/run/backend.sock')
source = source.replace('registry.example.com/spawnpod-agent@sha256:' + 'a' * 64, agent)
source = source.replace('registry.example.com/ci-base@sha256:' + 'b' * 64, job)
for old, new in {
    'maxPods: 16': 'maxPods: 2',
    'maxConfigMaps: 1024': 'maxConfigMaps: 64',
    'maxServices: 16': 'maxServices: 1',
    'maxNetworkPolicies: 33': 'maxNetworkPolicies: 1',
    'requestsCPU: "4"': 'requestsCPU: "1"',
    'limitsCPU: "8"': 'limitsCPU: "2"',
    'requestsMemory: 4Gi': 'requestsMemory: 512Mi',
    'limitsMemory: 16Gi': 'limitsMemory: 2Gi',
    'requestsStorage: 16Gi': 'requestsStorage: 2Gi',
    'limitsStorage: 48Gi': 'limitsStorage: 8Gi',
}.items():
    if old not in source:
        raise SystemExit('example quota field changed: ' + old)
    source = source.replace(old, new, 1)
if 'registry.example.com/' in source:
    raise SystemExit('example image placeholders were not replaced')
pathlib.Path(sys.argv[2]).write_text(source)

runner = f'''# Fill server.connections after Forgejo registration. Keep this file private.
runner:
  capacity: 1
  timeout: 30m
  labels:
    - "spawnpod:spawnpod://{job}"
cache:
  enabled: false
plugins:
  spawnpod:
    address: unix://{root}/.local/run/backend.sock
    options:
      profile: default
server:
  connections: {{}}
'''
pathlib.Path(sys.argv[3]).write_text(runner)
PY
chmod 600 "$local_dir/config.yaml" "$local_dir/runner.generated.yaml"
if [ ! -e "$local_dir/runner.yaml" ]; then
  cp "$local_dir/runner.generated.yaml" "$local_dir/runner.yaml"
  chmod 600 "$local_dir/runner.yaml"
fi

printf 'Prepared %s and %s\n' "$local_dir/config.yaml" "$local_dir/runner.generated.yaml"
printf 'Existing %s was preserved; compare its label and socket with the generated file.\n' "$local_dir/runner.yaml"
printf 'Binaries are in %s\n' "$local_dir/bin"
printf 'Agent image: localhost/spawnpod-agent@%s\n' "$agent_digest"
printf 'Job image: docker.io/library/alpine@%s\n' "$job_digest"
printf 'Import with: sudo k3s ctr -n k8s.io images import --digests %s\n' "$local_dir/spawnpod-agent.oci.tar"
printf 'Tag digest with: sudo k3s ctr -n k8s.io images tag localhost/spawnpod-agent:smoke localhost/spawnpod-agent@%s\n' "$agent_digest"
