package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/iec104"
	"github.com/goburrow/modbus"
)

type config struct {
	composeDir     string
	service        string
	readyURL       string
	modbusAddr     string
	modbusUnitID   byte
	modbusAddress  uint16
	modbusCount    uint16
	iec104Addr     string
	iec104Common   uint16
	composeTimeout time.Duration
	httpTimeout    time.Duration
	modbusTimeout  time.Duration
	iec104Timeout  time.Duration
	runCommand     commandRunner
}

type commandRunner func(ctx context.Context, dir string, name string, args ...string) ([]byte, error)

func main() {
	cfg := config{
		composeDir:     "/opt/m261sim",
		service:        "m261sim",
		readyURL:       "http://127.0.0.1:8081/api/v1/health/ready",
		modbusAddr:     "127.0.0.1:502",
		modbusUnitID:   34,
		modbusAddress:  2,
		modbusCount:    2,
		iec104Addr:     "127.0.0.1:2404",
		iec104Common:   1,
		composeTimeout: 90 * time.Second,
		httpTimeout:    5 * time.Second,
		modbusTimeout:  5 * time.Second,
		iec104Timeout:  10 * time.Second,
		runCommand:     defaultCommandRunner,
	}
	var modbusUnitID, modbusAddress, modbusCount, iec104Common uint

	flag.StringVar(&cfg.composeDir, "compose-dir", cfg.composeDir, "directory containing docker-compose.yml")
	flag.StringVar(&cfg.service, "service", cfg.service, "docker compose service name to verify")
	flag.StringVar(&cfg.readyURL, "ready-url", cfg.readyURL, "HTTP readiness URL")
	flag.StringVar(&cfg.modbusAddr, "modbus-addr", cfg.modbusAddr, "Modbus TCP address")
	flag.UintVar(&modbusUnitID, "modbus-unit-id", uint(cfg.modbusUnitID), "Modbus unit ID")
	flag.UintVar(&modbusAddress, "modbus-address", uint(cfg.modbusAddress), "Modbus input register address")
	flag.UintVar(&modbusCount, "modbus-count", uint(cfg.modbusCount), "number of Modbus input registers to read")
	flag.StringVar(&cfg.iec104Addr, "iec104-addr", cfg.iec104Addr, "IEC-104 TCP address")
	flag.UintVar(&iec104Common, "iec104-common-address", uint(cfg.iec104Common), "IEC-104 common address")
	flag.DurationVar(&cfg.composeTimeout, "compose-timeout", cfg.composeTimeout, "timeout for Docker Compose health")
	flag.DurationVar(&cfg.httpTimeout, "http-timeout", cfg.httpTimeout, "timeout for HTTP readiness probe")
	flag.DurationVar(&cfg.modbusTimeout, "modbus-timeout", cfg.modbusTimeout, "timeout for Modbus probe")
	flag.DurationVar(&cfg.iec104Timeout, "iec104-timeout", cfg.iec104Timeout, "timeout for IEC-104 probe")
	flag.Parse()
	if modbusUnitID > 255 || modbusAddress > 65535 || modbusCount == 0 || modbusCount > 65535 || iec104Common > 65535 {
		log.Fatal("Modbus unit ID must fit in 8 bits; Modbus address/count and IEC-104 common address must fit in 16 bits; Modbus count must be non-zero")
	}
	cfg.modbusUnitID = byte(modbusUnitID)
	cfg.modbusAddress = uint16(modbusAddress)
	cfg.modbusCount = uint16(modbusCount)
	cfg.iec104Common = uint16(iec104Common)

	if err := run(context.Background(), cfg); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, cfg config) error {
	if cfg.runCommand == nil {
		cfg.runCommand = defaultCommandRunner
	}
	if err := waitForComposeHealthy(ctx, cfg); err != nil {
		return err
	}
	if err := probeHTTPReady(ctx, cfg.readyURL, cfg.httpTimeout); err != nil {
		return err
	}
	if err := probeModbus(ctx, cfg.modbusAddr, cfg.modbusUnitID, cfg.modbusAddress, cfg.modbusCount, cfg.modbusTimeout); err != nil {
		return err
	}
	if err := probeIEC104(ctx, cfg.iec104Addr, cfg.iec104Common, cfg.iec104Timeout); err != nil {
		return err
	}
	return nil
}

func waitForComposeHealthy(ctx context.Context, cfg config) error {
	deadline := time.Now().Add(cfg.composeTimeout)
	lastStatus := "unknown"
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("compose health check timed out for service %q after %s (last status: %s)", cfg.service, cfg.composeTimeout, lastStatus)
		}

		containerID, err := composeContainerID(ctx, cfg)
		if err != nil {
			lastStatus = err.Error()
		} else {
			status, err := inspectContainerHealth(ctx, cfg, containerID)
			if err != nil {
				lastStatus = err.Error()
			} else {
				lastStatus = status
				if status == "running/healthy" {
					return nil
				}
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("compose health check canceled: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

func composeContainerID(ctx context.Context, cfg config) (string, error) {
	out, err := cfg.runCommand(ctx, cfg.composeDir, "docker", "compose", "ps", "-q", cfg.service)
	if err != nil {
		return "", fmt.Errorf("docker compose ps -q %s: %w", cfg.service, err)
	}
	containerID := strings.TrimSpace(string(out))
	if containerID == "" {
		return "", fmt.Errorf("docker compose ps -q %s returned no container id", cfg.service)
	}
	return containerID, nil
}

func inspectContainerHealth(ctx context.Context, cfg config, containerID string) (string, error) {
	out, err := cfg.runCommand(
		ctx,
		cfg.composeDir,
		"docker",
		"inspect",
		"--format",
		"{{.State.Status}}/{{if .State.Health}}{{.State.Health.Status}}{{else}}no-healthcheck{{end}}",
		containerID,
	)
	if err != nil {
		return "", fmt.Errorf("docker inspect %s: %w", containerID, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func probeHTTPReady(ctx context.Context, readyURL string, timeout time.Duration) error {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, readyURL, nil)
	if err != nil {
		return fmt.Errorf("http readiness request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http readiness probe %s: %w", readyURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("http readiness probe %s: read body: %w", readyURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http readiness probe %s: status %d, body %s", readyURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("http readiness probe %s: decode body: %w", readyURL, err)
	}
	if payload.Status != "ready" {
		return fmt.Errorf("http readiness probe %s: expected JSON status %q, got %q", readyURL, "ready", payload.Status)
	}
	return nil
}

func probeModbus(ctx context.Context, addr string, unitID byte, address, count uint16, timeout time.Duration) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("modbus probe %s: %w", addr, err)
	}
	handler := modbus.NewTCPClientHandler(addr)
	handler.Timeout = timeout
	handler.IdleTimeout = timeout
	handler.SlaveId = unitID
	defer handler.Close()

	registers, err := modbus.NewClient(handler).ReadInputRegisters(address, count)
	if err != nil {
		return fmt.Errorf("modbus probe %s: FC04 read unit %d address %d count %d: %w", addr, unitID, address, count, err)
	}
	if len(registers) != int(count)*2 {
		return fmt.Errorf("modbus probe %s: FC04 returned %d bytes, want %d", addr, len(registers), int(count)*2)
	}
	return nil
}

func probeIEC104(ctx context.Context, addr string, commonAddr uint16, timeout time.Duration) error {
	if err := iec104.ProbeGeneralInterrogation(ctx, addr, commonAddr, timeout); err != nil {
		return fmt.Errorf("iec104 probe %s: %w", addr, err)
	}
	return nil
}

func defaultCommandRunner(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
