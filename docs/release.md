# Release

Release builds produce the three shipped binaries:

- `flow`
- `flow-server`
- `flow-worker`

The apps support `--version`:

```sh
flow --version
flow-server --version
flow-worker --version
```

Release builds inject the repository tag (`v*`) into those commands. The Flow
API protocol version is separate and is shown by command status output and the
`Flow-Protocol-Version` HTTP header.

## Artifacts

Pushing a `v*` tag runs `.github/workflows/release.yml`. The workflow builds:

- macOS arm64 on `macos-26`
- Linux amd64
- Linux arm64

It publishes tarballs, `.deb`, `.rpm`, a signed and notarized macOS `.pkg`,
Homebrew bottles for the `flow`, `flow-server`, and `flow-worker` formulae on
macOS arm64, macOS Intel, Linux amd64, and Linux arm64, Docker images for Linux
amd64 and Linux arm64, SHA-256 checksums, and GitHub artifact attestations. It
then updates `ClarifiedLabs/homebrew-tap` through a GitHub App installation
token.

The release workflow publishes these GitHub Container Registry images:

- `ghcr.io/clarifiedlabs/flow-server`
- `ghcr.io/clarifiedlabs/flow-worker`

Each release image gets `vX.Y.Z`, `X.Y.Z`, and `latest` tags. The `vX.Y.Z` and
`X.Y.Z` tags identify the release, while `latest` tracks the newest published
release. If GitHub creates either package as private, make it public once in the
package settings so users can pull the images without authentication.

The tap repository must already exist with an initialized default branch. No
formula files are required ahead of time; the release workflow writes
`Formula/flow.rb`, `Formula/flow-server.rb`, `Formula/flow-worker.rb`, and
`Formula/flow-full.rb`, then merges the generated bottle metadata. The
`flow-full` formula is a meta-package that depends on the three installable
role formulae.

## CI Dry Runs

Push a branch named `release-ci` or under `release-ci/`, or run the `release`
workflow manually, to exercise the release workflow without publishing. Dry-run
builds use version `v0.0.0` and the pushed commit archive as the Homebrew source.
They build and upload the normal workflow artifacts, generate checksums, build
and smoke-test both Docker images, build Homebrew bottles from a local tap, and
dry-run the Homebrew formula merge.

Dry runs do not publish a GitHub release, push Docker images, push to the
Homebrew tap, or create artifact attestations. The macOS `.pkg` is built
unsigned in dry runs so Apple Developer ID and notarization secrets are only
required for real `v*` tag releases.

## Tagging

Create release tags with:

```sh
make release VERSION=patch
make release VERSION=minor
make release VERSION=major
make release VERSION=1.2.3
make release VERSION=patch AUTOPUSH=1
```

`patch`, `minor`, and `major` are computed from the latest `vX.Y.Z` git tag.
`patch` starts at `v0.0.1` when no prior tag exists. The target requires a clean
worktree, runs `go build ./...`, `go vet ./...`, and `go test ./...`, then
creates an annotated `vX.Y.Z` tag. `AUTOPUSH=1` pushes the tag to `origin`.

## Required Secrets And Variables

- `MACOS_DEVELOPER_ID_APPLICATION_P12_BASE64`: base64 of a `.p12` exported from
  Certificates, Identifiers & Profiles -> Certificates -> **Developer ID
  Application**. Export it with the private key from Keychain Access.
- `MACOS_DEVELOPER_ID_APPLICATION_PASSWORD`: password used when exporting that
  Application `.p12`.
- `MACOS_DEVELOPER_ID_INSTALLER_P12_BASE64`: base64 of a `.p12` exported from
  Certificates, Identifiers & Profiles -> Certificates -> **Developer ID
  Installer**.
- `MACOS_DEVELOPER_ID_INSTALLER_PASSWORD`: password used when exporting that
  Installer `.p12`.
- `APPLE_TEAM_ID`: the Apple Developer Team ID, visible in the developer account
  membership page and in Developer ID certificate subjects.
- `APPLE_NOTARY_KEY_ID`, `APPLE_NOTARY_ISSUER_ID`,
  `APPLE_NOTARY_KEY_P8_BASE64`: an **App Store Connect API key** for
  notarization. This is created in App Store Connect under Users and Access ->
  Integrations -> App Store Connect API, not in the
  Certificates/Identifiers/Profiles certificate list. Download the `.p8` key
  once and base64 it for the secret.
- `MACOS_DEVELOPER_ID_APPLICATION_IDENTITY` and
  `MACOS_DEVELOPER_ID_INSTALLER_IDENTITY` are optional override secrets for the
  exact certificate common names if automatic identity discovery is ambiguous.
- `HOMEBREW_TAP_APP_PRIVATE_KEY`: private key for the GitHub App installed on
  `ClarifiedLabs/homebrew-tap`.
- `HOMEBREW_TAP_APP_CLIENT_ID`: the GitHub App Client ID.

The GitHub App only needs to be installed on `ClarifiedLabs/homebrew-tap` with
repository Contents read/write permission. No Apple provisioning profile is used
for this Developer ID CLI/pkg distribution flow.
