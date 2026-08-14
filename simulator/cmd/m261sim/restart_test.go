package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// buildM261sim builds the real m261sim binary once per test into
// t.TempDir() and returns its path — used by TestProcessRestartStartsClean
// below, which needs an actual second OS process, not another in-process
// harness instance. scenarios/restart.yaml's own header comment explains
// why a literal process restart isn't expressible in the scenario DSL at
// all (POST /reset is explicitly a Store/state reset, not a process
// restart — AGENT-TASK.md, Task 7 item 7's own "not a network event"
// language extends to "not a process lifecycle event" too); this test is
// the reviewed requirement to cover that gap with something that actually
// restarts the process, not another way of asking the same running
// process to reset itself.
func buildM261sim(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "m261sim")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build m261sim: %v\n%s", err, out)
	}
	return bin
}

// startedM261sim is one running m261sim OS process, with the three
// addresses it actually bound to (parsed from its own startup log lines
// — every test address below is 127.0.0.1:0, so the real bound port is
// only known after the process itself reports it).
type startedM261sim struct {
	cmd                              *exec.Cmd
	modbusAddr, iecAddr, controlAddr string
}

var (
	reModbusListen  = regexp.MustCompile(`modbus tcp listening on (\S+)`)
	reIECListen     = regexp.MustCompile(`iec104 listening on (\S+)`)
	reControlListen = regexp.MustCompile(`control api listening on (\S+)`)
)

// startM261simProcess spawns the real built binary against loopback:0
// addresses (never a fixed port — parallel test runs, or a leftover
// process from a previous run, must not collide) and blocks until all
// three of its own "listening on ..." log lines have been seen, so the
// caller never races the child's own startup.
func startM261simProcess(t *testing.T, bin string, extraArgs ...string) *startedM261sim {
	t.Helper()
	args := append([]string{
		"-modbus-addr=127.0.0.1:0",
		"-iec104-addr=127.0.0.1:0",
		"-control-addr=127.0.0.1:0",
	}, extraArgs...)
	cmd := exec.Command(bin, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start m261sim: %v", err)
	}

	got := &startedM261sim{cmd: cmd}
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	deadline := time.After(10 * time.Second)
	for got.modbusAddr == "" || got.iecAddr == "" || got.controlAddr == "" {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("m261sim exited before reporting all three listen addresses (modbus=%q iec=%q control=%q)",
					got.modbusAddr, got.iecAddr, got.controlAddr)
			}
			if m := reModbusListen.FindStringSubmatch(line); m != nil {
				got.modbusAddr = m[1]
			}
			if m := reIECListen.FindStringSubmatch(line); m != nil {
				got.iecAddr = m[1]
			}
			if m := reControlListen.FindStringSubmatch(line); m != nil {
				got.controlAddr = m[1]
			}
		case <-deadline:
			t.Fatalf("m261sim did not report all three listen addresses within 10s (modbus=%q iec=%q control=%q)",
				got.modbusAddr, got.iecAddr, got.controlAddr)
		}
	}
	t.Cleanup(func() {
		if got.cmd.Process != nil {
			got.cmd.Process.Kill() //nolint:errcheck
			got.cmd.Wait()         //nolint:errcheck
		}
	})
	return got
}

func (s *startedM261sim) getState(t *testing.T) map[string]float64 {
	t.Helper()
	resp, err := http.Get("http://" + s.controlAddr + "/state")
	if err != nil {
		t.Fatalf("GET /state: %v", err)
	}
	defer resp.Body.Close()
	var decoded struct {
		Points map[string]float64 `json:"points"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode GET /state: %v", err)
	}
	return decoded.Points
}

// TestProcessRestartStartsClean is the real (not scenario-DSL, not
// POST /reset) restart acceptance criterion: kill an actual running
// m261sim OS process mid-dirty-state and start a genuinely new one
// against the same config — the new process must come up at the exact
// same deterministic startup state the first one did (same config, same
// hardcoded initial SoC/RNG seed — AGENT-TASK.md, Task 7 item 7's
// determinism requirement extends to "a fresh process", not only to
// POST /reset on a process already running), never anything influenced
// by whatever the killed process's in-memory state happened to be —
// there is no persistence layer for this to accidentally leak through,
// but this test proves that structurally rather than assuming it.
func TestProcessRestartStartsClean(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real OS process and a real go build; skipped under -short")
	}
	bin := buildM261sim(t)

	// -physics-step=24h: the reviewed fix for a flaky assumption -- the
	// live background pacer (physics.Runner.PacedRun) starts ticking the
	// instant main() reaches it, before this test ever gets a chance to
	// read GET /state, so "heartbeat is still exactly 0" is only
	// deterministically true if no tick could plausibly have fired yet.
	// A 24h step (paired with the default -speed=1x) makes the first real
	// tick due so far in the future that this test's own real-time
	// duration can never reach it, however slow the CI worker -- closing
	// the gap a 1s default step left open (a tick landing between process
	// start and this test's first GET /state on a sufficiently loaded
	// machine).
	first := startM261simProcess(t, bin, "-physics-step=24h")
	firstState := first.getState(t)
	hb, ok := firstState["EMS/ems_periodic_heartbeat_indicator"]
	if !ok {
		t.Fatal("first process: EMS/ems_periodic_heartbeat_indicator missing from GET /state")
	}
	if hb != 0 {
		t.Fatalf("first process: heartbeat = %v immediately after startup, want 0 (no PacedRun tick can plausibly have fired yet, -physics-step=24h)", hb)
	}

	// Dirty the first process's in-memory state via its own control API —
	// exactly the kind of change a real restart (unlike POST /reset,
	// AGENT-TASK.md, Task 7 item 7) must not carry forward, since nothing
	// about this process's actual crash-and-restart semantics involves
	// snapshotting or restoring anything.
	dirtyReq, _ := json.Marshal(map[string]any{"device": "BMS", "point": "cell_temperature_too_high", "value": 1})
	resp, err := http.Post("http://"+first.controlAddr+"/faults", "application/json", strings.NewReader(string(dirtyReq)))
	if err != nil {
		t.Fatalf("POST /faults on first process: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /faults on first process: status %d", resp.StatusCode)
	}
	dirtied := first.getState(t)
	if dirtied["BMS/cell_temperature_too_high"] != 1 {
		t.Fatalf("first process: BMS/cell_temperature_too_high = %v after injection, want 1 (setup)", dirtied["BMS/cell_temperature_too_high"])
	}

	// Kill it — not POST /scenario/stop, not POST /reset: an actual
	// process termination, the only way to test a restart's own
	// guarantees rather than an in-process reset's.
	if err := first.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill first process: %v", err)
	}
	first.cmd.Wait() //nolint:errcheck

	second := startM261simProcess(t, bin, "-physics-step=24h")
	secondState := second.getState(t)

	if got := secondState["BMS/cell_temperature_too_high"]; got != 0 {
		t.Errorf("second process: BMS/cell_temperature_too_high = %v, want 0 (a fresh process, not a survivor of the first's dirty state)", got)
	}
	if got := secondState["EMS/ems_periodic_heartbeat_indicator"]; got != 0 {
		t.Errorf("second process: heartbeat = %v, want 0 (no PacedRun tick can plausibly have fired yet, -physics-step=24h -- same deterministic startup state as the first process had)", got)
	}
	// Every point the first process had at its own clean startup must
	// match the second process's clean startup, point for point — same
	// config, same hardcoded initial SoC, same RNG seed (physics.New's
	// determinism, AGENT-TASK.md Task 7 item 7's "same simulated future"
	// requirement, here proven across two independent processes rather
	// than across one process's own POST /reset).
	if len(firstState) != len(secondState) {
		t.Fatalf("point count differs: first process %d, second process %d", len(firstState), len(secondState))
	}
	for k, v := range firstState {
		if secondState[k] != v {
			t.Errorf("%s: first process clean startup = %v, second process clean startup = %v, want equal", k, v, secondState[k])
		}
	}
}
