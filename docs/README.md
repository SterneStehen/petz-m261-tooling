# Running the ENERGRID M261 simulator

This is the operator guide: how to start the simulator, which ports it
listens on, and how to connect a client to each of them. For what this
repository is and how the point catalog/codegen pipeline works, see the
[repository README](../README.md).

The full point-by-point reference (every address, type, range, and
setpoint) is generated locally by `make generate` but is **not** part of
this repository — it's derived directly from the manufacturer register
map, which is private/internal material (see the repository README).
Ask a maintainer with access to the register maps for the current
reference document if you need one.

## Quick start: Docker Compose

```sh
docker compose up
```

This builds and starts one container running `m261sim` with its default
configuration ([`simulator/config/m261sim.yaml`](../simulator/config/m261sim.yaml))
and the bundled [`scenarios/`](../scenarios/). Ports (below) are published
to the host as-is.

Bring it down with `docker compose down` (add `--volumes` if you want a
clean slate — the simulator itself keeps no state on disk between runs;
`POST /reset` gives you the same clean slate without restarting anything).

## Quick start: locally, without Docker

```sh
make build   # go build -> simulator/bin/m261sim
make run     # go run ./simulator/cmd/m261sim (equivalent, no separate binary)
```

Requires Go 1.22+ (see the repository README's own prerequisites) and
`gen/go/m261points` already generated — it's committed, so a plain
`git clone` already has it; you only need `make generate` again if
you're regenerating the catalog from the manufacturer register maps
yourself (private input, not part of this repository).

## Ports

| Port | Protocol | Default bind | Purpose |
|---|---|---|---|
| 502 | Modbus TCP | `:502` (all interfaces) | Functions 02/03/04/06/16 against the shared point store. |
| 2404 | IEC-104 | `:2404` (all interfaces) | Station/general interrogation, spontaneous transmission, single/setpoint commands. |
| 8081 | Control API (HTTP/JSON) | `127.0.0.1:8081` (loopback only) | Fault injection, link-fault simulation, scenario playback, clock control, reset — simulator-only, no equivalent on the real M261. |

The control API binds to loopback only by default, deliberately — it is
an operational/testing surface with no equivalent on the real M261, and
its endpoints (`/reset`, `/faults`, `/link`, scenario control) are not
something any host on the network should be able to call. The bundled
`docker-compose.yml` overrides the *container's* own bind to `0.0.0.0:8081`
(via the `-control-addr` flag, not by changing the code's own default —
needed since Docker's port forwarding doesn't arrive as a literal
loopback connection) but publishes that port to the host as
`127.0.0.1:8081` specifically so it stays loopback-only end to end — see
that file's own comment. Running the binary directly, the default stays
loopback-only unless you pass `-control-addr` yourself.

Every listen address is a flag or a config value — see `m261sim -h` for
the full list (`-modbus-addr`, `-iec104-addr`, `-control-addr`,
`-config`, `-scenarios-dir`, `-physics-step`, `-speed`, `-initial-soc`,
`-modbus-byte-order`).

## Connecting a client

### Modbus TCP

Any standard Modbus TCP master works against `<host>:502`. Unit IDs are
per-device addresses (the private point reference — see above — has the
address and register/function for every point). Example: BMS (unit 34)
`soc` is an F32 (2 registers) input register at Modbus address 30003
(function 04), 0-based wire address 2. With `mbpoll`:

```sh
mbpoll -m tcp -a 34 -t 4 -r 3 -c 2 <host>       # BMS (unit 34) SoC, 2 input registers from 30003
```

Or with `pymodbus`:

```python
from pymodbus.client import ModbusTcpClient

client = ModbusTcpClient("<host>", port=502)
client.connect()
result = client.read_input_registers(address=2, count=2, slave=34)  # BMS unit 34, SoC (F32)
print(result.registers)
```

### IEC-104

Any IEC 60870-5-104 master (e.g. `lib60870`, `iec104-python`) can connect
to `<host>:2404`, send `STARTDT_ACT`, and issue a general interrogation
(`C_IC_NA_1`) to read every point's current value, or a single/setpoint
command (`C_SC_NA_1`/`C_SE_NC_1`) to write a setpoint. Point addresses
(ASDU common address + information object address) are in the private
point reference — see above.

### Control API

Plain JSON over HTTP, reachable at `http://127.0.0.1:8081` by default (or
wherever `-control-addr`/`docker-compose.yml` points it). A few examples:

```sh
# Every point's current value
curl http://127.0.0.1:8081/state

# Inject an alarm
curl -X POST http://127.0.0.1:8081/faults -H 'Content-Type: application/json' \
  -d '{"device": "BMS", "point": "cell_temperature_too_high", "value": 1}'

# Drop the IEC-104 link
curl -X POST http://127.0.0.1:8081/link -H 'Content-Type: application/json' \
  -d '{"protocol": "iec104", "mode": "drop"}'

# Load and start a bundled scenario
curl -X POST http://127.0.0.1:8081/scenario/load -H 'Content-Type: application/json' -d '{"name": "restart.yaml"}'
curl -X POST http://127.0.0.1:8081/scenario/start

# Reset everything to the state right after process start
curl -X POST http://127.0.0.1:8081/reset
```

Every endpoint returns `204` on success or a JSON `{"error": {"code",
"message"}}` body on failure. `GET /state` accepts an optional
`?device=<device>` filter.

## Configuration

[`simulator/config/m261sim.yaml`](../simulator/config/m261sim.yaml) holds
every parameter the manufacturer hasn't confirmed yet (Modbus byte order,
watchdog behavior, mode-arbitration priority, whether dangerous commands
are accepted) — each one carries its own `unconfirmed: true` marker and a
documented default. The specific, itemized questions these defaults are
standing in for are generated locally by `make generate`
(`catalog.validate_catalog`) but, like the point reference above, are not
part of this repository — see the repository README.

## Development

```sh
make generate   # rebuild the catalog and generated code (needs the private register maps)
make build      # go build -> simulator/bin/m261sim
make test       # go vet + go test -race + the Python test suite
make validate   # regenerate from scratch and fail if anything differs from what's committed
make run        # run the simulator locally
```

See the [repository README](../README.md) for what `make generate` needs
and why most of it can't run without the (private, not included)
manufacturer register maps.
