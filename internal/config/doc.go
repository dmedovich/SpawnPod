// Package config loads the operator-owned spawnPod configuration.
//
// YAML is decoded with unknown-field rejection and only one document is
// accepted. Validation checks namespace names, durations, approved image
// digests, architecture, private socket path, non-root identities and resource
// bounds and the aggregate namespace quota before cluster access. Profiles
// are immutable inputs to Forgejo
// Create requests; workflow data cannot edit this configuration at runtime.
package config
