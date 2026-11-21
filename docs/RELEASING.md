# Releasing the backend

GitHub is the development home of the public Go backend. Branch from `main`
and merge after the required tests, PostgreSQL 16 & 18 integration, container,
and privacy checks pass. Keep pending changes in `CHANGELOG.md` under
`## Unreleased`.

## Release checklist

1. Update root `VERSION`, `docker-compose.yml` default image tag, and
   `CHANGELOG.md` together in a reviewed pull request. `VERSION` contains a
   stable semantic version without its `v` prefix.
2. Confirm the release commit is on `main` and all required checks are green.
   Test database changes against disposable PostgreSQL 16 and 18 instances and
   review public artifacts for secrets and identifying deployment data.
3. Create an immutable `vX.Y.Z` tag on that exact checked commit and push it.
4. The tag workflow re-runs public CI/privacy, qualifies both `linux/amd64` and
   `linux/arm64` natively, proves `main` ancestry and version equality, pushes
   the multi-arch OCI index to `ghcr.io/spencernorton/dh-waypoint`, generates a
   CycloneDX SBOM, and attaches GitHub OIDC build provenance attestations.
5. Download and qualify the release assets before deployment. Never mutate a
   published tag or include private configuration/data in an artifact.

## Container tag channels

- `ghcr.io/spencernorton/dh-waypoint:vX.Y.Z` — immutable multi-arch release tag
  (`linux/amd64`, `linux/arm64`). Always pin production deployments by immutable
  digest (`IMAGE_DIGEST.txt`).
- `:vX` and `:vX.Y` — floating major and minor channels (for example `:v5`,
  `:v5.15`, `:v6`).
- `:latest` — updated automatically for `v5.x` releases and held across major
  version boundaries (`v6+`) until repository variable
  `RELEASE_PROMOTE_V6_LATEST` is explicitly set to `true`, protecting unpinned
  installations from unexpected major upgrades.

## Published artifacts

- `source.tar.gz` — `git archive` of the checked tag.
- `IMAGE_DIGEST.txt` — immutable multi-arch OCI index digest (plus
  `IMAGE_DIGEST_AMD64.txt` and `IMAGE_DIGEST_ARM64.txt`).
- `TESTED_IMAGE.json` and `TESTED_IMAGE_ARM64.json` — runtime qualification
  receipts bound to the exact exported image IDs.
- `SBOM.cdx.json` — CycloneDX JSON software bill of materials generated from
  the runtime-qualified container image.
- `SHA256SUMS.txt` — SHA-256 manifest covering every published release asset.

Verify the asset checksums, verify provenance with
`gh attestation verify oci://ghcr.io/spencernorton/dh-waypoint@<digest> -R spencernorton/DH-Waypoint`,
inspect OCI labels (`version`, `revision`, `created`, `source`), and run:

```sh
sh scripts/check_backend_image.sh '<exact-image-reference>' vX.Y.Z
```

## Local candidate builds

Build from a clean checkout or an exact `git archive`, specifying `--platform
linux/amd64` (or `linux/arm64`). Pass Docker arguments `VERSION` (with `v`
prefix), `COMMIT`, `BUILD_DATE` (the source commit timestamp), and `SOURCE`
(the public repository URL), as the release workflow does.

## Deployment and recovery

Each consumer manages its own secrets, configuration, and deployment. Record
version/digest before upgrading, back up state, rehearse restoration, and retain
the previous artifact. Before upgrading from `v5.x` to `v6.0.0`, run
`dh-waypoint doctor --check-v6` on `v5.15.0` to inspect readiness and obtain
the database fingerprint required by v6's pre-flight backup guard. Review
[4.0 migration](migration-v4.md) when upgrading from a combined 3.x image and
[processor migration](processors/migration.md) for the 5.x runtime boundary.
