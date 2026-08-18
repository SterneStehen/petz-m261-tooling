# syntax=docker/dockerfile:1
#
# Builds and runs the M261 simulator only — Task 8's "run the simulator
# with one command" deliverable, not a catalog/codegen build environment.
# The build stage never touches catalog/, the Python toolchain, or the
# private manufacturer register maps: gen/go/m261points (the generated
# catalog code the simulator imports) is committed to this repository like
# any other Go source file, so `go build` alone is enough. See
# docs/README.md.

# --- build stage -------------------------------------------------------
FROM golang:1.24-alpine AS builder
WORKDIR /src

# Dependencies first, cached separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY gen/ gen/
COPY simulator/ simulator/
COPY webui/embed.go webui/embed.go
COPY webui/dist/ webui/dist/
RUN CGO_ENABLED=0 go build -trimpath -o /out/m261sim ./simulator/cmd/m261sim

# --- runtime stage -------------------------------------------------------
FROM alpine:3.20
RUN addgroup -S m261sim && adduser -S -G m261sim m261sim
WORKDIR /app

COPY --from=builder /out/m261sim ./m261sim
COPY simulator/config/m261sim.yaml ./simulator/config/m261sim.yaml
COPY scenarios/ ./scenarios/

USER m261sim

# 502 Modbus TCP, 2404 IEC-104, 8081 control API (see docs/README.md for
# the full port table and why the control API's *default* bind stays
# loopback-only — docker-compose.yml is what actually publishes it here).
EXPOSE 502 2404 8081

ENTRYPOINT ["./m261sim"]
CMD ["-config=simulator/config/m261sim.yaml", "-scenarios-dir=scenarios"]
