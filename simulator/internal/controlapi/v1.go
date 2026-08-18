package controlapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

type v1PointValue struct {
	Device string  `json:"device"`
	Slug   string  `json:"slug"`
	Value  float64 `json:"value"`
}
type sseEvent struct {
	ID        uint64    `json:"id"`
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Revision  *uint64   `json:"revision"`
	Payload   any       `json:"payload"`
}

func (s *Server) handleV1Catalog(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	type point struct {
		Device             string                `json:"device"`
		Slug               string                `json:"slug"`
		NameRaw            string                `json:"name_raw"`
		Tag                string                `json:"tag"`
		Class              m261points.PointClass `json:"class"`
		Access             m261points.Access     `json:"access"`
		Unit               string                `json:"unit"`
		Enum               map[int]string        `json:"enum,omitempty"`
		Dangerous          bool                  `json:"dangerous"`
		IEC104Addr         int                   `json:"iec104_addr"`
		ModbusAddr         *int                  `json:"modbus_addr"`
		ModbusClass        *int                  `json:"modbus_class"`
		ReadbackIEC104Addr *int                  `json:"readback_iec104_addr"`
		Range              *m261points.Range     `json:"range"`
	}
	keys := make([]m261points.PointKey, 0, len(m261points.Points))
	for k := range m261points.Points {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].Device == keys[j].Device && keys[i].Slug < keys[j].Slug || keys[i].Device < keys[j].Device
	})
	out := make([]point, 0, len(keys))
	for _, k := range keys {
		m := m261points.Points[k]
		out = append(out, point{m.Device, m.Slug, m.NameRaw, m.Tag, m.Class, m.Access, m.Unit, m.Enum, m.Dangerous, m.IEC104Addr, m.ModbusAddr, m.ModbusClass, m.ReadbackIEC104Addr, m.Range})
	}
	writeJSON(w, http.StatusOK, map[string]any{"points": out})
}

func (s *Server) handleV1State(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	snap, rev := s.cfg.Store.SnapshotWithRevision()
	writeJSON(w, http.StatusOK, map[string]any{"revision": rev, "model_time": s.cfg.Clock.Now(), "points": valuesFromSnapshot(snap)})
}

func valuesFromSnapshot(snap map[m261points.PointKey]float64) []v1PointValue {
	out := make([]v1PointValue, 0, len(snap))
	for k, v := range snap {
		out = append(out, v1PointValue{k.Device, k.Slug, v})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Device == out[j].Device && out[i].Slug < out[j].Slug || out[i].Device < out[j].Device
	})
	return out
}

func (s *Server) handleV1Commands(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		Device string   `json:"device"`
		Slug   string   `json:"slug"`
		Value  *float64 `json:"value"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Value == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Errorf("device, slug and value are required"))
		return
	}
	key := m261points.PointKey{Device: req.Device, Slug: req.Slug}
	if err := s.cfg.Processor.Write(key, *req.Value); err != nil {
		writeError(w, http.StatusBadRequest, "command_rejected", err)
		return
	}
	meta, ok := m261points.Points[key]
	if !ok || meta.ReadbackIEC104Addr == nil {
		writeError(w, http.StatusInternalServerError, "readback_unavailable", fmt.Errorf("no readback point for %s/%s", req.Device, req.Slug))
		return
	}
	// Return the persisted mirror, not an echo of the request. The Store's
	// readback point is the authoritative command result.
	_, readback, ok := s.cfg.Store.GetByIEC(store.IECAddr{CommonAddr: meta.DeviceAddr, ObjAddr: *meta.ReadbackIEC104Addr})
	if !ok {
		writeError(w, http.StatusInternalServerError, "readback_unavailable", fmt.Errorf("readback point for %s/%s is absent from Store", req.Device, req.Slug))
		return
	}
	response := map[string]any{"device": req.Device, "slug": req.Slug, "accepted_value": *req.Value, "readback": readback}
	if d, ok := s.cfg.Processor.DiagnosticFor(key); ok {
		response["diagnostic"] = d
		rev := s.cfg.Store.CurrentRevision()
		s.events.publish("diagnostic", s.cfg.Clock.Now(), &rev, d)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleV1Diagnostics(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"diagnostics": s.cfg.Processor.Diagnostics()})
}
func (s *Server) handleV1Scenarios(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if s.cfg.ScenariosDir == "" {
		writeJSON(w, http.StatusOK, map[string]any{"scenarios": []string{}})
		return
	}
	entries, err := os.ReadDir(s.cfg.ScenariosDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "scenarios_unavailable", err)
		return
	}
	out := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			out = append(out, filepath.Base(e.Name()))
		}
	}
	sort.Strings(out)
	writeJSON(w, http.StatusOK, map[string]any{"scenarios": out})
}
func (s *Server) handleV1ScenarioStatus(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	loaded := s.cfg.ScenarioRunner.Loaded()
	name := ""
	if loaded != nil {
		name = loaded.Name
	}
	err := ""
	if e := s.cfg.ScenarioRunner.LastError(); e != nil {
		err = e.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{"loaded": name, "running": s.cfg.ScenarioRunner.Running(), "cursor": s.cfg.ScenarioRunner.Cursor(), "error": err})
}
func (s *Server) handleV1Status(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model_time": s.cfg.Clock.Now(), "configuration": s.cfg.PublicConfig, "ready": s.ready()})
}
func (s *Server) handleV1Live(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
}
func (s *Server) handleV1Ready(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if !s.ready() {
		writeError(w, http.StatusServiceUnavailable, "not_ready", fmt.Errorf("one or more simulator servers are unavailable"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// handleV1DemoPrepare is intentionally one reset/ready transaction for the
// guided demo: the UI must not assemble it from independently racing calls.
func (s *Server) handleV1DemoPrepare(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !s.ready() {
		writeError(w, http.StatusServiceUnavailable, "not_ready", fmt.Errorf("simulator is not ready"))
		return
	}
	if err := s.doReset(); err != nil {
		writeError(w, http.StatusInternalServerError, "reset_failed", err)
		return
	}
	rev := s.cfg.Store.CurrentRevision()
	s.events.publish("reset", s.cfg.Clock.Now(), &rev, map[string]any{"source": "demo_prepare"})
	writeJSON(w, http.StatusOK, map[string]any{"demo_session_id": fmt.Sprintf("demo-%d", rev), "model_time": s.cfg.Clock.Now(), "revision": rev})
}

func (s *Server) ready() bool {
	return s.listeningReady() && (s.cfg.Ready == nil || s.cfg.Ready())
}

// handleV1Events uses Store's atomic subscribe-and-snapshot primitive: no
// state transition can fall between the bootstrap snapshot and batch stream.
func (s *Server) handleV1Events(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if r.URL.Query().Get("initial_state") != "true" {
		writeError(w, http.StatusBadRequest, "initial_state_required", fmt.Errorf("use ?initial_state=true for an atomic SSE bootstrap"))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", fmt.Errorf("response writer does not support streaming"))
		return
	}
	lastID, err := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64)
	if r.Header.Get("Last-Event-ID") != "" && err != nil {
		writeError(w, http.StatusBadRequest, "invalid_last_event_id", err)
		return
	}
	rare, replay, resync, unsubscribeRare := s.events.subscribe(lastID)
	defer unsubscribeRare()
	ch, snap, rev, unsubscribe := s.cfg.Store.SubscribeBatchesWithSnapshot()
	defer unsubscribe()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	id := uint64(0)
	s.writeSSE(w, sseEvent{ID: id, Type: "snapshot", Timestamp: s.cfg.Clock.Now(), Revision: &rev, Payload: map[string]any{"points": valuesFromSnapshot(snap)}})
	if resync {
		s.writeSSE(w, sseEvent{ID: id, Type: "resync_required", Timestamp: s.cfg.Clock.Now(), Payload: map[string]any{"reason": "rare_event_history_expired"}})
	} else {
		for _, event := range replay {
			s.writeSSE(w, event)
		}
	}
	flusher.Flush()
	last := rev
	// Coalesce high-frequency Store batches for the browser. The Store
	// subscription remains loss-detecting at the batch level: every batch is
	// checked before its final value is folded into this short UI window.
	const telemetryWindow = 150 * time.Millisecond
	ticker := time.NewTicker(telemetryWindow)
	defer ticker.Stop()
	pending := make(map[m261points.PointKey]float64)
	var pendingStartRevision uint64
	var pendingRevision uint64
	flushTelemetry := func() {
		if len(pending) == 0 {
			return
		}
		id = s.events.nextID()
		changes := make([]v1PointValue, 0, len(pending))
		for key, value := range pending {
			changes = append(changes, v1PointValue{Device: key.Device, Slug: key.Slug, Value: value})
		}
		sort.Slice(changes, func(i, j int) bool {
			return changes[i].Device == changes[j].Device && changes[i].Slug < changes[j].Slug || changes[i].Device < changes[j].Device
		})
		s.writeSSE(w, sseEvent{ID: id, Type: "telemetry", Timestamp: s.cfg.Clock.Now(), Revision: &pendingRevision, Payload: map[string]any{"from_revision": pendingStartRevision, "changes": changes}})
		flusher.Flush()
		clear(pending)
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			flushTelemetry()
		case event, ok := <-rare:
			if !ok {
				return
			}
			s.writeSSE(w, event)
			flusher.Flush()
		case batch, ok := <-ch:
			if !ok {
				return
			}
			if batch.Revision != last+1 {
				s.writeSSE(w, sseEvent{ID: s.events.nextID(), Type: "resync_required", Timestamp: s.cfg.Clock.Now(), Payload: map[string]any{"expected_revision": last + 1, "received_revision": batch.Revision}})
				flusher.Flush()
				return
			}
			last = batch.Revision
			if len(pending) == 0 {
				pendingStartRevision = batch.Revision
			}
			for _, c := range batch.Changes {
				pending[c.Key] = c.Value
			}
			pendingRevision = batch.Revision
		}
	}
}
func (s *Server) writeSSE(w http.ResponseWriter, e sseEvent) {
	b, _ := json.Marshal(e)
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Type, b)
}
