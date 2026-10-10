// Package supervisor controls job commands and one long-running service
// process inside isolated CI containers.
//
// The package has no Kubernetes or Forgejo dependency. A server owns a Unix
// socket inside the job Pod; an exec client connects to it and sends a Run
// request. Requests and events are newline-delimited JSON. Event data is a
// JSON byte slice (base64 on the wire). Closing the Run connection cancels its
// command. A separate Cancel request can stop a command even when the original
// Kubernetes exec transport has become unresponsive.
// ServiceServer accepts one start request over the same private-socket
// protocol, keeps the service process alive after the request disconnects,
// and exits when that process exits or the Pod is stopped.
//
// The supervisor is process control, not a security boundary against code in
// the same container. The controller must delete the whole Pod if cancellation
// cannot be confirmed.
package supervisor
