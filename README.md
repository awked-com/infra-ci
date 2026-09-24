# Infra CI workflow

This repository contains the public
[GitHub Actions workflow](.github/workflows/build.yml) for
[nix-ci-worker](https://github.com/awked-com/nix-ci-worker). Edit and push the
workflow directly in this repository.

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

Dispatch `build.yml` through GitHub Actions with `request` (32 lowercase hex
characters) and `source` (branch, tag, or commit).
Optional `host` and `package` select a host system or package attribute; an empty
selection builds all configured targets. Admission resolves the source to a
commit and emits coordinator and helper matrices. Each platform runs one
coordinator and two helpers. Retry all build jobs together for a new helper pool;
a coordinator retried alone can finish locally. Rerunning admission may resolve
its source ref again; retrying only later jobs retains the admitted commit.

The workflow pins its GitHub actions, Go toolchain, and Nix installer. It
compiles the worker from the checked-out source module's pinned dependency.
Keep the Nix version compatible with the worker's derivation JSON schema.
Linux build jobs configure 16 GiB swap before checkout; admission and macOS
skip swap setup. The workflow removes checkout credentials, disables the public
Go Actions cache, and writes private compiler diagnostics only to a runner-local
file. Worker steps receive automatic job-scoped Actions cache credentials from
the JavaScript action. Helpers never receive the final cache signing key.

Workflow files, source refs, selections, and Actions logs are public. GHCR
results and cache payloads and Actions coordination messages are encrypted.
Never add source contents, secrets, build diagnostics, or source-derived details
to this repository, logs, artifacts, or annotations.
