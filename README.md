# Infra CI

Public [build workflow](.github/workflows/build.yml) for
[nix-ci-worker](https://github.com/awked-com/nix-ci-worker).

## Configure and run

Grant this repository's Actions permission to write packages and **Write** access
under the GHCR package's **Manage Actions access** settings. Configure:

| Secret | Purpose |
| --- | --- |
| `CI_SOURCE_REPOSITORY` | Private source repository in `OWNER/NAME` form |
| `CI_IDENTITY` | Age identity for encrypted inputs |
| `CI_RECIPIENTS` | Age recipients for encrypted outputs |
| `CI_STORAGE` | GHCR package configuration |
| `NIX_SIGNING_KEY` | Final Nix cache signing key, coordinators only |

Create the organization-owned GitHub App `awked-infra-ci`, installed **only** on
the private source repository, with **Contents: write**, **Pull requests: write**,
and **Checks: write**. Set its client ID in the `INFRA_APP_CLIENT_ID` repository
variable. Create the `infra-automation` environment, restrict it to `main`, and
set its `INFRA_APP_PRIVATE_KEY` secret. `infra ci sync` manages the build secrets
above; configure the App separately.

## Builds

Dispatch [build.yml](.github/workflows/build.yml) through GitHub Actions.
Admission resolves the source ref to a commit; rerunning admission resolves it
again. Retry all build jobs together for a new helper pool. A coordinator retried
alone can finish locally.

Keep the worker pin independent of the source revision and Nix compatible with
its derivation JSON schema. The launcher supplies job-scoped Actions cache
credentials. Helpers must never receive the final signing key.

Workflow files, source refs, selections, and logs are public. GHCR payloads and
Actions coordination messages are encrypted. Never expose private source,
secrets, diagnostics, or source-derived details in this repository or its output.
Build subprocesses receive neither the App key nor write tokens.

## Private source maintenance

[Maintenance](.github/workflows/infra-maintenance.yml) checks private `main` daily
at 03:17 UTC and updates Nix inputs Mondays at 03:43 UTC. Manual runs accept
`check` or `update`. It verifies the App's single-repository scope before use.
Checks run Go tests, vet, build, and `nix flake check` with private diagnostics
suppressed; reproduce failures locally. A private check records the commit's
result.

Updates create a `codex/nix-inputs-*` branch only when the lockfile changes and
no earlier update request is open. A separate runner validates it before opening
a private pull request, including on validation failure. Review failed requests
and delete their branches manually. Maintenance never merges or deploys; package
pins and vendor hashes need separate updates. Other private pull requests do not
trigger CI.

Read tokens are scoped to clone steps; branch-write tokens are issued after the
refresh. Publication jobs never execute private source. Do not pass private pull
request identifiers through the public build inputs.

## Repository checks and updates

With Go and Node.js installed, run `go test -race ./...` and `go vet ./...`.
[Repository CI](.github/workflows/ci.yml) also runs actionlint; it needs no private
credentials. [Dependabot](.github/dependabot.yml) checks dependencies weekly.
