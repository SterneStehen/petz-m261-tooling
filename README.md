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
