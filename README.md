# Infra CI

Manually dispatched public [build cache workflow](.github/workflows/build.yml) for
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
the private source repository, with **Contents: read**. Set its client ID in the
`INFRA_APP_CLIENT_ID` repository variable. Create the `infra-automation`
environment, restrict it to `main`, and set its `INFRA_APP_PRIVATE_KEY` secret.
`infra ci sync` manages the build secrets above; configure the App separately.

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
Build subprocesses receive neither the App key nor write tokens. Raw compiler
and worker diagnostics stay in mode-0600 files under `RUNNER_TEMP`; they are
not uploaded as artifacts.

## Local checks

With Go and Node.js installed, run:

```sh
go test -race ./...
go vet ./...
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 -ignore 'unexpected key "queue" for "concurrency" section'
```
