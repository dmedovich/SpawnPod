# Implementation step 05 — Forgejo protocol mapping

**Status at step 05:** typed `plugin.v1alpha` adapter implemented Create,
Start, Exec, Remove and Capabilities mapping in process. CopyIn and CopyOut
were pending here and were implemented in [step 06](06-file-transfer.md).
The adapter was not registered on a listener at this step. This is a verified local
code step, not runner compatibility.

## Protocol provenance

`internal/adapter/forgejo/proto` contains the MIT-licensed `.proto` and two
generated Go files copied from the local runner tree on 2026-10-08. The
directory includes the license and a provenance note. The local tree has no
Git metadata; the exact Forgejo release revision still needs verification.
The upstream `go_package` remains in the schema, while spawnPod imports the
local copy of the generated code. The current module graph requires Go 1.25.

## Package behavior

- `New` requires a backend, record reader and valid operator configuration.
  The adapter copies the approved profiles. It cannot change Pod security
  settings or image digests from a workflow request.
- `Create` chooses the named `backend_options.profile`, defaulting to
  `default`. Unknown options, capabilities, services, unapproved job or label
  images, and timeouts above the profile maximum receive `InvalidArgument`.
  The backend generates the environment ID and persists the deadline. The
  runner name is stored in the management record for diagnosis; it is not an
  idempotency key.
- `Start` waits for the job agent, then sends `StartComplete`. Image `ENV`
  discovery was missing at this step and was added in step 07.
- `Exec` validates argv, environment keys, profile UID and workdir. It uses a
  random command ID and caps command execution at the supervisor's 30-minute
  limit or the environment deadline, whichever is shorter. It rejects a
  serialized request above the agent's 1 MiB input limit before changing
  environment state. Stdout/stderr are forwarded as data chunks. A normal
  command exit (including nonzero) becomes
  `ExecComplete`; infrastructure failure becomes `ExecFailed` with a nonempty
  message. The terminal message is sent only after the engine commits its
  state transition. Cancellation follows the stream context.
- `Remove` delegates to the engine's repeatable cleanup. Missing IDs map to
  `NotFound`. Invalid IDs map to `InvalidArgument`; Kubernetes availability
  errors map to `Unavailable`.
- `CopyIn` and `CopyOut` were the next dependency and are documented in step
  06. The listener was added in step 07.

## Verification and next dependencies

Unit tests cover valid Create mapping, rejected unsafe inputs, StartComplete,
Exec output/terminal ordering, nonzero exit, infrastructure failure and
status mapping. Run `go test ./internal/adapter/forgejo` to check this layer.

Bounded streaming tar transfer and the workspace directories were added in
[step 06](06-file-transfer.md). Metadata and a Unix socket server were added
in [step 07](07-local-rpc-and-metadata.md). Testing against the actual Runner
client remains. See the [protocol integration design](../integrations/forgejo.md) for
the release matrix and remaining compatibility gates.
