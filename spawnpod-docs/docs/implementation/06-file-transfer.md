# Implementation step 06 — workspace file transfer

**Status at step 06:** CopyIn and CopyOut are wired from the Forgejo protocol through the
engine and Kubernetes `pods/exec` to the job agent. Local unit tests pass;
streaming over a real K3s API server remains unverified. The listener was
added in [step 07](07-local-rpc-and-metadata.md).

## Package and command map

- `internal/workspace` validates canonical paths under `/workspace`, extracts
  tar archives and writes tar archives. It uses `os.Root` for directory-relative
  filesystem calls and rejects symlinks and special files. Limits are 10,000
  entries, 256 MiB per regular file and 512 MiB per inbound archive. Each
  inbound file is written to a temporary name and renamed after its full
  payload arrives. Modes are masked, and files are created by the job UID/GID.
- `cmd/spawnpod-agent copy-in DEST` reads tar from stdin; `copy-out SRC` writes
  tar to stdout. `serve` creates `.act`, `.toolcache` and `.tmp` under the
  workspace before declaring readiness.
- `internal/provider/kubernetes` verifies the recorded Pod UID and running
  phase, then opens a bounded `pods/exec` stream for the selected agent mode.
  Archive bytes pass through `io.Reader`/`io.Writer` without accumulating the
  full archive in controller memory. Agent stderr is capped at 8 KiB.
- `internal/engine` serializes transfers with Exec and Remove. CopyIn uses a
  durable `transferring` state. A failed or interrupted CopyIn makes the
  workspace uncertain and triggers Pod cleanup; reconciliation cleans a
  `transferring` record after controller restart. CopyOut is read-only and
  leaves the environment `ready` when complete.
- `internal/adapter/forgejo` validates the first CopyIn metadata frame,
  rejects metadata on later frames and streams bytes through a pipe. CopyOut
  sends chunks of at most 256 KiB. Runner paths all stay in `/workspace`,
  including the reported temporary directory `/workspace/.tmp`.

## Safety boundary and limits

Tar extraction accepts only directories and regular files. Absolute paths,
parent traversal, noncanonical names, duplicate entries, symlinks, hard links
and device entries fail. Existing symlinks in destination path components also
fail. `os.Root` contains any concurrent path replacement inside the mounted
workspace. This does not claim protection against bind mounts or arbitrary
devices inside the workspace; the restricted Pod spec must prevent those.

An incomplete archive may have earlier complete files committed. The engine
therefore removes the entire environment after a failed CopyIn instead of
reusing a partly changed workspace. CopyOut rejects symlinks and special
files, and limits file count and sizes. Cancellation closes the transfer's
Kubernetes exec stream; Pod deletion is the cleanup backstop.

## Verification and remaining work

Local tests cover extraction and archiving, traversal, symlink and oversized
entry rejection, incomplete-file cleanup, engine CopyIn failure cleanup,
reconciliation after interrupted CopyIn, CopyOut state, protocol framing and
Pod UID checks. Run `go test ./...` and `go vet ./...`.

Binary streaming and cancellation through real `pods/exec` on K3s and
non-root workspace ownership remain unverified. Image metadata and the local
listener were added in step 07. Service containers and network policy remain
separate release gates.
