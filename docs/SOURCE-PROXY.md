# Controlled source networking (RC7 candidate)

This runbook accompanies `RC7-AUDIT-AND-PLAN.md`. Local K3s v1.34.12 network and
two-label official Runner v13.1.0 and isolated RC6 upgrade/rollback gates passed.
Private checkout and scoped OCI push/pull passed locally through the proxy.
Target-CNI checks remain for operator acceptance after release. The operator examples
must be adapted and verified on each installation.

## Runtime and configuration

The existing release runtime contains `/spawnpod-source-proxy` and runs as
UID/GID 10001. Pin the verified runtime digest and explicitly override its default
controller command. Use the existing release checksums, signatures and provenance;
there is no separate proxy signing mechanism.

```sh
spawnpod-source-proxy --config routes.yaml --validate
spawnpod-source-proxy --config routes.yaml
```

Configuration uses a literal listen IP, numeric port, optional `maxConnections`
(default 128, range 1–4096), and 1–64 operator-owned routes. The executable caps
accepted connections, including idle HTTP sockets and CONNECT tunnels. Active
requests are also bounded. Saturation can queue connections in the OS backlog;
clients must use timeouts. Resource budgets still need measurement under load.

HTTP headers have a five-second timeout, idle HTTP sockets 30 seconds, upstream
dials five seconds, HTTP upstream response headers 30 seconds, and the first
TLS record ten seconds. Requests/tunnels have an absolute 30-minute limit.
SIGTERM closes active requests and tunnels; in-flight transfers fail and clients
must retry. Configuration is loaded on startup; there is no dynamic reload.

Route validation accepts ordinary public unicast addresses only. It rejects
private, link-local, multicast, documentation, benchmarking and transition
ranges. IPv6 routes must belong to `2000::/3`; `2001::/23`, `2001:db8::/32`,
`2002::/16` and `3fff::/20` are conservatively excluded. This also excludes
some globally reachable special-purpose services. The
[IANA IPv4](https://www.iana.org/assignments/iana-ipv4-special-registry/) and
[IPv6 registries](https://www.iana.org/assignments/iana-ipv6-special-registry/)
explain these allocations. Public node/API addresses still require operator
review and an exact proxy egress policy.

CONNECT requires an explicit host:port and one complete initial TLS ClientHello
with an exact matching lowercase SNI. Fragmented, missing, encrypted, duplicate
or malformed extensions fail closed. No upstream connection opens before this
check. A CONNECT 200 only acknowledges tunnel setup; TLS/upstream failure can
still close it. Certificate verification remains the client's responsibility.

The proxy accepts any HTTP method at an approved destination, including HTTPS
write operations. Scope Forgejo repository/package credentials independently.
Never disable certificate verification or put tokens in routes, images or bundles.
The proxy emits startup/configuration errors, not per-request headers or bodies;
there is no metrics or HTTP health endpoint in RC7 so far.

## Kubernetes example

Review `deploy/release/source-proxy.kubernetes.example.yaml` and copy it into
operator-controlled deployment sources. Before applying, replace:

- the runtime image with a verified digest available on the target architecture;
- Service ClusterIP with a reserved free address in the actual Service CIDR;
- the route's upstream IP and matching proxy egress CIDR with a verified pin;
- infrastructure/workload namespace references if the installation uses other names.

The supplied upstream IP is an RFC observation, not a permanent record of DNS
or ownership. The example requests 50m CPU / 32Mi memory and limits 500m / 128Mi;
measure and adjust against the infra namespace quota. Workload quota is separate.

The ConfigMap is immutable and versioned. The Deployment has a read-only rootfs,
dropped capabilities, RuntimeDefault seccomp, no API token and no host resources.
Proxy ingress combines the workload namespace selector and source job selector
in one peer. Proxy egress permits only the pinned upstream IP:443, without DNS.
Other policies are additive: review every policy selecting jobs or proxy pods.

Apply the proxy boundary policy before starting the Deployment. TCP probes only
prove that the listener accepts connections, not upstream health or CNI isolation.
Verify actual requests and packet denials before enabling source scheduling.
In particular, check Service DNAT behavior on the actual CNI; never temporarily
allow broad namespace egress to debug it.

`sourceProxyPolicy` defaults to `ip`, preserving the exact IP/port policy. Local
K3s v1.34.12 rejects that rule for a ClusterIP because it evaluates egress after
DNAT. For a Kubernetes-hosted proxy use `sourceProxyPolicy: pod-selector`: the
destination must simultaneously belong to `kubernetes.leaseNamespace` and carry
both `app.kubernetes.io/name: spawnpod-source-proxy` and
`spawnpod.dev/component: source-proxy`, on the configured TCP port. The namespace
must differ from the workload namespace and remain operator-controlled. Anyone
who can create or relabel Pods there can impersonate this identity. Do not grant
workflows or Runner that access. No IPBlock or namespace-wide allowance is added.

The literal Service IP remains pinned in `sourceProxy` and injected proxy env.
In selector mode, source jobs can also address the selected proxy Pods directly;
the same route allowlist and proxy ingress/egress boundary apply. NetworkPolicy
does not distinguish a Service connection from a direct backend connection after
DNAT. Recheck enforcement on the target CNI and after changes to proxy selectors.

## Profiles and Runner

Keep `profiles.default.network: isolated`. Add a separate `source` profile with
the approved job image and resource settings, `network: source`, and
`sourceProxy: http://<verified-literal-proxy-IP>:3128` at the root of SpawnPod config.
Keep services disabled for the initial network gate.

Render with the supported `spawnpod upgrade render` command. The job egress
policy is in `20-admission.yaml`, with the workload namespace, and appears in
bundle inventory in the POLICY phase. Review it before switching Controller.
Do not hand edit generated bundle files.

`deploy/release/runner-source.example.yaml` is the locally verified two-plugin setup:
`spawnpod` selects profile default, `spawnpod-source` selects source; both connect
to the same private socket. The corresponding workflow selection is:

```yaml
jobs:
  isolated:
    runs-on: spawnpod
    steps:
      - run: echo isolated-smoke
  controlled:
    runs-on: spawnpod-source
    steps:
      - run: git ls-remote https://git.dmedovich.com/OWNER/REPOSITORY.git HEAD
```

The selected label image must match its profile exactly. The job image needs
Git, CA trust and the chosen registry client's dependencies. This example is
verified for profile selection and HTTPS probes on the pinned official Runner
image. The separate local Runner gate also exercises pinned actions/checkout
and a small OCI artifact with verified TLS and scoped package credentials.
Do not use `?profile=source` label query syntax: the reviewed Runner parser
rejects that option. Runner action downloads use a separate network path.

## Pin rotation and rollback

Verify a replacement upstream IP and ownership outside untrusted workflows.
Create a new immutable ConfigMap name, update the operator Deployment reference,
and coordinate the exact proxy egress pin with the route change. Pause source
submissions and finish active jobs before this change; the one-replica Recreate
Deployment has a maintenance interval. Verify HTTPS and all negative checks
before resuming. Retain the previous ConfigMap, Deployment and policy sources
for restoration. A pod restart alone does not update its immutable routes.

For an RC7 rollback, pause source submissions, drain and stop Controller/Runner,
restore the complete previous RC6 dependencies/admission/config/deployment bundle,
then verify isolated smoke before removing unused proxy resources. Preserve
default-deny and Runner credentials. Endpoint and route pin changes require
their own reviewed source changes; Deployment-only rollout undo is insufficient.
