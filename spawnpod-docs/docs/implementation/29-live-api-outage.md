# Implementation step 29 — Kubernetes API loss during Exec

**Date:** 2026-10-09. This local K3s test interrupted only the controller's
API connection. K3s itself and the direct admin kubeconfig remained available.

[`make-api-proxy-config.py`](../../../deploy/smoke/make-api-proxy-config.py)
created mode `0600` copies of the ignored local config and kubeconfig. The
copy points to `https://127.0.0.1:1744`; `socat` forwarded that loopback port
to the real API at `127.0.0.1:744`. `doctor --as-controller` passed through
the proxy. We stopped the ordinary controller, started it with the proxy
config, and created environment `b0860ac696c34d1d2f73d1917fada116`.

The recovery client started a long `hold` command and printed its start
marker. Stopping `socat` with Ctrl-C closed the active API connections. The
Exec RPC ended with `Exec failed: execution failed`; the controller logged
that the agent stream ended without a terminal event. Lease renewal then
failed, so the controller stopped. With direct K3s access, the durable record
was still `executing` and its Pod was `Running`. A direct process listing
inside that Pod showed only the agent and the listing command: the held
process had stopped after the transport disconnected.

We restarted the ordinary controller with direct API access. After it
acquired the Lease, initial reconciliation set the record to `cleaned` and
deleted the Pod. The command was not replayed. The namespace had zero Pods
and Services, only the baseline default-deny policy, and no cleanup-preview
candidates.

To reproduce on the local host, with no unrelated `ci` workload:

```sh
python3 deploy/smoke/make-api-proxy-config.py
# Terminal A: keep this foreground process running.
socat TCP4-LISTEN:1744,bind=127.0.0.1,reuseaddr,fork TCP4:127.0.0.1:744
# Terminal B: verify the proxy and run the controller.
.local/bin/spawnpod doctor --as-controller --config .local/config.api-proxy.yaml
# Stop the ordinary controller, then start it through the proxy.
.local/bin/spawnpod serve --as-controller --config .local/config.api-proxy.yaml
# Terminal C: create an environment and start hold in separate commands.
.local/bin/spawnpod-recovery-client -mode create \
  -id-file .local/recovery-api-loss-id -image "$APPROVED_ALPINE_DIGEST"
.local/bin/spawnpod-recovery-client -mode hold \
  -id-file .local/recovery-api-loss-id
# After "command started", stop socat with Ctrl-C in Terminal A.
# Observe the client, record and Pod using the direct .local/kubeconfig.
# Restart serve with .local/config.yaml and verify cleanup.
```

Keep the admin kubeconfig copies private and ignored. This test interrupted
an active Exec and the Lease connection. It did not target a particular
Kubernetes Create/Delete response or a controller that remains leader through
a brief API outage. Those failure points need separate injection.
