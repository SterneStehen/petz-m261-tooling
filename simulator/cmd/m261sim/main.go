// Command m261sim runs the M261 simulator: an IEC-104 server and a
// Modbus TCP server, both backed by one shared point store (Task 4).
package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/iec104"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/modbustcp"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

func main() {
	modbusAddr := flag.String("modbus-addr", ":502", "Modbus TCP listen address")
	iecAddr := flag.String("iec104-addr", ":2404", "IEC-104 listen address")
	byteOrder := flag.String(
		"modbus-byte-order", "big",
		"big|little|big_word_swap|little_word_swap — unconfirmed by the manufacturer as of writing (AGENT-TASK §7)",
	)
	flag.Parse()

	order, err := parseByteOrder(*byteOrder)
	if err != nil {
		log.Fatal(err)
	}

	st := store.New()

	mb := modbustcp.New(st, modbustcp.Config{Addr: *modbusAddr, ByteOrder: order})
	if err := mb.Start(); err != nil {
		log.Fatalf("modbus tcp: %v", err)
	}
	log.Printf("modbus tcp listening on %s (byte order: %s)", mb.Addr(), *byteOrder)

	iec := iec104.New(st, iec104.Config{Addr: *iecAddr})
	if err := iec.Start(); err != nil {
		log.Fatalf("iec104: %v", err)
	}
	log.Printf("iec104 listening on %s", iec.Addr())

	select {} // servers run in background goroutines; block forever
}

func parseByteOrder(s string) (m261points.ByteOrder, error) {
	switch s {
	case "big":
		return m261points.BigEndian, nil
	case "little":
		return m261points.LittleEndian, nil
	case "big_word_swap":
		return m261points.BigEndianWordSwap, nil
	case "little_word_swap":
		return m261points.LittleEndianWordSwap, nil
	default:
		return 0, fmt.Errorf("unknown modbus-byte-order %q (want big|little|big_word_swap|little_word_swap)", s)
	}
}
