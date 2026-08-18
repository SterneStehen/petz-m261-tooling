package commands

import (
	"sort"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
)

// DiagCodeAcceptedButUnsupported is the one stable, machine-readable
// diagnostic code Task 6 defines: a write was accepted (store and readback
// both updated) but the simulator has no modeled effect for it — either
// because it's a dangerous command with no confirmed dispatched-power
// behavior (Trip, Clear Protection — see Write's doc comment) or because
// it selects a mode with no confirmed computation (Demand Control, Load
// Tracking — see ResolvePower's doc comment). Tests must assert on this
// field, not parse a log line — see Diagnostics/DiagnosticFor.
const DiagCodeAcceptedButUnsupported = "accepted_but_unsupported"

// Diagnostic is one structured, queryable event. SelectedMode is only set
// for Demand Control/Load Tracking (the mode that won priority but has no
// modeled effect); it's the empty string for Trip/Clear Protection.
type Diagnostic struct {
	Code          string              `json:"code"`
	PointKey      m261points.PointKey `json:"point_key"`
	AcceptedValue float64             `json:"accepted_value"`
	SelectedMode  string              `json:"selected_mode,omitempty"`
}

// recordDiagnostic stores d, keyed by PointKey — a point's latest
// diagnostic replaces its previous one rather than growing an unbounded
// log. This keeps a long-running simulator (Task 7's multi-hour scenarios)
// bounded in memory while still giving tests (and, later, an HTTP
// inspection endpoint) the current, structured picture per point.
func (p *Processor) recordDiagnostic(d Diagnostic) {
	p.diagMu.Lock()
	defer p.diagMu.Unlock()
	if p.diagnostics == nil {
		p.diagnostics = make(map[m261points.PointKey]Diagnostic)
	}
	p.diagnostics[d.PointKey] = d
}

// Diagnostics returns every point's latest diagnostic, in a stable order
// (sorted by device then slug) so tests can assert on it directly.
func (p *Processor) Diagnostics() []Diagnostic {
	p.diagMu.Lock()
	defer p.diagMu.Unlock()
	out := make([]Diagnostic, 0, len(p.diagnostics))
	for _, d := range p.diagnostics {
		out = append(out, d)
	}
	sortDiagnostics(out)
	return out
}

// DiagnosticFor returns key's latest diagnostic, if any.
func (p *Processor) DiagnosticFor(key m261points.PointKey) (Diagnostic, bool) {
	p.diagMu.Lock()
	defer p.diagMu.Unlock()
	d, ok := p.diagnostics[key]
	return d, ok
}

func sortDiagnostics(ds []Diagnostic) {
	sort.Slice(ds, func(i, j int) bool {
		a, b := ds[i].PointKey, ds[j].PointKey
		if a.Device != b.Device {
			return a.Device < b.Device
		}
		return a.Slug < b.Slug
	})
}
