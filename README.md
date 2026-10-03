# Infra CI

Public [build workflow](.github/workflows/build.yml) for
[nix-ci-worker](https://github.com/awked-com/nix-ci-worker).

## Repository checks and updates

[Repository CI](.github/workflows/ci.yml) tests the workflow launcher, vets the
Go module, and validates workflows on pushes to `main` and pull requests. It
needs no private credentials. [Dependabot](.github/dependabot.yml) opens weekly
GitHub Actions and Go module update requests for this repository.

## Configure and run

Grant this repository’s Actions permission to write packages and grant the
workflow repository **Write** access under the GHCR package’s
**Manage Actions access** settings. Configure these Actions secrets:

| Secret | Purpose |
| --- | --- |
| `CI_SOURCE_REPOSITORY` | Private source repository in `OWNER/NAME` form |
| `CI_IDENTITY` | Age identity for encrypted inputs |
| `CI_RECIPIENTS` | Age recipients for encrypted outputs |
| `CI_STORAGE` | GHCR package configuration |
| `NIX_SIGNING_KEY` | Final Nix cache signing key, coordinators only |

Create the organization-owned GitHub App `awked-infra-ci`, installed **only** on
the private source repository, with repository permissions **Contents: write**,
**Pull requests: write**, and **Checks: write**. Set the
`INFRA_APP_CLIENT_ID` repository variable to its client ID. Use the
`infra-automation` environment, restricted to `main`, and set its
`INFRA_APP_PRIVATE_KEY` environment secret to the App's private key.
`infra ci sync` manages the build secrets in the table; configure the App
variable and environment secret separately. Builds request short-lived
Contents: read tokens for source checkout. The App key and write tokens are not
passed to build subprocesses.

Dispatch [build.yml](.github/workflows/build.yml) through GitHub Actions.
Admission resolves the source to a commit. Retry all build jobs together for a
new helper pool; a coordinator retried alone can finish locally. Rerunning
admission resolves its source ref again; later jobs retain the admitted commit.

Keep the worker pin independent of the source revision and Nix compatible with
the worker’s derivation JSON schema. The JavaScript launcher supplies job-scoped
Actions cache credentials. Helpers must never receive the final signing key.

Workflow files, source refs, selections, and Actions logs are public. GHCR
results and cache payloads and Actions coordination messages are encrypted.
Never add source contents, secrets, build diagnostics, or source-derived details
to this repository, logs, artifacts, or annotations.

## Private source maintenance

[maintenance workflow](.github/workflows/infra-maintenance.yml) requests
short-lived installation tokens. It verifies that the App installation contains
exactly the configured source repository before using a token. Missing App
access fails before an update branch is created.

Maintenance checks the private source's `main` daily at 03:17 UTC. It runs Go
tests, vet, and build, then `nix flake check`. At 03:43 UTC on Mondays it runs
`nix flake update` and the same checks. Run the workflow manually with `check`
or `update` for an extra run. The update creates a `codex/nix-inputs-*` branch
only when the lock file changes and no earlier automated input update request
is open. A separate runner validates that branch before a private pull request
opens. Failed validation leaves a failing private pull request and branch for
inspection; close the request and delete its branch manually after review. The
workflow never merges or deploys.
Package recipe pins and vendor hashes need separate review and updates.

Private check output stays in temporary runner files and is discarded after
the run. Public logs show generic failures; reproduce the checks locally for
diagnostics. A GitHub App check on the exact private commit records the result.
In maintenance jobs, Contents: read tokens are passed only to Git clone
steps. A Contents: write token is created after the input refresh and used only
for its branch push. Pull request and check tokens are used in jobs that do not
execute private source code. The build workflow exposes its source ref and
selections as public dispatch inputs, so do not use it to pass private pull
request identifiers.
Maintenance checks `main` and its own update branches. Other private pull
requests do not yet trigger CI; they need a signed webhook relay or a
privacy-reviewed polling design.
