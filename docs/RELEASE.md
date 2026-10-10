# Release process

Supported platform: Linux/amd64. The current version is recorded in `VERSION`.

## Prepare the source

Update `VERSION`, the specification and `docs/releases/VERSION.md` for each
new release. Run:

```sh
make check
go test -race -short ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
git diff --check
```

`make check` includes a 100-second keepalive regression. The short test suite
skips that test. Review linked dependency licenses and the
[validation results](VALIDATION.md), then commit the release sources.

## Build the artifacts

Use Go 1.27.2, Podman and a clean working tree:

```sh
make release
./scripts/release-image.sh
```

The scripts write to `dist/VERSION/`. They produce:

- A Linux/amd64 archive with three static binaries, binary checksums,
  `release.json`, dependency inventory, licenses, documentation and manifests.
- A runtime OCI archive and `IMAGE_DIGEST`.
- `SHA256SUMS` covering the distribution files.

Verify the checksums, binary versions and source revision in `release.json`.
Published builds must have `dirty: false`. The image script rejects stale or
dirty package inputs. Registry pushes may change the manifest digest; verify
the digest reported by the registry before using it in configuration.

For a development build from uncommitted sources, use
`SPAWNPOD_ALLOW_DIRTY=1 ./scripts/release.sh`. Its provenance is marked dirty.
The packaging scripts do not upload artifacts or install into a cluster.

## Publish

The tag-triggered `.github/workflows/release.yml` runs the checks, builds the
packages and creates a GitHub prerelease with the release notes and assets.
For a new version, tag the reviewed commit after checking that `VERSION`
and the release notes match:

```sh
release_version=$(cat VERSION)
git tag -a "$release_version" -m "SpawnPod $release_version"
git push origin "$release_version"
```

Keep existing release tags unchanged. Do not also run `gh release create`:
the workflow creates the release. Download the uploaded assets, verify
`SHA256SUMS` and compare their provenance with the tagged revision.

## Before expanding support

Repeat installation, admission and network denial tests, representative
workflows and drained upgrades on the target cluster. Measure provisioning,
command and cleanup latency, idle CPU/RAM and concurrency with actual jobs.
Exercise eviction, OOM, storage exhaustion and API outages under that load.
Validate the job image, HTTPS trust and source-proxy routes.

ARM64 requires execution tests on real hardware before adding it as a
supported platform. HA, running-job migration and hostile tenant isolation
are outside the current [specification](SPEC.md).
