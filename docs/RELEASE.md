# Release process and acceptance checklist

Version: **v0.1.0-rc.2**, Linux/amd64. Scope: [SPEC.md](SPEC.md).

## Candidate gates

The candidate contains the security fixes described in [SECURITY.md](SECURITY.md).
[VALIDATION.md](VALIDATION.md) records source checks, local K3s isolation,
failure recovery, quiescent rc.1/rc.2 compatibility and three-job measurements.
Final artifacts must be built from clean committed sources, with matching
`release.json`, tag, binary checksums and OCI digest. Publication is a separate
operator action; a local tag and package are not a published release.

## Gates for a later production support claim

- Run installation, live denial checks, network isolation and representative
  workflows on the actual target cluster. The current scope excludes
  deployment to the target server.
- Measure cold provisioning/pulls, command/cleanup latency, idle CPU/RAM and
  concurrency with an explicit node budget protecting existing services.
- Expand fault evidence where needed: supervisor process death, true eviction,
  OOM/storage/scheduling failures, cancel/Remove race and process descendants.
  Unit coverage or Pod deletion is not automatically the same live fault.
- Repeat the documented quiescent upgrade/rollback on the target installation.
- Validate a dedicated maintained job image and production HTTPS trust/source
  endpoints. Local short-lived smoke CA and maintained route snapshots are
  not production configuration.
- Test ARM64 on real hardware before advertising ARM64 support or distributing
  it as a supported platform. No ARM64 machine is needed to release this
  explicitly amd64-only candidate.
- HA, broad Docker compatibility, Helm, signing, automatic image publication
  and additional integrations are outside this candidate's contract.

## Build and verify

```sh
make check
# Race and dependency checks:
go test -race -short ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
# Commit reviewed sources before the final build.
make release
# For a clearly marked local preview only:
SPAWNPOD_ALLOW_DIRTY=1 ./scripts/release.sh
```

`make check` includes the real-time 100-second keepalive regression. The
short suite skips only that long test. The build script creates a static
amd64 archive, binary checksums, release provenance, dependency inventory
and bundled third-party notices. Verify all licenses and missing inventory
entries as part of distribution review. It never uploads or installs.

Run `./scripts/release-image.sh` to build/export the OCI image and complete
checksums. Alternatively build the OCI image from the extracted architecture directory using
`deploy/release/runtime.Dockerfile`. Export it with `podman save --format
oci-archive`, inspect the archive's index manifest digest and include the
archive, digest and SHA256SUMS with the release. Registry pushes may use a
different manifest digest and must be verified independently.

## Publish from the reviewed source revision

The tag-triggered `.github/workflows/release.yml` automates the checks, clean
binary/image build and prerelease upload. Git pushes can use an existing SSH
identity; no personal API token is needed on the workstation.

The source remote is `https://github.com/dmedovich/SpawnPod`. Publication
requires authorized GitHub access. No personal token should be placed in
source, configuration examples, command arguments or release artifacts.

```sh
git tag -a v0.1.0-rc.2 -m 'SpawnPod v0.1.0-rc.2'
git push origin main
git push origin v0.1.0-rc.2
# If GitHub CLI is installed/authenticated:
gh release create v0.1.0-rc.2 --verify-tag --prerelease \
  --title 'SpawnPod v0.1.0-rc.2' \
  --notes-file docs/releases/v0.1.0-rc.2.md \
  dist/v0.1.0-rc.2/spawnpod_v0.1.0-rc.2_linux_amd64.tar.gz \
  dist/v0.1.0-rc.2/spawnpod_v0.1.0-rc.2_linux_amd64.oci.tar \
  dist/v0.1.0-rc.2/IMAGE_DIGEST \
  dist/v0.1.0-rc.2/SHA256SUMS
```

[GitHub's release command](https://cli.github.com/manual/gh_release_create)
can publish an existing tag with attached artifacts. Confirm the tag points
to release.json's source revision and download/verify uploaded assets before
marking publication complete. A local build or a local tag alone is not a
published release.
