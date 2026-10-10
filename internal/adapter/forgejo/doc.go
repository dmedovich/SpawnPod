// Package forgejo maps the pinned Forgejo Runner plugin.v1alpha contract to
// spawnPod's environment engine. The Lease-holding controller registers it
// on a private Unix socket; the protocol types are pinned locally. Service
// requests require an explicit controller gate and approved profile entries.
package forgejo
