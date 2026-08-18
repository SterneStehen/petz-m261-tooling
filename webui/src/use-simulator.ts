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

export function useSimulator() {
  const [status, setStatus] = useState<Status | null>(null);
  const [catalog, setCatalog] = useState<Point[]>([]);
  const [points, setPoints] = useState<Record<string, number>>({});
  const [history, setHistory] = useState<Record<string, number[]>>({});
  const [modelTime, setModelTime] = useState("");
  const [stream, setStream] = useState<StreamState>("connecting");
  const [lastEvent, setLastEvent] = useState<RareEvent | null>(null);
  const revision = useRef<number | null>(null);
  const retryTimer = useRef<number | null>(null);
  const initialReplayComplete = useRef(false);

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
    if (Object.keys(points).length === 0) return;
    setHistory((current) => {
      const next = { ...current };
      for (const [key, value] of Object.entries(points)) {
        const samples = next[key] ?? [];
        if (samples.at(-1) !== value) next[key] = [...samples, value].slice(-36);
      }
      return next;
    });
  }, [points]);

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

        if (
          initialReplayComplete.current &&
          ["fault", "reset", "diagnostic", "scenario_step"].includes(
            message.type,
          )
        ) {
          setLastEvent({
            id: message.id,
            type: message.type as RareEvent["type"],
            timestamp: message.timestamp,
            payload: message.payload,
          });
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
    () => ({ status, catalog, points, history, modelTime, stream, lastEvent }),
    [status, catalog, points, history, modelTime, stream, lastEvent],
  );
}
