# Implementation step 28 — controller crash during service provisioning

**Date:** 2026-10-09. This local K3s test exercised recovery after an
unexpected controller exit, while service creation was still in progress.

[`make-service-delay-config.py`](../../../deploy/smoke/make-service-delay-config.py)
copies the ignored local config to a private file and inserts a 120-second
delay before the approved BusyBox echo service listens. The copy retains the
image, port, UID and resource limits. `validate` passed. We restarted the
controller with this temporary config and called `create-service` through the
same Forgejo plugin RPC used in earlier tests.

Environment `fbfc4c8b0df82529ae6667b701ab8220` was observed in
`provisioning` with its job Pod, service Pod, ClusterIP Service and two
NetworkPolicies present. We sent SIGKILL only to the test controller process.
The RPC client received `Unavailable`; the durable record remained
`provisioning` while the controller was down. A new controller started with
the ordinary `.local/config.yaml`, acquired the Lease, reconciled that record
to `cleaned`, and deleted all five resources. Only the baseline default-deny
policy remained in `ci`. The delayed service was not resumed or replayed.

To reproduce on an idle local `ci` namespace:

```sh
python3 deploy/smoke/make-service-delay-config.py
.local/bin/spawnpod validate --config .local/config.service-delay.yaml
# Stop the ordinary controller, then start:
.local/bin/spawnpod serve --as-controller --config .local/config.service-delay.yaml
# In a separate terminal, start:
.local/bin/spawnpod-recovery-client -mode create-service \
  -id-file .local/recovery-provision-crash-id -image "$APPROVED_ALPINE_DIGEST"
# Wait until the newest record is provisioning and all five resources exist.
# Send SIGKILL only to that controller PID, then start the ordinary controller.
```

The test did not interrupt an individual Kubernetes Create response. API loss
during Exec is covered in [step 29](29-live-api-outage.md).
`APPROVED_ALPINE_DIGEST` must be the digest from the local approved profile.
