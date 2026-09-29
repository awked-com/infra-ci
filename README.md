# Infra CI workflow

Public [GitHub Actions workflow](.github/workflows/build.yml) for
[nix-ci-worker](https://github.com/awked-com/nix-ci-worker).

## Configure and run

Grant this repository's Actions permission to read Actions metadata and write
packages. The GHCR package also needs **Admin** access under **Manage Actions
access**. Configure these Actions secrets:

| Secret | Purpose |
| --- | --- |
| `CI_SOURCE_REPOSITORY` | Private source repository in `OWNER/NAME` form |
| `CI_DEPLOY_KEY` | Read-only SSH deploy key for that source |
| `CI_IDENTITY` | Age identity for encrypted inputs |
| `CI_RECIPIENTS` | Age recipients for encrypted outputs |
| `CI_STORAGE` | GHCR package configuration |
| `NIX_SIGNING_KEY` | Final Nix cache signing key, coordinators only |

Dispatch [build.yml](.github/workflows/build.yml) through GitHub Actions.
Admission resolves the source to a commit. Retry all build jobs together for a
new helper pool; a coordinator retried alone can finish locally. Rerunning
admission may resolve its source ref again; retrying only later jobs retains the
admitted commit.

The worker is compiled from the source module's pinned dependency. Keep the Nix
version compatible with its derivation JSON schema. Launch it through the
JavaScript action for job-scoped Actions cache credentials. Helpers must never
receive the final cache signing key.

Workflow files, source refs, selections, and Actions logs are public. GHCR
results and cache payloads and Actions coordination messages are encrypted.
Never add source contents, secrets, build diagnostics, or source-derived details
to this repository, logs, artifacts, or annotations.
