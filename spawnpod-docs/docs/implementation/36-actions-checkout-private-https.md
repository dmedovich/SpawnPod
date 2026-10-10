# Implementation step 36 — Node action and private HTTPS checkout

**Date:** 2026-10-10. **Environment:** official Forgejo Runner v13.1.0,
Forgejo v15.0.9, single-node K3s v1.34.12+k3s1 on `amd64`.

## Result

[`workflow-runner-private-https-checkout.yml`](../../../deploy/smoke/workflow-runner-private-https-checkout.yml)
completed as Forgejo run **39**, status **success**. It runs the Node-based
`actions/checkout@v4` against the private `spawnpod-admin/spawnpod-smoke`
repository at `https://forgejo.local:3443`, with the workflow commit as its
explicit ref. The following shell step checked a repository file, the exact
HTTPS origin URL, `HEAD == GITHUB_SHA` and a shallow repository. The action's
cached `v4` ref resolved to `11d5960a326750d5838078e36cf38b85af677262`
and declared `using: node20`; the workflow still uses the mutable `v4` tag.

The source job used
`localhost/spawnpod-checkout@sha256:b11ffac6256a0324aac5e0d6ca996524bc5b58c1b28179b4cbf4dac847e52f83`.
The image contains Node **v20.20.2**, Git **2.52.0**, CA certificates and the
local smoke CA. A separate image probe ran both version commands as UID
10001 with a read-only filesystem. The live job Pod reached `1/1 Running`.

Environment `862a69dcf139e1a2e7482d03859fd272` used profile `source` and
reached `cleaned`. The namespace had no Pods or Services afterwards; only
`spawnpod-default-deny` and `spawnpod-source-proxy` NetworkPolicies remained.

## Local HTTPS fixture

[`prepare-forgejo-tls.sh`](../../../deploy/smoke/prepare-forgejo-tls.sh)
creates a private CA and a certificate for `forgejo.local` under ignored
`.local/forgejo-tls`. The CA expires after 30 days and the server certificate
after two days. The script refuses to overwrite existing keys.
[`forgejo-tls.go`](../../../deploy/smoke/forgejo-tls.go) terminates TLS only on
`127.0.0.1:3443` and forwards to local Forgejo at `127.0.0.1:3000`.
The source proxy has an exact `forgejo.local:3443` route to `127.0.0.1` with
the explicit local-test `allowLoopback` exception. A request through the
proxy verified the TLS certificate and returned Forgejo's version; anonymous
Git access to the private repository returned **401**.

[`checkout-job.Dockerfile`](../../../deploy/smoke/checkout-job.Dockerfile)
uses a digest-pinned Node Alpine base, installs Git and CA certificates, and
adds the public smoke CA. It does not copy either private key. The workflow
sets `NODE_EXTRA_CA_CERTS` and `GIT_SSL_CAINFO` to trust this CA without
disabling TLS verification. The official Runner downloads the action on the
host; the Node action and repository fetch execute in the source job Pod.
This test does not claim that action download on the Runner host goes through
the job Pod's source proxy.

## Keepalive compatibility fix

Run 37 was dispatched before the controller acquired its Lease and opened the
Unix socket. Run 38 encountered `ErrImagePull`: the prepared OCI archive had
not been imported and tagged in K3s containerd. During that quiet Start RPC,
the server closed the connection with `ENHANCE_YOUR_CALM / too_many_pings`.
Its environment was cleaned after the failed run.

The official Runner sends keepalive pings every 30 seconds on active RPCs;
gRPC's default server minimum is five minutes. The Unix server now explicitly
accepts a 30-second interval, while retaining the default restriction on
pings without an active stream. This prevents quiet Start and Exec streams
from being disconnected by normal Runner keepalive traffic.

`TestServeRunnerKeepalive` opens a health Watch with the Runner's keepalive
settings and waits for its 100-second deadline without application traffic.
It passed with the fix. A separate ignored copy using the former server
settings failed after **91 seconds** with `too_many_pings`, confirming the
test reproduces the original behavior. The test is skipped by `go test
-short`; a normal test run includes it.

`go test ./...` and `go vet ./...` passed before the change. After the fix,
`go test -short ./...`, the full keepalive regression test and `go vet ./...`
passed. The controller was rebuilt and restarted before successful run 39.

## Reproduction

The local TLS material and image were already prepared when this session
resumed. For a fresh fixture, generate the certificates first, then build
using `.local/forgejo-tls` as the build context:

```sh
deploy/smoke/prepare-forgejo-tls.sh
podman build -f deploy/smoke/checkout-job.Dockerfile \
  -t localhost/spawnpod-checkout:smoke .local/forgejo-tls
podman save --format oci-archive --output .local/spawnpod-checkout.oci.tar \
  localhost/spawnpod-checkout:smoke
tar -xOf .local/spawnpod-checkout.oci.tar index.json
sudo k3s ctr -n k8s.io images import --digests .local/spawnpod-checkout.oci.tar
sudo k3s ctr -n k8s.io images tag localhost/spawnpod-checkout:smoke \
  localhost/spawnpod-checkout@sha256:b11ffac6256a0324aac5e0d6ca996524bc5b58c1b28179b4cbf4dac847e52f83
```

A rebuild can produce a different digest: use the manifest digest from the
new archive in the tag, source profile, Runner label and generated admission
allowlist. The digest above identifies the image actually tested in run 39.
Apply the matching admission output and restart the controller. Keep the
normal `.local/runner.yaml` configuration intact; `.local/runner-source.yaml`
selects the source profile and tested image and contains private credentials.

With the proxy, Forgejo, TLS endpoint, controller and tagged Runner serving:

```sh
go build -o .local/bin/forgejo-tls ./deploy/smoke/forgejo-tls.go
.local/bin/forgejo-tls
# Run in another terminal once the controller socket is ready:
.local/bin/forgejo-runner-v13.1.0 --config .local/runner-source.yaml daemon
# Dispatch from another terminal:
python3 deploy/smoke/forgejo-workflow.py \
  --token-file .local/forgejo-admin-token \
  --workflow deploy/smoke/workflow-runner-private-https-checkout.yml
```

## Remaining limits

This verifies a private HTTPS checkout and Node action on `amd64` using a
local TLS fixture. ARM64 remains untested. The smoke image embeds a short-lived
local CA and installs unpinned Alpine package versions at build time; it is
not a production CI image. The TLS endpoint and Runner/controller started
here are foreground processes, unlike the persistent source-proxy systemd
service. Production image maintenance, a real HTTPS Forgejo deployment and
broader checkout options (submodules, LFS, alternate paths) remain separate
validation work.
