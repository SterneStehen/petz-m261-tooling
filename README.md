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
  it). Triggers on every push to `main`, every pushed `v*` tag (`git tag
  v1.2.3 && git push --tags`), or manual dispatch (Actions tab → Release
  → Run workflow), and only runs `build-and-push` after the `ci.yml` test
  gate passes. A successful `main` push publishes exactly one immutable
  test image tag: `sha-<full-commit-sha>`. A pushed `v*` tag publishes
  the immutable release tag (`1.2.3`), the existing semver companion tag
  (`1.2`), and `latest` only for a stable non-prerelease tag. Manual
  dispatch requires both an immutable source ref (40-character commit SHA
  or `refs/tags/vX.Y.Z`) and an explicit immutable image tag; it never
  auto-publishes `latest`. The pushed image carries OCI labels, Buildx GHA
  cache metadata, and a signed build-provenance attestation (viewable via
  `gh attestation verify` or the Packages page). The GHCR package is
  public.

- **`deploy.yml`** — manual-only (`workflow_dispatch`) and bound only to
  the GitHub Environment named **`dev`**. It runs exclusively on the
  self-hosted runner labels `[self-hosted, linux, m261-dev]`, which is
  installed directly on the Linux server that already hosts Docker
  Compose. The only input is `image_tag`, and the workflow rejects
  anything except `sha-<40 lowercase hex>` or a stable release tag
  `X.Y.Z`; it refuses `latest`, prereleases, arbitrary refs, and malformed
  tags. Before touching deployment state it verifies that
  `ghcr.io/<owner>/<repo>:<image_tag>` actually exists in public GHCR.
  Deployment happens locally in `/opt/m261sim`: it writes
  `M261SIM_IMAGE=ghcr.io/...:<image_tag>` into `.env`, runs `docker
  compose pull`, then `docker compose up -d --remove-orphans`, and calls
  the repository's `simulator/cmd/deploy-probe` checker.

  That probe must pass all of the following before the deploy is accepted:
  the existing Docker Compose container healthcheck, `GET
  http://127.0.0.1:8081/api/v1/health/ready` returns `200` with
  `{"status":"ready"}`, a real read-only Modbus TCP FC04 request to
  `127.0.0.1:502` (unit 34, address 2, count 2) returns a valid
  non-exception response, and a real IEC-104 session to `127.0.0.1:2404`
  completes `STARTDT_ACT`/`STARTDT_CON` plus a `C_IC_NA_1` general
  interrogation with valid activation, data, and termination frames. A
  successful deploy then updates `/opt/m261sim/.deploy-state` with the
  deployed image reference as the new previous known-good state. On any
  verification failure, the workflow restores the image recorded in
  `.deploy-state`, reruns Compose, rechecks with the same full probe, and
  still fails the workflow even if rollback succeeds.

### Required repository settings

- **GitHub Environment** named exactly `dev` (Settings → Environments →
  New environment) — `deploy.yml` targets it by name. Creating it is what
  makes environment-scoped rules and optional required reviewers
  available; the workflow file itself cannot create Environments. Any
  repository writer may manually run the deploy workflow against `dev`.
- **Self-hosted runner** installed on the Docker Compose server itself,
  with labels exactly `self-hosted`, `linux`, and `m261-dev`.
- **Deployment directory** `/opt/m261sim` already present on that host,
  containing this project's `docker-compose.yml` (or an equivalent using
  the same `M261SIM_IMAGE` variable).
- **GHCR package visibility**: `ghcr.io/<owner>/<repo>` is public, so the
  deployment host can pull images without deployment credentials or a
  private-package workaround.
- **Dependency Graph** must be enabled (Settings → Code security → Dependency graph) for `dependency-review.yml` to have anything to diff — on by default for a public repository like this one.

### Manual deploy

```sh
gh workflow run deploy.yml -f image_tag=sha-0451d99c00000000000000000000000000000000
```

Or, for a stable release tag already published by `release.yml`:

```sh
gh workflow run deploy.yml -f image_tag=1.2.3
```

CD assumes the self-hosted runner is already on the Docker Compose host,
with Docker and Compose installed and `/opt/m261sim` already provisioned.
Provisioning that host itself is outside this repository's scope; no
Kubernetes, cloud-provider, Terraform, or Helm bootstrap automation is
included here.

### Private register-map CI note

`ci.yml` intentionally does **not** implement any secret-based retrieval
of the private `m261-registermap` input. The generate/validate gate stays
opt-in behind `HAS_REGISTERMAP == true`, and the secret name plus encoding
contract for any future CI provisioning step still needs to be defined
separately before that can be automated safely.
