# Implementation step 35 — Runner HTTPS source checkout and proxy service

**Date:** 2026-10-10. **Environment:** official Forgejo Runner v13.1.0,
Forgejo v15.0.9, single-node K3s v1.34.12+k3s1 on `amd64`.

## HTTPS route and job image

The local proxy route file `.local/source-proxy.yaml` now includes the exact
`github.com:443` route pinned to the IP observed during this test,
`140.82.121.3`. Git on the host completed `ls-remote` through the proxy using
HTTPS `CONNECT`. This IP is a local test snapshot; an operator must verify
and update a public route when its upstream address changes. The proxy does
not resolve an arbitrary name supplied by a workflow.

The local `source` profile uses the pinned `amd64` image
`codeberg.org/forgejo/forgejo@sha256:46c82686a28aae45154325501ffa51aaee2cdf6cdfcf92137e3f7cce54987823`
because this already available image contains Git and CA certificates. Git
ran in the image as UID 10001 with a read-only root filesystem. K3s pulled
it and the source Pod reached Ready. This is a smoke-test image; a dedicated
minimal CI image is needed for production.

The generated admission allowlist was updated from `.local/config.yaml` and
applied to `ci`. The controller was restarted with that config. A temporary
`.local/runner-source.yaml` selected `profile: source` and the same pinned
image for the official Runner. Its token-bearing file has mode `0600` and is
ignored by Git. The normal Runner config was restored after the test.

## Official Runner result

[`workflow-runner-https-checkout.yml`](../../../deploy/smoke/workflow-runner-https-checkout.yml)
executes `git clone --depth 1 https://github.com/octocat/Hello-World.git`
inside the source job Pod, checks the cloned README and verifies the remote
URL. Forgejo run **36** completed `success`. Its SpawnPod environment record
used profile `source` and reached `cleaned`; no job Pod or Service remained.
The proxy injected HTTP(S) variables only into the command environment, and
the source NetworkPolicy admitted only the proxy's IP and port.

This verifies a real HTTPS Git checkout through the official Runner's shell
step and SpawnPod adapter. It does not verify the Node-based
`actions/checkout` action, a private Forgejo repository over HTTPS, or ARM64.
Those require a production-like CI image, Forgejo HTTPS endpoint and second
architecture. The local Forgejo instance still serves HTTP on loopback.
The Node action and private HTTPS checkout were subsequently verified using
a local TLS fixture in [step 36](36-actions-checkout-private-https.md).

## Persistent local proxy

The local user systemd unit is
[`deploy/systemd/spawnpod-source-proxy.service`](../../../deploy/systemd/spawnpod-source-proxy.service).
It starts the built binary and private route file from this checkout, restarts
on failure and uses `NoNewPrivileges` and a private `/tmp`. It was linked and
enabled in the user's systemd manager. `systemctl --user is-active` returned
`active`; the Git HTTPS probe still worked after the handoff. `loginctl`
reported `Linger=yes`, so the user manager can run after logout and at boot.

Useful local commands:

```sh
systemctl --user status spawnpod-source-proxy.service
systemctl --user restart spawnpod-source-proxy.service
journalctl --user -u spawnpod-source-proxy.service -n 50 --no-pager
loginctl show-user dmedovich -p Linger
```

The unit references `/home/dmedovich/SpawnPod/.local/bin` and `.local` config;
moving or deleting this checkout breaks the service. Rebuild the binary and
restart the unit after a proxy code change. Keep the route file operator-owned
and review `allowLoopback` routes. The public IP pin must be maintained; a
DNS change does not widen egress, but can stop source checkout until the pin
is updated.

`internal/sourceproxy` now has a regression test confirming that a CONNECT
with a different TLS SNI closes before forwarding bytes to the upstream.
`go test ./internal/sourceproxy` passed after this change.
