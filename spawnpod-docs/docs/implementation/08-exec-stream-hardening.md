# Implementation step 08 — agent exec stream completion

**Status:** local provider fix and regression tests complete. Real Kubernetes
SPDY behavior still requires a K3s test.

## Why this changed

The agent writes one JSON event per line. After a terminal event, the previous
provider read at most one more byte before waiting for the `pods/exec` writer
to finish. The terminal line's newline could be mistaken for a protocol
violation. If more output remained in the pipe, the writer could block while
the provider waited for it.

## Control flow

`internal/provider/kubernetes.Exec` decodes events until a terminal event,
then drains the remaining stream to EOF. The drain accepts JSON whitespace
and flags any other byte as output after the terminal event. It retains none
of those bytes in memory. If decoding or the event callback fails, it cancels
the SPDY stream and closes the pipe reader before waiting for the writer.
Agent stderr is capped at 8 KiB instead of accumulating without a bound.

## Verification

`go test ./internal/provider/kubernetes` covers a terminal JSON event with a
256 KiB trailing frame and an ordinary whitespace trailer. These tests prove
local pipe handling; they do not establish K3s `pods/exec` behavior. The next
integration gate remains a real cluster run with command output, cancellation,
metadata and file transfer.
