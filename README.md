# petz-m261-tooling

Canonical ENERGRID M261 point catalog, code generator, and simulator
(IEC-104 and Modbus TCP servers backed by a battery physics model).

The manufacturer register maps and internal specifications are intentionally
local-only and are not included in this repository. Obtain them through the
project's approved internal channel before generating a catalog.

## Prerequisites

- Python 3.11+ with `pip` and `venv` (for `catalog/` and `codegen/`)
- Go 1.22+ (for `simulator/`)
- Docker (for `docker compose up`, once packaging is implemented)

## Common commands

```text
make generate   # build catalog and generated models
make build      # build the simulator
make test       # run tests
make run        # run the simulator locally
```
