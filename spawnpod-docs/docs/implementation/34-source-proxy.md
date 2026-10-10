# Implementation step 34 — source proxy and K3s egress

**Date:** 2026-10-10. **Environment:** Fedora 43, single-node K3s
v1.34.12+k3s1 on `amd64`, Forgejo v15.0.9 bound by rootless Podman to
`127.0.0.1:3000`.

## Data path

The administrator starts [`spawnpod-source-proxy`](../../../cmd/spawnpod-source-proxy/main.go)
with a strict YAML route file. Each route names one lowercase DNS host, port
and fixed dial IP. Public destinations reject private, loopback, link-local,
metadata, shared and documentation address ranges. The local Forgejo route
uses the explicit `allowLoopback: true` exception, which accepts loopback
addresses only. The proxy does no DNS resolution. HTTP requests require an
absolute URL and exact approved host and port; proxy credentials and hop
headers are removed. HTTPS `CONNECT` requires an approved authority and a
single complete TLS ClientHello whose SNI equals that authority. Fragmented
or absent SNI fails closed. The proxy does not handle SSH checkout.

An administrator selects `network: source` in an execution profile and sets
`sourceProxy` to an HTTP URL with a fixed IP and port. The controller validates
both values. The job Pod receives `spawnpod.dev/network=source`; the generated
`spawnpod-source-proxy` NetworkPolicy admits only TCP to the proxy IP and port
for such Pods. The namespace default-deny remains. The Forgejo adapter sets
uppercase and lowercase HTTP(S) proxy variables for each command through the
agent request, not PodSpec. It clears both `NO_PROXY` variables. An isolated
profile has no proxy route. `render-admission` now also renders the source
policy when enabled; the quota must allow this additional policy. Controller
preflight compares the installed policy with the active configuration.

For the local smoke, the relevant configuration is:

```yaml
sourceProxy: http://10.42.0.1:3128
profiles:
  default:
    # Existing isolated profile fields.
  source:
    network: source
    # Same approved job image and bounds as default.
```

The full local source config is `.local/config.yaml` and the proxy route is
`.local/source-proxy.yaml`; both are ignored by Git. The pre-source config is
backed up as `.local/config-before-source.yaml`. A shareable local route
example is [`deploy/source-proxy.local.example.yaml`](../../../deploy/source-proxy.local.example.yaml).
At this step the proxy was a foreground local process. It was moved to a user
systemd service in [step 35](35-runner-https-source-and-systemd.md).

## Reproduction

```sh
go build -o .local/bin/spawnpod-source-proxy ./cmd/spawnpod-source-proxy
.local/bin/spawnpod-source-proxy --config .local/source-proxy.yaml

go build -o .local/bin/spawnpod-source ./cmd/spawnpod
.local/bin/spawnpod-source render-admission --config .local/config.yaml > .local/source-admission.yaml
kubectl --kubeconfig .local/kubeconfig apply -n ci -f .local/source-admission.yaml
.local/bin/spawnpod-source doctor --as-controller --config .local/config.yaml
.local/bin/spawnpod-source serve --as-controller --config .local/config.yaml
```

With the controller serving, create and probe a source environment through
the real Unix RPC. Use `-id-file .local/source-environment-id` for all calls.

```sh
go run ./deploy/smoke/recovery-client.go -mode create -profile source \
  -image docker.io/library/alpine@sha256:3e9b4b680bfc9fb5269227cffbd6d42be39fbf7c0b908123913864aa4447e764 \
  -id-file .local/source-environment-id
go run ./deploy/smoke/recovery-client.go -mode source-probe \
  -id-file .local/source-environment-id
go run ./deploy/smoke/recovery-client.go -mode remove \
  -id-file .local/source-environment-id
```

The source probe fetched Forgejo's version API through the proxy, received a
denial for another host, and failed direct TCP connections to the host's
Forgejo port, Kubernetes API Service and metadata IP. A host-side explicit
proxy request returned HTTP 200 for `forgejo.local:3000` and HTTP 403 for
`other.local:3000`. The live Pod had the `source` label, no PodSpec environment
and a matching `/32` egress rule for `10.42.0.1:3128`. The environment was
removed; only default-deny and source proxy policies remained in `ci`.

`go test ./...` and `go vet ./...` passed. Unit tests cover route validation,
private and metadata denial, exact HTTP host and port, CONNECT tunneling and
TLS SNI extraction. The local K3s test covered HTTP and NetworkPolicy;
after the admission change, a separate `default` environment still completed
Create, Start, Exec and Remove. The canonical `.local/config.yaml` passed
`doctor --as-controller`, and no temporary Pods or Services remained. The
proxy-aware HTTPS checkout through a real source host and the official Runner
was later verified in [step 35](35-runner-https-source-and-systemd.md). The
original Alpine probe verifies the HTTP network path; it has no Git or Node
runtime. `actions/checkout` and ARM64 remain separate gates.
