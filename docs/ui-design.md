# M261 Simulator Web Console — UI Design

**Status:** Proposed for approval

This document defines the Task 10 MVP interface before implementation. It is
an original M261 simulator console. It takes only high-level organisational
principles from general business software (clear navigation, grouping, and
hierarchy); it does not reproduce Worksection or any third-party product's
assets, copy, layout, HTML, CSS, illustrations, or branding.

## Product and users

The console is a local, offline-first interface on the simulator control API.
It helps a presenter explain M261 behaviour, an engineer send safe commands,
and a tester inject controlled faults. It is not a cloud portal, a live plant
control system, or an engineering point explorer.

The MVP has exactly three destinations:

1. **Overview / Demo** — understand live simulated operation and run the
   guided presentation.
2. **Control** — send the small set of operator commands and see the
   backend-confirmed result.
3. **Test Lab** — inject alarms, simulate protocol faults, and reset.

The persistent top bar always contains the `SIMULATOR` label, current model
time, a plain-text connection status, and this three-item navigation.

## Design tokens

### Typography

Use the local system UI stack: `Inter, ui-sans-serif, system-ui, -apple-system,
"Segoe UI", sans-serif`. No remote font request is permitted. Telemetry uses
tabular numerals (`font-variant-numeric: tabular-nums`).

| Token | Value | Use |
|---|---:|---|
| `font.body` | 14 px / 20 px | Forms, labels, dense telemetry |
| `font.meta` | 12 px / 16 px | Units, hints, status detail |
| `font.metric` | 28 px / 32 px, 600 | Main metric values |
| `font.heading` | 24 px / 30 px, 650 | Page headings |
| `font.title` | 16 px / 22 px, 650 | Panel headings |

### Colour

| Token | Value | Meaning |
|---|---|---|
| `canvas` | `#F7F6F2` | Warm near-white page background |
| `surface` | `#FFFFFF` | Cards and dialogs |
| `surface.muted` | `#F0F0EB` | Dense data strips and disabled areas |
| `ink` | `#161817` | Primary text and primary action |
| `ink.muted` | `#626862` | Supporting text |
| `line` | `#DDDCD5` | Quiet separators |
| `blue` | `#2367D1` | Selected navigation and active state |
| `green` | `#1C7C54` | Normal, charging, success |
| `amber` | `#A86500` | Warning, limit, unconfirmed |
| `red` | `#B42318` | Alarm, rejected, dangerous action |
| `gray` | `#667085` | Offline, unavailable, disabled |

Colour never communicates a state by itself. Every chip and banner includes
plain language plus an icon or symbol: for example, `● Connected`,
`! Unconfirmed`, and `× Alarm`.

### Layout and interaction

| Token | Value |
|---|---:|
| `space.1` / `2` / `3` / `4` / `6` / `8` | 4 / 8 / 12 / 16 / 24 / 32 px |
| `radius.card` | 16 px |
| `radius.control` | 10 px |
| `shadow.surface` | `0 1px 2px rgb(16 24 40 / 6%), 0 8px 24px rgb(16 24 40 / 5%)` |
| `focus` | 3 px `#2367D1` outer ring, 2 px offset |
| `desktop` | 1280 px reference width |
| `tablet` | 768 px breakpoint |
| `mobile` | 390 px reference width |

Cards use a large soft radius, a subtle surface tint, and quiet shadows rather
than heavy borders. Motion is limited to loading, toast entry, and state
transition feedback; all motion is removed or reduced for
`prefers-reduced-motion: reduce`.

## Shared component inventory

Only these reusable components are required by the MVP:

- **Top bar:** `SIMULATOR`, navigation, model time, connection summary.
- **Status chip:** text, optional icon, and semantic colour.
- **Metric card:** name, primary value, unit, comparison/detail line.
- **Chart panel:** title, current value, time-series chart, empty/loading
  state.
- **Device summary:** device name, online state, key telemetry, alarm count.
- **Command form:** labelled inputs, validation, pending state, and confirmed
  backend response.
- **Fault/link panel:** search or protocol controls with explicit scope.
- **Confirmation dialog:** named dangerous action, consequence, Cancel and
  Confirm controls.
- **Toast:** confirmed, rejected, or connection result; never a speculative
  success message.
- **Loading, error, and empty states:** consistent surface-level components,
  not ad hoc text.

## Navigation and user flows

### Overview / Demo

The default route is `/`. A presenter opens the console, checks readiness,
selects **Prepare Demo**, and receives a confirmed demo session from
`POST /api/v1/demo/prepare`. The page then shows system scale, live status,
and the guided-demo sequence. It does not claim a command is complete until
the backend response and subsequent state stream agree.

### Control

The operator opens `/control`, changes one small command group, reviews the
requested value and known constraints, then submits. The form becomes pending
while the request is in flight. It shows success only after the API confirms
it; rejections remain visible with the backend reason. `Trip` and `Clear
Protection` are isolated in a red Danger Zone and open a confirmation dialog.
The backend remains the authority when `commands.allow_dangerous` is false.

### Test Lab

The tester opens `/test-lab`, searches the generated alarm catalog without
inventing severity, injects or clears an alarm, then observes the backend
result. Protocol-fault controls explicitly choose IEC-104 or Modbus, mode, and
delay where applicable. Reset requires confirmation and is sent only through
the existing reset endpoint.

## Wireframes

### Overview / Demo — desktop (1280 px)

```text
┌ SIMULATOR ── Overview / Demo | Control | Test Lab ── 2026-08-12 10:00  ● API connected ┐
├ Overview / Demo                                      [Prepare Demo]                         ┤
│ [ SoC 52.4 % ] [ Charging 100 kW ] [ Requested −100 kW ] [ Alarm count 0 ]                │
│                                                                                              │
│ ┌ SoC and power chart ──────────────────────────┐ ┌ System state ─────────────────────────┐ │
│ │  SoC / active power over model time           │ │ Mode: Remote   Grid: Connected          │ │
│ │                                                │ │ Limit: 50 kW — System maximum           │ │
│ └───────────────────────────────────────────────┘ └────────────────────────────────────────┘ │
│ ┌ Temperature chart ────────────────────────────┐ ┌ Device summary ───────────────────────┐ │
│ │ Battery temperature over model time            │ │ EMS / PCS / BMS / Meter status chips   │ │
│ └───────────────────────────────────────────────┘ └────────────────────────────────────────┘ │
│ Configuration: Byte order big  [! Unconfirmed] · Watchdog zero_after [! Unconfirmed]         │
└──────────────────────────────────────────────────────────────────────────────────────────────┘
```

### Overview / Demo — mobile (390 px)

```text
┌ SIMULATOR                         ● Connected ┐
│ Overview / Demo  Control  Test Lab             │
│ 2026-08-12 10:00       [Prepare Demo]          │
│ [ SoC 52.4 % ]                                  │
│ [ Charging 100 kW ]                             │
│ [ Requested −100 kW ]                           │
│ [ Alarm count 0 ]                               │
│ [ SoC and power chart ]                         │
│ [ Temperature chart ]                           │
│ [ System state ]                                │
│ [ Device summary ]                              │
│ [ Configuration · Unconfirmed ]                 │
└────────────────────────────────────────────────┘
```

The mobile navigation remains horizontally scrollable with visible labels; it
does not collapse into an unlabeled icon menu.

### Control — desktop (1280 px)

```text
┌ SIMULATOR ── Overview / Demo | Control | Test Lab ── model time ──────────────────────────────┐
├ Control                                                                                          ┤
│ ┌ Operating state ────────────────────────────┐ ┌ Power command ──────────────────────────────┐ │
│ │ Power: [On / Off]  Mode: [Manual|Remote|Auto]│ │ Active kW [ -100 ]  Reactive kvar [ 0 ]     │ │
│ │ Backend state: Remote · confirmed            │ │ Requested / limited / dispatched explanation │ │
│ └──────────────────────────────────────────────┘ │                         [Send command]       │ │
│ ┌ Operating limits ───────────────────────────┐ └───────────────────────────────────────────────┘ │
│ │ Max charge SOC [ ] Min discharge SOC [ ]     │ ┌ Danger Zone ─────────────────────────────────┐ │
│ │ Max charge power [ ] Max discharge power [ ] │ │ Trip [requires confirmation]                  │ │
│ │                  [Apply limits]              │ │ Clear Protection [requires confirmation]       │ │
│ └──────────────────────────────────────────────┘ └──────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────────────────────────────────────┘
```

### Control — mobile (390 px)

```text
┌ SIMULATOR · Control ┐
│ [Operating state]   │
│ [Power command]     │
│ [Operating limits]  │
│ [Danger Zone]       │
└─────────────────────┘
```

Sections stack in this order. Each form keeps a visible label, unit, inline
validation, pending indicator, and a full-width submit control.

### Test Lab — desktop (1280 px)

```text
┌ SIMULATOR ── Overview / Demo | Control | Test Lab ── model time ──────────────────────────────┐
├ Test Lab                                                                                         ┤
│ ┌ Alarm injection ─────────────────────────────┐ ┌ Link fault simulation ──────────────────────┐ │
│ │ Search alarms [                         ]     │ │ Protocol: [IEC-104] [Modbus]                  │ │
│ │ BMS / Cell Temperature Too High               │ │ Mode: [drop|hang|delay|heartbeat pause]       │ │
│ │ [Inject] [Clear] — no inferred severity       │ │ Delay ms [     ] [Apply] [Restore link]       │ │
│ └───────────────────────────────────────────────┘ └──────────────────────────────────────────────┘ │
│ ┌ Reset simulator ──────────────────────────────────────────────────────────────────────────────┐ │
│ │ Restores the deterministic startup state.                                      [Reset…]        │ │
│ └────────────────────────────────────────────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────────────────────────────────────┘
```

### Test Lab — mobile (390 px)

```text
┌ SIMULATOR · Test Lab ┐
│ [Alarm search]       │
│ [Inject / Clear]     │
│ [Link fault controls]│
│ [Reset simulator]    │
└──────────────────────┘
```

## States and accessibility

- All navigation, controls, dialogs, and chart affordances are keyboard
  reachable in visual order. `Escape` closes dialogs without submitting.
- Focus is always visible with the `focus` token; colour contrast meets WCAG
  AA for text and interactive controls.
- Buttons have a disabled state while an API request is pending. Submission
  success appears only after a confirmed backend response; failures remain
  readable in the form and in a toast.
- Live telemetry uses a polite ARIA live region for meaningful state changes,
  not for every chart sample. Alarms use an assertive, textual banner.
- Loading surfaces use labels such as `Loading simulator state…`; errors offer
  `Retry stream` and preserve the last known timestamp as stale, never as
  current.
- Empty states explain the cause, for example `No active alarms` or `No
  scenario loaded`.
- The `Unconfirmed` label is shown next to every display configuration value
  whose API object has `unconfirmed: true`. No point-level unconfirmed marker
  is created because the catalog has no source for it.
- Charts include textual current values and accessible summaries; colour lines
  are not the only way to distinguish SoC, power, or temperature.

## Runtime and implementation constraints

- React, TypeScript, Vite, Apache ECharts, and local assets only.
- The browser uses `/api/v1/events?initial_state=true` for initial connection
  and resynchronisation. It reconnects after `resync_required` or a revision
  gap; it does not pair `/state` with a separate stream connection.
- No external fonts, CDN files, assets, analytics, or runtime internet calls.
- The compiled static bundle is embedded in the Go binary and served from the
  same loopback control-API port. API routes always take precedence over
  static routing.

