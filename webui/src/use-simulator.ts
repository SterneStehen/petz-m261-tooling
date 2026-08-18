import { useEffect, useMemo, useRef, useState } from "react";
import { api, type Point, type PointValue, type Status } from "./api";

export type StreamState =
  "connecting" | "connected" | "reconnecting" | "offline";
export type RareEvent = {
  id: number;
  type: "fault" | "reset" | "diagnostic" | "scenario_step";
  timestamp: string;
  payload: Record<string, unknown>;
};
export type ScenarioStepEvent = {
  scenario: string;
  index: number;
  action: string;
  result: string;
  error: string;
};
// A history point pairs the value with when it was actually observed --
// charts plot real elapsed time, not an evenly-spaced fake index.
export type Sample = { value: number; t: number };
const RECENT_EVENTS_STORAGE_KEY = "m261-recent-events";

// Task 10.1 item 4: bounded, most-recent-first log of rare events shown on
// Overview -- distinct from `lastEvent` (still kept, drives toasts).
const RECENT_EVENTS_LIMIT = 20;

// Task 10.1 item 3: the heartbeat point is identified by its catalog name,
// never a hardcoded slug -- gen/go/m261points is the only source of truth
// for device/slug, and the catalog endpoint already mirrors it verbatim.
const HEARTBEAT_POINT_NAME = "EMS Periodic Heartbeat Indicator";

export function useSimulator() {
  const [status, setStatus] = useState<Status | null>(null);
  const [catalog, setCatalog] = useState<Point[]>([]);
  const [points, setPoints] = useState<Record<string, number>>({});
  const [history, setHistory] = useState<Record<string, Sample[]>>({});
  const [modelTime, setModelTime] = useState("");
  const [stream, setStream] = useState<StreamState>("connecting");
  const [lastEvent, setLastEvent] = useState<RareEvent | null>(null);
  // Restored once at mount from a prior session -- this is a persisted
  // historical log, not the "just happened" toast path (that stays gated
  // on initialReplayComplete below and is never restored from storage).
  const [events, setEvents] = useState<RareEvent[]>(() => {
    try {
      const stored = window.localStorage.getItem(RECENT_EVENTS_STORAGE_KEY);
      return stored ? (JSON.parse(stored) as RareEvent[]) : [];
    } catch {
      return [];
    }
  });
  const [scenarioProgress, setScenarioProgress] = useState<ScenarioStepEvent[]>([]);
  const [heartbeatTick, setHeartbeatTick] = useState<number | null>(null);
  const revision = useRef<number | null>(null);
  const retryTimer = useRef<number | null>(null);
  const initialReplayComplete = useRef(false);
  const lastHeartbeatValue = useRef<number | null>(null);

  useEffect(() => {
    void Promise.all([api.status(), api.catalog()])
      .then(([nextStatus, nextCatalog]) => {
        setStatus(nextStatus);
        setCatalog(nextCatalog.points);
      })
      .catch(() => setStream("offline"));
  }, []);

  useEffect(() => {
    const timer = window.setInterval(() => {
      void api.status().then(setStatus).catch(() => undefined);
    }, 1000);
    return () => window.clearInterval(timer);
  }, []);

  useEffect(() => {
    try {
      window.localStorage.setItem(
        RECENT_EVENTS_STORAGE_KEY,
        JSON.stringify(events),
      );
    } catch {
      // Storage can legitimately be unavailable (private browsing, quota) --
      // the in-memory list still works for the current session either way.
    }
  }, [events]);

  useEffect(() => {
    if (Object.keys(points).length === 0) return;
    setHistory((current) => {
      const next = { ...current };
      const now = Date.now();
      for (const [key, value] of Object.entries(points)) {
        const samples = next[key] ?? [];
        if (samples.at(-1)?.value !== value)
          next[key] = [...samples, { value, t: now }].slice(-36);
      }
      return next;
    });
  }, [points]);

  // Task 10.1 item 3: resolve the heartbeat point's device/slug from the
  // already-fetched catalog, by its catalog name -- never a literal
  // "ems_periodic_heartbeat_indicator" string anywhere in this file. If the
  // catalog ever drops or renames this point, heartbeatKey silently becomes
  // null and the indicator has nothing to watch, rather than watching the
  // wrong (or a nonexistent) point.
  const heartbeatKey = useMemo(() => {
    const point = catalog.find(
      (item) => item.device === "EMS" && item.name_raw === HEARTBEAT_POINT_NAME,
    );
    return point ? `${point.device}/${point.slug}` : null;
  }, [catalog]);

  useEffect(() => {
    if (!heartbeatKey) return;
    const value = points[heartbeatKey];
    if (value === undefined) return;
    if (lastHeartbeatValue.current !== null && value !== lastHeartbeatValue.current) {
      setHeartbeatTick(Date.now());
    }
    lastHeartbeatValue.current = value;
  }, [heartbeatKey, points[heartbeatKey ?? ""]]);

  useEffect(() => {
    let closed = false;
    let source: EventSource | null = null;
    const connect = () => {
      if (closed) return;
      setStream(revision.current === null ? "connecting" : "reconnecting");
      source = new EventSource("/api/v1/events?initial_state=true");
      initialReplayComplete.current = false;
      const consume = (event: MessageEvent<string>) => {
        const message = JSON.parse(event.data) as {
          id: number;
          type: string;
          revision: number | null;
          timestamp: string;
          payload: {
            points?: PointValue[];
            changes?: PointValue[];
            from_revision?: number;
            [key: string]: unknown;
          };
        };
        if (message.type === "resync_required") {
          source?.close();
          connectLater();
          return;
        }
        if (message.type === "snapshot") {
          const next: Record<string, number> = {};
          for (const point of message.payload.points ?? [])
            next[`${point.device}/${point.slug}`] = point.value;
          revision.current = message.revision;
          setPoints(next);
          setModelTime(message.timestamp);
          setStream("connected");
          return;
        }
        if (message.type === "initial_replay_complete") {
          initialReplayComplete.current = true;
          return;
        }
        if (message.type === "telemetry" && message.revision !== null) {
          if (
            revision.current !== null &&
            message.payload.from_revision !== revision.current + 1
          ) {
            source?.close();
            connectLater();
            return;
          }
          revision.current = message.revision;
          setPoints((current) => {
            const next = { ...current };
            for (const point of message.payload.changes ?? [])
              next[`${point.device}/${point.slug}`] = point.value;
            return next;
          });
          setModelTime(message.timestamp);
        }

        // Gated on initialReplayComplete exactly like the toast logic this
        // was already built for: a fresh connection's history replay must
        // never be presented as something that just happened (Task 10
        // review round, S7). Task 10.1 items 1 and 4 reuse the same gate.
        if (
          initialReplayComplete.current &&
          ["fault", "reset", "diagnostic", "scenario_step"].includes(
            message.type,
          )
        ) {
          const rare: RareEvent = {
            id: message.id,
            type: message.type as RareEvent["type"],
            timestamp: message.timestamp,
            payload: message.payload,
          };
          setLastEvent(rare);
          setEvents((current) => [rare, ...current].slice(0, RECENT_EVENTS_LIMIT));
          if (message.type === "reset") {
            setScenarioProgress([]);
          }
          if (message.type === "scenario_step") {
            const step: ScenarioStepEvent = {
              scenario: String(message.payload.scenario ?? ""),
              index: Number(message.payload.index ?? 0),
              action: String(message.payload.action ?? ""),
              result: String(message.payload.result ?? ""),
              error: String(message.payload.error ?? ""),
            };
            // Only ever append -- this list represents what has actually
            // executed since connecting, never a predicted future step
            // (Task 10.1 item 1). A new scenario name starts a fresh list
            // rather than appending to a previous run's steps.
            setScenarioProgress((current) => {
              const sameRun = current.length > 0 && current[0].scenario === step.scenario;
              return sameRun ? [...current, step] : [step];
            });
          }
        }
      };
      [
        "snapshot",
        "telemetry",
        "fault",
        "reset",
        "diagnostic",
        "scenario_step",
        "resync_required",
        "initial_replay_complete",
      ].forEach((type) =>
        source!.addEventListener(type, consume as EventListener),
      );
      // Do not replace this EventSource on a transport error: its native retry
      // is what supplies Last-Event-ID and enables the server's rare-event
      // replay contract. Explicit resync/gap recovery above deliberately
      // starts a fresh atomic bootstrap instead.
      source.onerror = () => {
        if (!closed) setStream("reconnecting");
      };
    };
    const connectLater = () => {
      if (!closed) retryTimer.current = window.setTimeout(connect, 800);
    };
    connect();
    return () => {
      closed = true;
      source?.close();
      if (retryTimer.current !== null) window.clearTimeout(retryTimer.current);
    };
  }, []);

  return useMemo(
    () => ({
      status,
      catalog,
      points,
      history,
      modelTime,
      stream,
      lastEvent,
      events,
      scenarioProgress,
      heartbeatKey,
      heartbeatTick,
    }),
    [
      status,
      catalog,
      points,
      history,
      modelTime,
      stream,
      lastEvent,
      events,
      scenarioProgress,
      heartbeatKey,
      heartbeatTick,
    ],
  );
}
