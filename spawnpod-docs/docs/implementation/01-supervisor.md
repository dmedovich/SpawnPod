# Implementation step 01 — command supervisor

**Status:** local implementation and tests complete; Kubernetes integration pending.

## Why this is the first step

Kubernetes `pods/exec` only transports a process stream. A CI executor needs
to know that cancellation actually stops the command, including quiet commands
and children. The supervisor is the process owner inside a job container.

## Packages and path through the system

1. `cmd/spawnpod-agent` runs `serve` as the main job-container process. It
   listens on `/tmp/spawnpod/control.sock` (an environment-private volume in
   the future Pod) and accepts one command at a time.
2. A future Kubernetes provider starts `spawnpod-agent invoke` with `pods/exec`
   and sends a JSON request on stdin. The request contains an opaque command
   ID, argv, per-command environment, working directory and timeout. Secret
   environment values are absent from command-line arguments.
3. `internal/supervisor` launches the process in a new process group and sends
   newline-delimited JSON events. `stream: stdout|stderr` events carry bytes;
   the final event has either `exit_code` or a non-empty `error`.
4. Closing the client socket cancels the command. `spawnpod-agent cancel ID`
   opens a second socket connection to request cancellation when the original
   exec stream is not reliable. The cancel response acknowledges the request;
   the controller must wait for the Run terminal event or delete the job Pod.
5. The supervisor sends TERM, waits a bounded grace, then sends KILL to the
   process group. It also applies a hard command timeout locally. The controller
   remains responsible for deleting the entire Pod if termination cannot be
   confirmed, or if job code bypasses process-group control.

## Local protocol

`Request`: `{ "op": "run", "id": "...", "argv": ["..."], "env": {},
"dir": "/workspace", "timeout_ms": 1000 }`. `op: cancel` requires only `id`.
The protocol is internal to the current agent binary; it is not the
Forgejo plugin protocol. A single event JSON object is sent per line. Output
bytes are base64-encoded by JSON. The controller must cap request and output
sizes and must not log request bodies.

## Verification

`go test ./...` covers separated stdout/stderr, nonzero exit status, explicit
cancellation of a silent command, local timeout, and cancellation after the
client disconnects. The tests use a real Unix socket and shell process.

## Known integration work

- Wire the agent into an approved image via a read-only tools mount and ensure
  the socket directory has the profile UID/GID permissions. A PodSpec and image
  recipe now exist, but this still needs a K3s integration test.
- Replace the hard-coded agent maximum timeout with validated profile policy
  while retaining a hard ceiling in the agent.
- Add command digest/idempotency checks, environment state persistence and a
  controller-side cancel-then-delete fallback.
- Verify process trees that create new sessions and failure behavior during a
  kubelet/API partition. The supervisor cannot itself prove that an escaped
  process has stopped; the Pod boundary is the backstop.
- Add a wire protocol version handshake before the controller and agent can be
  upgraded independently.
