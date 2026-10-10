// Package kubernetes creates and operates restricted CI job Pods.
//
// This package owns Kubernetes API types; the supervisor and engine do not
// import them. BuildJobPod is a pure function for reviewable PodSpec policy.
// Provider methods create a Pod, observe readiness, invoke commands, metadata
// and file transfers through pods/exec, and delete with a UID precondition.
// Exec requires one terminal agent event, drains its JSON line trailer before
// joining the transport, and cancels the stream on decode or callback errors.
// BuildServicePod and BuildService render approved service resources. Provider
// can create, recover, wait for and remove them using verified labels and UID
// preconditions. EngineServices connects that API to the environment engine.
// BuildJobPod renders validated service names and ClusterIPs as HostAliases.
// Service startup sends an approved command and transient environment through
// pods/exec stdin to an agent running as PID 1 in the service container.
// ConfigMapStore persists management records. Network policy remains separate
// work.
package kubernetes
