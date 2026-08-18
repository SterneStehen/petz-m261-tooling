import { useEffect, useMemo, useRef, useState } from "react";
import { api, type Point, type PointValue, type Status } from "./api";

export type StreamState = "connecting" | "connected" | "reconnecting" | "offline";

export function useSimulator() {
  const [status, setStatus] = useState<Status | null>(null);
  const [catalog, setCatalog] = useState<Point[]>([]);
  const [points, setPoints] = useState<Record<string, number>>({});
  const [modelTime, setModelTime] = useState("");
  const [stream, setStream] = useState<StreamState>("connecting");
  const revision = useRef<number | null>(null);
  const retryTimer = useRef<number | null>(null);

  useEffect(() => {
    void Promise.all([api.status(), api.catalog()]).then(([nextStatus, nextCatalog]) => {
      setStatus(nextStatus); setCatalog(nextCatalog.points);
    }).catch(() => setStream("offline"));
  }, []);

  useEffect(() => {
    let closed = false;
    let source: EventSource | null = null;
    const connect = () => {
      if (closed) return;
      setStream(revision.current === null ? "connecting" : "reconnecting");
      source = new EventSource("/api/v1/events?initial_state=true");
      const consume = (event: MessageEvent<string>) => {
        const message = JSON.parse(event.data) as { type: string; revision: number | null; timestamp: string; payload: { points?: PointValue[]; changes?: PointValue[]; from_revision?: number } };
        if (message.type === "resync_required") { source?.close(); connectLater(); return; }
        if (message.type === "snapshot") {
          const next: Record<string, number> = {};
          for (const point of message.payload.points ?? []) next[`${point.device}/${point.slug}`] = point.value;
          revision.current = message.revision; setPoints(next); setModelTime(message.timestamp); setStream("connected"); return;
        }
        if (message.type === "telemetry" && message.revision !== null) {
          if (revision.current !== null && message.payload.from_revision !== revision.current + 1) { source?.close(); connectLater(); return; }
          revision.current = message.revision;
          setPoints((current) => { const next = { ...current }; for (const point of message.payload.changes ?? []) next[`${point.device}/${point.slug}`] = point.value; return next; });
          setModelTime(message.timestamp);
        }
      };
      ["snapshot", "telemetry", "fault", "reset", "diagnostic", "scenario_step", "resync_required"].forEach((type) => source!.addEventListener(type, consume as EventListener));
      source.onerror = () => { source?.close(); connectLater(); };
    };
    const connectLater = () => { if (!closed) retryTimer.current = window.setTimeout(connect, 800); };
    connect();
    return () => { closed = true; source?.close(); if (retryTimer.current !== null) window.clearTimeout(retryTimer.current); };
  }, []);

  return useMemo(() => ({ status, catalog, points, modelTime, stream }), [status, catalog, points, modelTime, stream]);
}
