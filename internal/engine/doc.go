// Package engine owns the lifecycle of one CI execution environment.
//
// It has no Forgejo or Kubernetes API imports. A Store persists non-secret
// state; Jobs operates the approved job Pod and Services manages approved
// service Pods and ClusterIP objects. Engine serializes Start and Exec per
// environment while allowing Remove to request cancellation without waiting
// behind a streaming operation. Reconcile recovers work after a controller
// restart. Adapters implement Store, Jobs and Services for Kubernetes.
// Service ClusterIPs are resolved before job Pod creation so its immutable
// hostname aliases refer to the approved Services.
// Transient service environment values are passed to Services during Create
// and are never stored in environment records.
package engine
