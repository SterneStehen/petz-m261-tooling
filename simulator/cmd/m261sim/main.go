// Command m261sim runs the M261 simulator: an IEC-104 server and a
// Modbus TCP server, both backed by one shared point store (Task 4), plus
// Task 7's control API (fault injection, link faults, scenario playback,
// clock control, reset).
package main

import (
	"flag"
	"log"
	"math"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/appgate"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/config"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/controlapi"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/faults"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/iec104"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/modbustcp"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/physics"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/scenario"
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
	controlAddr := flag.String(
		"control-addr", "",
		"override control_api.bind from the config file (default from config: 127.0.0.1:8081, loopback-only per AGENT-TASK §1.3)",
	)
	configPath := flag.String("config", "simulator/config/m261sim.yaml", "path to the simulator config file (AGENT-TASK §7 parameters)")
	byteOrderOverride := flag.String(
		"modbus-byte-order", "",
		"override modbus.byte_order from the config file: big|little|big_word_swap|little_word_swap",
	)
	initialSOC := flag.Float64("initial-soc", 50, "starting battery SoC, percent (0-100)")
	stepInterval := flag.Duration("physics-step", time.Second, "physics model tick interval (AGENT-TASK §5: 1s default, configurable)")
	speed := flag.Float64(
		"speed", 1.0,
		"live pacing rate for the simulator's single model clock: model-seconds per real second when "+
			"no scenario is loaded and no POST /clock/advance is in flight (1.0 = real-time)",
	)
	scenariosDir := flag.String("scenarios-dir", "scenarios", "directory POST /scenario/load's {\"name\": ...} form reads scenario files from")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	order, cfg, err := resolveByteOrder(cfg, *byteOrderOverride)
	if err != nil {
		log.Fatal(err)
	}
	if *controlAddr != "" {
		cfg.ControlAPI.Bind = *controlAddr
		if err := cfg.Validate(); err != nil {
			log.Fatal(err)
		}
	}
	if *stepInterval <= 0 {
		// PacedRun/time.NewTicker treat a non-positive interval as a
		// silent no-op rather than a panic; fail fast here instead, with
		// a clear message before anything is listening.
		log.Fatalf("-physics-step must be positive, got %s", *stepInterval)
	}
	// math.IsNaN/IsInf checked explicitly, not just <= 0: every
	// comparison against NaN is false in Go ("NaN <= 0" doesn't reject
	// it), and +Inf also passes a bare <= 0 check — both would otherwise
	// reach physics.Runner.PacedRun's own realInterval computation
	// (stepInterval/speed) as a silently broken ticker interval instead
	// of failing fast here with a clear message.
	if math.IsNaN(*speed) || math.IsInf(*speed, 0) || *speed <= 0 {
		log.Fatalf("-speed must be a positive, finite number, got %v", *speed)
	}

	// The simulator has exactly one model clock (AGENT-TASK §1.5),
	// always a *clock.Fake — never clock.Real directly wired into
	// physics/commands/scenario. Task 7's POST /clock/advance and
	// scenario playback need a controllable clock to exist at all times,
	// not only in some special startup mode (a real gap in an earlier
	// version: a "-clock=real" default made every scenario/clock-advance
	// endpoint permanently 409 unless the operator remembered to pass
	// "-clock=fake"). physics.Runner.PacedRun (below) is what makes this
	// clock behave like ordinary wall-clock time by default: it advances
	// the fake clock at -speed model-seconds per real second, on its own,
	// whenever nothing else (a running scenario, an in-flight POST
	// /clock/advance) currently owns driving it (physics.Runner.
	// TryAcquireDrive).
	startupInstant := time.Now()
	sharedClock := clock.NewFake(startupInstant)

	// gate is the process-wide reset-atomicity lock (package appgate):
	// commands.Processor.Write, faults.Injector.Inject/Clear, and
	// physics.Runner.Tick/TickOnce all take its shared side; POST /reset
	// takes the exclusive side for its whole sequence. See
	// controlapi.Server.doReset's doc comment for the gap this closes.
	gate := appgate.New()

	st := store.New()

	// Task 6: build the commands processor before anything else touches
	// the store — it publishes its own out-of-the-box setpoint defaults
	// (Power On, SoC bounds, System Max Charge/Discharge Power; see
	// Processor.publishSensibleDefaults) that the physics runner's own
	// initial publish, below, doesn't override.
	physicsParams := physics.DefaultParams()
	cmdProcessor, err := commands.NewProcessor(st, sharedClock, commandsConfigFrom(cfg, physicsParams.NominalACPowerKW))
	if err != nil {
		log.Fatalf("commands: %v", err)
	}
	cmdProcessor.SetGate(gate)
	log.Printf(
		"commands: watchdog=%s (%ds), mode priority=%v, allow_dangerous=%v",
		cfg.Watchdog.Mode.Value, cfg.Watchdog.TimeoutS.Value, cfg.Modes.Priority.Value, cfg.Commands.AllowDangerous.Value,
	)

	// newEngine rebuilds an Engine identical to the one built here — same
	// params, same initial SoC, and (since RNGSeed lives in physicsParams)
	// the same RNG seed — for Task 7's POST /reset (AGENT-TASK.md, Task 7
	// item 7: reset must reproduce the same simulated future a fresh
	// process start would, not just some valid one).
	newEngine := func() *physics.Engine { return physics.New(physicsParams, *initialSOC) }
	runner := physics.NewRunner(newEngine(), st, sharedClock, cmdProcessor)
	runner.SetGate(gate)

	injector := faults.NewInjector(st)
	injector.SetGate(gate)

	// StartupSnapshot is captured now — after the commands processor's
	// sensible defaults and physics.NewRunner's own initial writeState
	// have already published everything they're going to at startup, but
	// *before* either protocol listener opens below — so POST /reset
	// (controlapi) has an exact, literal record of "the state right after
	// process start" to restore. Reviewed gap this closes: capturing the
	// snapshot after the listeners were already open meant a client that
	// connected and wrote something in the window before the first
	// request to /reset would have that write baked into the reset
	// baseline itself.
	startupSnapshot := st.Snapshot()

	mb := modbustcp.New(st, modbustcp.Config{Addr: *modbusAddr, ByteOrder: order, Commands: cmdProcessor})
	mb.SetGate(gate)
	if err := mb.Start(); err != nil {
		log.Fatalf("modbus tcp: %v", err)
	}
	log.Printf(
		"modbus tcp listening on %s (byte order: %s, unconfirmed: %v)",
		mb.Addr(), cfg.Modbus.ByteOrder.Value, cfg.Modbus.ByteOrder.Unconfirmed,
	)

	iec := iec104.New(st, iec104.Config{Addr: *iecAddr, Commands: cmdProcessor})
	iec.SetGate(gate)
	if err := iec.Start(); err != nil {
		log.Fatalf("iec104: %v", err)
	}
	log.Printf("iec104 listening on %s", iec.Addr())

	// Task 7: scenario engine and control API.
	scenarioRunner := scenario.NewRunner(st, injector, cmdProcessor, runner, sharedClock, *stepInterval, iec, mb)
	scenarioRunner.SetGate(gate)

	capi := controlapi.New(controlapi.Config{
		Addr:            cfg.ControlAPI.Bind,
		Store:           st,
		Injector:        injector,
		Processor:       cmdProcessor,
		PhysicsRunner:   runner,
		Clock:           sharedClock,
		StepInterval:    *stepInterval,
		ScenarioRunner:  scenarioRunner,
		IECServer:       iec,
		ModbusServer:    mb,
		ScenariosDir:    *scenariosDir,
		Gate:            gate,
		StartupSnapshot: startupSnapshot,
		NewEngine:       newEngine,
		StartupInstant:  startupInstant,
	})
	if err := capi.Start(); err != nil {
		log.Fatalf("control api: %v", err)
	}
	log.Printf("control api listening on %s", capi.Addr())

	go runner.PacedRun(*stepInterval, *speed, nil)
	log.Printf(
		"physics model running at a %s step, speed=%vx, starting SoC %.1f%% -- POST /clock/advance and scenarios "+
			"available at any time (suspends live pacing for their own duration)",
		*stepInterval, *speed, *initialSOC,
	)

	select {} // servers, the physics runner, and the control API run in background goroutines; block forever
}
