# Implementation step 14 — Forgejo service request mapping

**Status:** local adapter mapping and validation implemented. The example
configuration leaves `controller.enableServices: false`; the local smoke
profile enabled one approved service and passed a real Forgejo workflow.
See [step 25](25-live-service-isolation.md) for the live result and remaining
service cases.

## Request path

The Forgejo adapter selects the approved job profile, then validates at most
eight `CreateRequest.services` entries before calling the engine. Every service
name must be unique and declared in `profile.services`. Its image must exactly
match the pinned digest in that entry. The adapter constructs the service spec
from operator configuration; the workflow cannot set the command, user,
architecture or resource limits.

The Runner source passes workflow `ports` strings unchanged to the plugin.
The adapter accepts no port list, a single container port such as `6379`, or
an equal mapping such as `6379:6379`. All refer to the one approved Service
port. Remapped host ports, host IPs, dynamic publishing and multiple ports
are rejected. The approved port is available inside the environment even if
the workflow omits `ports`.

Workflow environment keys cannot be empty or contain `=` or NUL, and values
cannot contain NUL. The adapter also limits each service to 128 variables and
512 KiB total raw key/value bytes. The values
are passed as `engine.CreateRequest.ServiceEnvironments` for the immediate
startup stream. The engine's management record contains only the approved
service spec. The provider enforces its own 1 MiB encoded-request limit.
Errors from request validation use gRPC `InvalidArgument` and do not call the
engine.

## Gate and verification

`controller.enableServices` defaults to false. Setting it true allows only
services explicitly configured in the chosen profile. It is a temporary
operator gate, not a security control: it does not replace admission or
NetworkPolicy enforcement. The local single-node smoke passed an approved
service path; keep the gate off in production until broader service failure,
isolation, restart and compatibility scenarios pass. Config reload requires
a controller restart.

Local adapter tests cover accepted port spellings, profile-to-engine mapping,
transient environment forwarding, disabled gating, and rejection of unknown
images, service names, port remaps, duplicate entries and invalid environment.
