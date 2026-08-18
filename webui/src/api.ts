export type PointValue = { device: string; slug: string; value: number };
export type Point = PointValue & { name_raw: string; class: "alarm" | "telemetry" | "setpoint"; access: "RO" | "WO"; unit: string; dangerous: boolean };
export type Status = { model_time: string; ready: boolean; configuration: Record<string, { value: unknown; unconfirmed: boolean }> };
export type Snapshot = { revision: number; model_time: string; points: PointValue[] };
export type SSEPayload = { id: number; type: string; timestamp: string; revision: number | null; payload: { points?: PointValue[]; changes?: PointValue[]; from_revision?: number; [key: string]: unknown } };
export type Diagnostic = { code: string; PointKey: { Device: string; Slug: string }; AcceptedValue: number; SelectedMode: string };
export type CommandResponse = { device: string; slug: string; accepted_value: number; readback: number; diagnostic?: Diagnostic };

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, { headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) }, ...init });
  if (!response.ok) {
    const body = await response.json().catch(() => null) as { error?: { message?: string } } | null;
    throw new Error(body?.error?.message ?? `Request failed (${response.status})`);
  }
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}

export const api = {
  status: () => request<Status>("/api/v1/status"),
  catalog: () => request<{ points: Point[] }>("/api/v1/catalog"),
  demoPrepare: () => request<{ demo_session_id: string }>("/api/v1/demo/prepare", { method: "POST" }),
  command: (device: string, slug: string, value: number) => request<CommandResponse>("/api/v1/commands", { method: "POST", body: JSON.stringify({ device, slug, value }) }),
  injectFault: (device: string, point: string, value: number) => request<void>("/faults", { method: "POST", body: JSON.stringify({ device, point, value }) }),
  clearFault: (device: string, point: string) => request<void>(`/faults/${encodeURIComponent(device)}/${encodeURIComponent(point)}`, { method: "DELETE" }),
  link: (protocol: string, mode: string, delayMS: number) => request<void>("/link", { method: "POST", body: JSON.stringify({ protocol, mode, delay_ms: delayMS }) }),
  clearLink: (protocol: string) => request<void>("/link/clear", { method: "POST", body: JSON.stringify({ protocol }) }),
  reset: () => request<void>("/reset", { method: "POST" })
};
