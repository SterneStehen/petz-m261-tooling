// Command m261sim runs the M261 simulator: an IEC-104 server and a
// Modbus TCP server, both backed by one shared point store (Task 4).
package main

import (
	"flag"
	"log"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/config"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/iec104"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/modbustcp"
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

func main() {
	modbusAddr := flag.String("modbus-addr", ":502", "Modbus TCP listen address")
	iecAddr := flag.String("iec104-addr", ":2404", "IEC-104 listen address")
	configPath := flag.String("config", "simulator/config/m261sim.yaml", "path to the simulator config file (AGENT-TASK §7 parameters)")
	byteOrderOverride := flag.String(
		"modbus-byte-order", "",
		"override modbus.byte_order from the config file: big|little|big_word_swap|little_word_swap",
	)
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	order, cfg, err := resolveByteOrder(cfg, *byteOrderOverride)
	if err != nil {
		log.Fatal(err)
	}

	st := store.New()

	mb := modbustcp.New(st, modbustcp.Config{Addr: *modbusAddr, ByteOrder: order})
	if err := mb.Start(); err != nil {
		log.Fatalf("modbus tcp: %v", err)
	}
	log.Printf(
		"modbus tcp listening on %s (byte order: %s, unconfirmed: %v)",
		mb.Addr(), cfg.Modbus.ByteOrder.Value, cfg.Modbus.ByteOrder.Unconfirmed,
	)

	iec := iec104.New(st, iec104.Config{Addr: *iecAddr})
	if err := iec.Start(); err != nil {
		log.Fatalf("iec104: %v", err)
	}
	log.Printf("iec104 listening on %s", iec.Addr())

	select {} // servers run in background goroutines; block forever
}
