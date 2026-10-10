# Release process and acceptance checklist

Version: **v0.1.0-rc.1**. The initial PRD is a full-product roadmap. This first
candidate is scoped by SPEC.md and does not close every production gate.

## Candidate gates

| Gate | Evidence / status |
|---|---|
| Official Runner pinned and tested | v13.1.0 commit recorded in SPEC; journal step 32 |
| Lifecycle, services, files, errors, cancellation | Official Runner workflows, steps 32–33 |
| Private HTTPS Node checkout | Run 39, step 36 |
| Negative admission, RBAC, tar and network tests | Steps 23, 25, 33–34 |
| Restart/API/deletion/lost-response recovery | Steps 24, 26–31 |
| Normal shutdown and Lease loss | Drain tests and candidate live checks, step 37 |
| Offline install rendering and credential separation | deployment tests, server-side dry run in step 37 |
| Versioned binaries, OCI recipe/archive, checksums | scripts/release.sh and candidate artifacts |
| Source provenance | Commit/tag plus clean release.json required before publication |
| External release publication | Complete only when the tag and assets are visible on GitHub |

## Gates for a later production support claim

- Run installation, live denial checks, network isolation and representative
  workflows on the actual target cluster. The current user scope excludes
  deployment to `shusha`.
- Measure cold provisioning/pulls, command/cleanup latency, idle CPU/RAM and
  concurrency with an explicit node budget protecting existing services.
- Expand fault evidence where needed: supervisor process death, true eviction,
  OOM/storage/scheduling failures, cancel/Remove race and process descendants.
  Unit coverage or Pod deletion is not automatically the same live fault.
- Verify real upgrade/rollback compatibility once two versions exist.
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
# Optional targeted race checks:
go test -race -short ./internal/controller ./internal/transport/unixrpc ./internal/telemetry
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
git tag -a v0.1.0-rc.1 -m 'SpawnPod v0.1.0-rc.1'
git push origin main
git push origin v0.1.0-rc.1
# If GitHub CLI is installed/authenticated:
gh release create v0.1.0-rc.1 --verify-tag --prerelease \
  --title 'SpawnPod v0.1.0-rc.1' \
  --notes-file docs/releases/v0.1.0-rc.1.md \
  dist/v0.1.0-rc.1/spawnpod_v0.1.0-rc.1_linux_amd64.tar.gz \
  dist/v0.1.0-rc.1/spawnpod_v0.1.0-rc.1_linux_amd64.oci.tar \
  dist/v0.1.0-rc.1/IMAGE_DIGEST \
  dist/v0.1.0-rc.1/SHA256SUMS
```

[GitHub's release command](https://cli.github.com/manual/gh_release_create)
can publish an existing tag with attached artifacts. Confirm the tag points
to release.json's source revision and download/verify uploaded assets before
marking publication complete. A local build or a local tag alone is not a
published release.
