# petz-m261-tooling

Canonical ENERGRID M261 point catalog, code generator, and simulator
(IEC-104 and Modbus TCP servers backed by a battery physics model).

The manufacturer register maps and internal specifications are intentionally
local-only and are not included in this repository. Obtain them through the
project's approved internal channel before generating a catalog.

Running the simulator (launch, ports, connecting a client) is documented
separately in **[docs/README.md](docs/README.md)** — this file covers the
repository and its build pipeline. `make generate` also produces a full
point-by-point reference and a list of manufacturer open questions
locally, but — like `catalog/point_catalog.json` itself — neither is part
of this repository: both are derived directly from the private register
maps and stay local-only artifacts (see `.gitignore`).

## Prerequisites

- Python 3.11+ with `pip` and `venv` (for `catalog/` and `codegen/`)
- Go 1.22+ (for `simulator/`)
- Docker (for `docker compose up` — see [docs/README.md](docs/README.md))

## Common commands

```text
make generate   # build catalog and generated models (needs the private register maps)
make build      # build the simulator -> simulator/bin/m261sim
make test       # go vet + go test -race + the Python test suite
make validate   # regenerate from scratch and fail if anything differs from what's committed
make run        # run the simulator locally
```

`make generate` needs the manufacturer register maps, which this
repository does not include — as does most of the Python test suite
`make test` runs, since it exercises that same pipeline; those specific
tests skip themselves cleanly when the maps are absent (see
`tests/conftest.py`) rather than failing. `make build`/`make run`, and the
Go half of `make test` (`go vet`/`go test -race`), never need the maps at
all: the simulator only imports the already-generated `gen/go/m261points`
package, which is committed like any other source file.

## CI/CD

Three workflows under `.github/workflows/`:

- **`ci.yml`** — runs on every push to `main` and every pull request:
  `gofmt`/`go vet`/`go build`/`go test -race`, the Python test suite, the
  Web UI build plus its Playwright browser tests, and a Docker build
  smoke test. `mypy` runs as its own explicitly non-blocking
  (`continue-on-error: true`) job — see that job's own comment for why.
  A fifth job, `make generate (no diff)`, stays skipped unless a
  maintainer with access to the private manufacturer register maps sets
  the repository variable `HAS_REGISTERMAP` to `true` and adds their own
  step to provision `m261-registermap/` first (see the job's comment in
  the workflow file). `ci.yml` also declares a `workflow_call:` trigger
  so `release.yml` can reuse it as a test gate instead of duplicating its
  steps. A pull request additionally runs **`dependency-review.yml`**,
  which fails the check on any newly-introduced dependency with a
  moderate-or-higher severity advisory (needs the repository's Dependency
  Graph, on by default for a public repository).

- **`release.yml`** — publishes a container image to GitHub Container
  Registry (GHCR) as `ghcr.io/<owner>/<repo>` (lowercased; GHCR requires
  it). Triggers on pushing a tag matching `v*` (`git tag v1.2.3 && git
  push --tags`) or manual dispatch (Actions tab → Release → Run
  workflow), and only runs `build-and-push` after the `ci.yml` test gate
  passes. Every build gets an immutable `sha-<commit>` tag; a `v*` tag
  push additionally gets semver tags (`1.2.3`, `1.2`) and, only for a
  stable (non-prerelease, e.g. not `1.2.3-rc1`) tag, `latest`. The pushed
  image carries OCI labels and a signed build-provenance attestation
  (viewable via `gh attestation verify` or the Packages page).

- **`deploy.yml`** — manual-only (`workflow_dispatch`), with two inputs:
  `environment` (`staging` or `production`) and `image_tag` (an
  already-published tag from the list above — check the Packages page).
  It verifies that tag actually exists in GHCR, then rolls it out over
  SSH: writes `M261SIM_IMAGE=ghcr.io/.../<tag>` into `.env` at
  `DEPLOY_PATH` on the target host, runs `docker compose pull` and
  `docker compose up -d --remove-orphans`, and polls the service's
  existing Compose healthcheck for up to 60s. If it never reports
  healthy, the workflow automatically rewrites `.env` back to whichever
  tag was running before, redeploys that, and still fails the job — so a
  bad deploy self-heals but is never silently swallowed.

### Required repository settings

- **GitHub Environments** named exactly `staging` and `production`
  (Settings → Environments → New environment) — `deploy.yml` targets
  these by name. Creating them is what makes environment-scoped secrets
  and (optionally) required reviewers available; this repository's
  workflow files don't and can't create Environments themselves. On each
  one:
  - Add secrets `DEPLOY_HOST`, `DEPLOY_USER`, `DEPLOY_SSH_KEY` (the
    private key matching a `authorized_keys` entry on the host for
    `DEPLOY_USER`), and `DEPLOY_PATH` (an absolute path on the host
    containing a `docker-compose.yml` derived from this repository's own
    — with the `image:`/`M261SIM_IMAGE` pair from this change — and
    already reachable over SSH).
  - Set that environment's **Deployment branches and tags** rule (in the
    same environment settings page) to whatever this project considers
    safe to deploy from — e.g. tags matching `v*` for `production`. This
    is the actual guardrail against deploying an arbitrary branch commit;
    `deploy.yml` itself only checks that the requested image tag was
    genuinely published, not which ref triggered the run.
  - A required-reviewers rule on `production` (same page) gets you a
    manual approval gate before the job runs, for free.
- **GHCR package visibility**: the first `release.yml` run creates the
  `ghcr.io/<owner>/<repo>` package, defaulting to the parent repository's
  own visibility. If it ends up private and the deploy target needs to
  pull it without the workaround `deploy.yml` already does for its own
  pre-flight check, run `docker login ghcr.io` on the host once with a
  token that has `read:packages`.
- **Dependency Graph** must be enabled (Settings → Code security → Dependency graph) for `dependency-review.yml` to have anything to diff — on by default for a public repository like this one.

### Manual deploy

```sh
gh workflow run deploy.yml -f environment=staging -f image_tag=1.2.3
```

CD needs a real, reachable target host reachable over SSH with Docker and
Compose already installed and this project's `docker-compose.yml` (or an
equivalent using the same `M261SIM_IMAGE` variable) already in place at
`DEPLOY_PATH` — provisioning that host itself is outside this repository's
scope; no Kubernetes, cloud-provider, or Terraform/Helm tooling is assumed
or included here.
