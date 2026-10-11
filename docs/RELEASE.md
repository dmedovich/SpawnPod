# Release process

Release artifacts target Linux/amd64 and Linux/arm64. The current version is
recorded in `VERSION`. ARM64 needs execution tests on target hardware before
it is considered a validated deployment platform.

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
./scripts/release.sh "$(cat VERSION)" arm64
./scripts/release-image.sh
bash ./scripts/verify-release.sh
```

The scripts write to `dist/VERSION/`. They produce:

- Linux/amd64 and Linux/arm64 archives with three static binaries, binary checksums,
  `release.json`, dependency inventory, licenses, documentation and manifests.
- A local Linux/amd64 runtime OCI archive and `IMAGE_DIGEST`.
- `SHA256SUMS` covering the distribution files.

Verify the checksums, binary versions and source revision in `release.json`.
Published builds must have `dirty: false`. The image script rejects stale or
dirty package inputs. Registry pushes may change the manifest digest; verify
the digest reported by the registry before using it in configuration.

For a development build from uncommitted sources, use
`SPAWNPOD_ALLOW_DIRTY=1 ./scripts/release.sh`. Its provenance is marked dirty.
The packaging scripts do not upload artifacts or install into a cluster.

## Publish

The tag-triggered `.github/workflows/release.yml` runs the checks, builds and
verifies both archives, and pushes a multi-platform runtime image to
`ghcr.io/dmedovich/spawnpod:VERSION`. The image carries SPDX SBOM and SLSA
provenance attestations and a keyless Cosign signature. GitHub Releases receives
both archives, their SPDX SBOMs, checksums, the local amd64 OCI archive and
`GHCR_DIGEST`. GitHub artifact attestations bind archives to their build
provenance and SBOMs. Candidate versions create prereleases.
For a new version, tag the reviewed commit after checking that `VERSION`
and the release notes match:

```sh
release_version=$(cat VERSION)
git tag -a "$release_version" -m "SpawnPod $release_version"
git push origin "$release_version"
```

Keep existing release tags unchanged. Do not also run `gh release create`:
the workflow creates the release. Download the uploaded assets, verify
`SHA256SUMS` and compare their provenance with the tagged revision. Verify an
archive with `gh attestation verify ARCHIVE -R dmedovich/SpawnPod`. Verify the
image reference recorded in `GHCR_DIGEST` with:

```sh
release_version=v0.1.0-rc.6 # replace with the downloaded release tag
cosign verify "$(cat GHCR_DIGEST)" \
  --certificate-identity "https://github.com/dmedovich/SpawnPod/.github/workflows/release.yml@refs/tags/$release_version" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Before expanding support

Repeat installation, admission and network denial tests, representative
workflows and drained upgrades on the target cluster. Measure provisioning,
command and cleanup latency, idle CPU/RAM and concurrency with actual jobs.
Exercise eviction, OOM, storage exhaustion and API outages under that load.
Validate the job image, HTTPS trust and source-proxy routes.

ARM64 requires execution tests on real hardware before adding it as a
validated platform. HA, running-job migration and hostile tenant isolation
are outside the current [specification](SPEC.md).
