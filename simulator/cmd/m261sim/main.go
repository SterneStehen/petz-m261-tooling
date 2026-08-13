// Command m261sim runs the M261 simulator: an IEC-104 server and a
// Modbus TCP server, both backed by one shared point store (Task 4).
package main

import (
	"flag"
	"log"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/config"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/iec104"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/modbustcp"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/physics"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// resolveByteOrder applies an optional CLI override on top of a loaded
// config and resolves the result to the codec type — split out from
// main() so the override/validate logic is unit-testable on its own.
func resolveByteOrder(cfg config.Config, override string) (m261points.ByteOrder, config.Config, error) {
	if override != "" {
		cfg.Modbus.ByteOrder.Value = override
		if err := cfg.Validate(); err != nil {
			return 0, cfg, err
		}
	}
	order, err := cfg.ModbusByteOrder()
	return order, cfg, err
}

// commandsConfigFrom translates the loaded, YAML-shaped config.Config into
// commands.Config's plain resolved values — split out for the same reason
// resolveByteOrder is: unit-testable without touching YAML or a real
// Processor. nominalPowerKW is threaded through separately (see
// commands.Config.NominalPowerKW's doc comment) rather than hardcoded here,
// so it always matches whatever physics.Params main() actually built the
// engine with.
func commandsConfigFrom(cfg config.Config, nominalPowerKW float64) commands.Config {
	return commands.Config{
		WatchdogMode:    commands.WatchdogMode(cfg.Watchdog.Mode.Value),
		WatchdogTimeout: time.Duration(cfg.Watchdog.TimeoutS.Value) * time.Second,
		ModePriority:    cfg.Modes.Priority.Value,
		AllowDangerous:  cfg.Commands.AllowDangerous.Value,
		NominalPowerKW:  nominalPowerKW,
	}
}

func main() {
	modbusAddr := flag.String("modbus-addr", ":502", "Modbus TCP listen address")
	iecAddr := flag.String("iec104-addr", ":2404", "IEC-104 listen address")
	configPath := flag.String("config", "simulator/config/m261sim.yaml", "path to the simulator config file (AGENT-TASK §7 parameters)")
	byteOrderOverride := flag.String(
		"modbus-byte-order", "",
		"override modbus.byte_order from the config file: big|little|big_word_swap|little_word_swap",
	)
	initialSOC := flag.Float64("initial-soc", 50, "starting battery SoC, percent (0-100)")
	stepInterval := flag.Duration("physics-step", time.Second, "physics model tick interval (AGENT-TASK §5: 1s default, configurable)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	order, cfg, err := resolveByteOrder(cfg, *byteOrderOverride)
	if err != nil {
		log.Fatal(err)
	}
	if *stepInterval <= 0 {
		// time.NewTicker panics on a non-positive duration; fail with a
		// clear message before anything is listening, rather than crash
		// the process the first time the physics runner starts.
		log.Fatalf("-physics-step must be positive, got %s", *stepInterval)
	}

	st := store.New()

	// Task 6: build the commands processor before anything else touches
	// the store — it publishes its own out-of-the-box setpoint defaults
	// (Power On, SoC bounds, System Max Charge/Discharge Power; see
	// Processor.publishSensibleDefaults) that the physics runner's own
	// initial publish, below, doesn't override.
	physicsParams := physics.DefaultParams()
	cmdProcessor, err := commands.NewProcessor(st, clock.Real{}, commandsConfigFrom(cfg, physicsParams.NominalACPowerKW))
	if err != nil {
		log.Fatalf("commands: %v", err)
	}
	log.Printf(
		"commands: watchdog=%s (%ds), mode priority=%v, allow_dangerous=%v",
		cfg.Watchdog.Mode.Value, cfg.Watchdog.TimeoutS.Value, cfg.Modes.Priority.Value, cfg.Commands.AllowDangerous.Value,
	)

	// Build and publish the physics model BEFORE either protocol listener
	// opens: a client connecting in the window before the first Tick fires
	// (physics-step defaults to 1s but is configurable, so that window
	// isn't always negligible) must see the configured initial SoC/
	// voltage/online status, not the store's zero defaults.
	engine := physics.New(physicsParams, *initialSOC)
	runner := physics.NewRunner(engine, st, clock.Real{}, cmdProcessor)

	mb := modbustcp.New(st, modbustcp.Config{Addr: *modbusAddr, ByteOrder: order, Commands: cmdProcessor})
	if err := mb.Start(); err != nil {
		log.Fatalf("modbus tcp: %v", err)
	}
	log.Printf(
		"modbus tcp listening on %s (byte order: %s, unconfirmed: %v)",
		mb.Addr(), cfg.Modbus.ByteOrder.Value, cfg.Modbus.ByteOrder.Unconfirmed,
	)

	iec := iec104.New(st, iec104.Config{Addr: *iecAddr, Commands: cmdProcessor})
	if err := iec.Start(); err != nil {
		log.Fatalf("iec104: %v", err)
	}
	log.Printf("iec104 listening on %s", iec.Addr())

	go runner.Run(*stepInterval, nil)
	log.Printf("physics model running at a %s step, starting SoC %.1f%%", *stepInterval, *initialSOC)

	select {} // servers and the physics runner run in background goroutines; block forever
}
