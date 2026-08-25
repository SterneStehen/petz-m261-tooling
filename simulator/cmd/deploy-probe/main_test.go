package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/iec104"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/modbustcp"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

func TestProbeHTTPReadySuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "ready"}) //nolint:errcheck
	}))
	defer srv.Close()

	if err := probeHTTPReady(context.Background(), srv.URL, time.Second); err != nil {
		t.Fatalf("probeHTTPReady: %v", err)
	}
}

func TestProbeHTTPReadyRejectsWrongBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "booting"}) //nolint:errcheck
	}))
	defer srv.Close()

	err := probeHTTPReady(context.Background(), srv.URL, time.Second)
	if err == nil || !strings.Contains(err.Error(), `expected JSON status "ready"`) {
		t.Fatalf("probeHTTPReady error = %v, want status mismatch", err)
	}
}

func TestWaitForComposeHealthyPollsUntilHealthy(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	inspectCalls := 0
	cfg := config{
		composeDir:     "/opt/m261sim",
		service:        "m261sim",
		composeTimeout: 5 * time.Second,
		runCommand: func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case name == "docker" && len(args) == 4 && args[0] == "compose" && args[1] == "ps":
				return []byte("abc123\n"), nil
			case name == "docker" && len(args) >= 4 && args[0] == "inspect":
				inspectCalls++
				if inspectCalls < 2 {
					return []byte("running/starting\n"), nil
				}
				return []byte("running/healthy\n"), nil
			default:
				return nil, fmt.Errorf("unexpected command: %s %v", name, args)
			}
		},
	}

	if err := waitForComposeHealthy(context.Background(), cfg); err != nil {
		t.Fatalf("waitForComposeHealthy: %v", err)
	}
	if inspectCalls != 2 {
		t.Fatalf("inspectCalls = %d, want 2", inspectCalls)
	}
}

func TestProbeModbusSuccess(t *testing.T) {
	st := store.New()
	st.Set(m261points.PointKey{Device: "BMS", Slug: "soc"}, 50)
	srv := modbustcp.New(st, modbustcp.Config{Addr: "127.0.0.1:0", ByteOrder: m261points.BigEndian})
	if err := srv.Start(); err != nil {
		t.Fatalf("modbustcp start: %v", err)
	}
	defer srv.Close()

	if err := probeModbus(context.Background(), srv.Addr().String(), 34, 2, 2, 2*time.Second); err != nil {
		t.Fatalf("probeModbus: %v", err)
	}
}

func TestProbeModbusRejectsException(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req := make([]byte, 12)
		if _, err := io.ReadFull(conn, req); err != nil {
			return
		}
		resp := []byte{
			req[0], req[1], 0x00, 0x00, 0x00, 0x03, req[6],
			0x84, 0x02, // FC04 exception response
		}
		conn.Write(resp) //nolint:errcheck
	}()

	err = probeModbus(context.Background(), ln.Addr().String(), 34, 2, 2, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "exception") {
		t.Fatalf("probeModbus error = %v, want exception response", err)
	}
}

func TestProbeIEC104Success(t *testing.T) {
	st := store.New()
	st.Set(m261points.PointKey{Device: "EMS", Slug: "desired_active_power_kw"}, 42.5)
	srv := iec104.New(st, iec104.Config{Addr: "127.0.0.1:0"})
	if err := srv.Start(); err != nil {
		t.Fatalf("iec104 start: %v", err)
	}
	defer srv.Close()

	if err := probeIEC104(context.Background(), srv.Addr().String(), 1, 5*time.Second); err != nil {
		t.Fatalf("probeIEC104: %v", err)
	}
}

func TestRunSuccess(t *testing.T) {
	modbusStore := store.New()
	modbusStore.Set(m261points.PointKey{Device: "BMS", Slug: "soc"}, 50)
	mb := modbustcp.New(modbusStore, modbustcp.Config{Addr: "127.0.0.1:0", ByteOrder: m261points.BigEndian})
	if err := mb.Start(); err != nil {
		t.Fatalf("modbustcp start: %v", err)
	}
	defer mb.Close()

	iecStore := store.New()
	iecStore.Set(m261points.PointKey{Device: "EMS", Slug: "desired_active_power_kw"}, 10)
	iec := iec104.New(iecStore, iec104.Config{Addr: "127.0.0.1:0"})
	if err := iec.Start(); err != nil {
		t.Fatalf("iec104 start: %v", err)
	}
	defer iec.Close()

	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ready"}) //nolint:errcheck
	}))
	defer httpSrv.Close()

	cfg := config{
		composeDir:     "/opt/m261sim",
		service:        "m261sim",
		readyURL:       httpSrv.URL,
		modbusAddr:     mb.Addr().String(),
		modbusUnitID:   34,
		modbusAddress:  2,
		modbusCount:    2,
		iec104Addr:     iec.Addr().String(),
		iec104Common:   1,
		composeTimeout: time.Second,
		httpTimeout:    time.Second,
		modbusTimeout:  2 * time.Second,
		iec104Timeout:  5 * time.Second,
		runCommand: func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
			switch {
			case name == "docker" && len(args) == 4 && args[0] == "compose" && args[1] == "ps":
				return []byte("abc123\n"), nil
			case name == "docker" && len(args) >= 4 && args[0] == "inspect":
				return []byte("running/healthy\n"), nil
			default:
				return nil, fmt.Errorf("unexpected command: %s %v", name, args)
			}
		},
	}

	if err := run(context.Background(), cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
}
