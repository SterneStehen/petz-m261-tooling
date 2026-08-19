import {
  FormEvent,
  createContext,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import * as echarts from "echarts";
import { api, type Point, type Status } from "./api";
import {
  useSimulator,
  type RareEvent,
  type Sample,
  type ScenarioStepEvent,
} from "./use-simulator";

type Page = "overview" | "control" | "lab";
type Toast = { tone: "success" | "error"; text: string } | null;
type Language = "en" | "uk";
type Text = Record<string, string>;

const en: Text = {
  overview: "Overview / Demo",
  control: "Control",
  lab: "Test Lab",
  connected: "Connected",
  offline: "Offline",
  connecting: "Connecting",
  loadingTime: "Loading time…",
  language: "Українська",
  liveView: "Live simulator view",
  currentState: "Current state from the simulator backend.",
  prepareDemo: "Prepare Demo",
  stateOfCharge: "State of charge",
  charging: "Charging",
  discharging: "Discharging",
  idle: "Idle",
  actualPower: "Actual power",
  requestedPower: "Requested power",
  minusCharge: "Minus = charge",
  activeAlarms: "Active alarms",
  noSeverity: "No inferred severity",
  alarmInjected: "Alarm injected:",
  physicalEffectUnconfirmed:
    "Its physical effect is unconfirmed by the documentation.",
  socAndPower: "SoC and power",
  batteryTemperature: "Battery temperature",
  systemState: "System state",
  mode: "Mode",
  remote: "Remote",
  manualAuto: "Manual / Auto",
  modelTime: "Model time",
  awaitingStream: "Awaiting stream",
  controlReadiness: "Control readiness",
  ready: "Ready",
  notReady: "Not ready",
  deviceSummary: "Device summary",
  online: "Online",
  simulatorConfiguration: "Simulator configuration",
  unconfirmed: "Unconfirmed",
  confirmedCommands: "Confirmed commands only",
  controlSubtitle:
    "Actions are shown as successful only after a backend response.",
  operatingState: "Operating state",
  power: "Power",
  powerOn: "Power On",
  powerOff: "Power Off",
  operatingMode: "Operating mode",
  manual: "Manual",
  autoStrategy: "Auto Strategy",
  activePower: "Active power",
  reactivePower: "Reactive power",
  negativePowerHint:
    "Negative active power means charging; positive active power means discharging.",
  operatingLimits: "Operating limits",
  maximumChargeSoc: "Maximum charge SoC",
  minimumDischargeSoc: "Minimum discharge SoC",
  maximumChargePower: "Maximum charge power",
  maximumDischargePower: "Maximum discharge power",
  sending: "Sending…",
  applyControls: "Apply controls",
  currentDispatch: "Current dispatch",
  dispatched: "Dispatched",
  reason: "Reason",
  backendLimits: "Backend limits are authoritative.",
  dangerZone: "Danger Zone",
  dangerDescription:
    "Dangerous commands need confirmation and can still be rejected by backend configuration.",
  trip: "Trip",
  clearProtection: "Clear Protection",
  confirmationRequired: "Confirmation required",
  cancel: "Cancel",
  confirmAction: "Confirm action",
  confirmTrip: "Confirm Trip",
  confirmClearProtection: "Confirm Clear Protection",
  dangerDetail:
    "This is a dangerous simulator command. The backend may still reject it if dangerous commands are disabled.",
  faultProtocol: "Fault injection and protocol testing",
  labSubtitle:
    "Alarm severity is not inferred when it is absent from the catalog.",
  alarmInjection: "Alarm injection",
  searchAlarms: "Search alarms",
  inject: "Inject",
  clear: "Clear",
  noMatchingAlarms: "No matching alarms.",
  linkFaultSimulation: "Link fault simulation",
  protocol: "Protocol",
  drop: "drop",
  hang: "hang",
  delay: "delay",
  heartbeatPause: "heartbeat_pause",
  apply: "Apply",
  restoreLink: "Restore link",
  resetSimulator: "Reset simulator",
  resetDescription:
    "Restores the deterministic startup state and stops active scenarios.",
  reset: "Reset…",
  confirmReset: "Confirm reset",
  eventReset: "Simulator reset was confirmed by the backend.",
  eventDiagnostic:
    "The simulator reported an accepted-but-unsupported command.",
  eventScenarioStep: "Scenario step {index} completed.",
  eventFault: "Fault {device} / {slug} changed.",
  linkFaultActive: "Active link fault: {protocol} / {mode}.",
  prepareSuccess: "Demo environment prepared and confirmed by the simulator.",
  controlAccepted: "Control values accepted by the simulator backend.",
  dangerUnsupported:
    "{label} was accepted, but has no modeled physical effect.",
  dangerAccepted: "{label} was accepted by the backend.",
  alarmInjectedConfirmed: "Alarm injected and confirmed.",
  alarmClearedConfirmed: "Alarm cleared and confirmed.",
  linkApplied: "Link fault applied and confirmed.",
  linkRestored: "Link restored and confirmed.",
  resetConfirmed: "Simulator reset and confirmed.",
  chartAria: "{title}: current value {value} {unit}",
  chartAriaDual:
    "{primaryTitle}: {primaryValue} {primaryUnit}; {secondaryTitle}: {secondaryValue} {secondaryUnit}",
  heartbeatTitle: "EMS Periodic Heartbeat Indicator",
  heartbeatLive: "Heartbeat live",
  heartbeatWaiting: "Awaiting first tick…",
  heartbeatLastTick: "Last tick {seconds}s ago",
  scenarioProgressTitle: "Scenario progress",
  recentEventsTitle: "Recent events",
  downloadLog: "Download log",
  allConfigUnconfirmed:
    "Unconfirmed: all parameters below are pending manufacturer confirmation.",
  eventTypeFault: "Fault",
  eventTypeReset: "Reset",
  eventTypeDiagnostic: "Diagnostic",
  eventTypeScenarioStep: "Scenario step",
  aboutButton: "About",
  aboutEyebrow: "M261 simulator console",
  aboutTitle: "About this program",
  close: "Close",
  aboutOriginal:
    "An original M261 simulator console. Its navigation and layout take only general organisational principles from public business-software examples — no third-party assets, copy, or code.",
  aboutCapabilities: "Capabilities",
  aboutPointCount: "{count} modeled data points",
  aboutDeviceCount: "{count} simulated devices",
  aboutProtocols:
    "Three protocol servers over one shared state: IEC-104, Modbus TCP, and an HTTP control API",
  aboutScenarioEngine:
    "A deterministic scenario engine driven by a single injectable clock — no wall-clock time in simulator logic",
  aboutLiveEvents:
    "A live event stream (Server-Sent Events) for telemetry, alarms, scenario progress, and diagnostics",
  aboutProtocolsHeading: "Protocols & interfaces",
  aboutProtocolModbus:
    "Modbus TCP on port 502 — read/write against the shared point store (functions 02/03/04/06/16)",
  aboutProtocolIec104:
    "IEC-104 on port 2404 — station/general interrogation, spontaneous transmission, single and setpoint commands",
  aboutProtocolControlApi:
    "HTTP control API on port 8081 (loopback-only by default) — fault injection, link-fault simulation, scenario playback, clock control, reset; no equivalent on the real M261",
  aboutAlarmsHeading: "Alarms & fault injection",
  aboutAlarmsCatalogDriven:
    "Every catalog point classified as an alarm can be injected or cleared individually, not just a fixed hand-picked list",
  aboutAlarmsSeverity:
    "Severity is shown only where the manufacturer catalog documents one; alarms with no documented severity are shown without an assumed level",
  aboutAlarmsControlApi:
    "Alarms can be triggered or cleared from the Test Lab, or directly via POST /faults and DELETE /faults/{device}/{point}",
  aboutAlarmsSearch: "Full-text search/filter across the alarm catalog for fast lookup during testing",
  aboutLinkFaultHeading: "Link fault simulation",
  aboutLinkFaultModes:
    "Four independent fault modes per protocol: connection drop, hang, delay, and EMS periodic-heartbeat pause",
  aboutLinkFaultPerProtocol:
    "IEC-104 and Modbus TCP links can be faulted independently, and cleared one at a time or all at once",
  aboutLinkFaultStatus:
    "The simulator reports back the authoritative link-fault status it actually applied, not just the request it received",
  aboutDangerHeading: "Danger zone commands",
  aboutDangerTripClear:
    "Trip and Clear Protection are modeled but gated behind a backend allow_dangerous configuration flag — rejected outright when it is disabled",
  aboutDangerConfirm:
    "The console requires an explicit confirmation step before dispatching either command",
  aboutScenarioHeading: "Scenario engine & guided demo",
  aboutScenarioLibrary:
    "Eight bundled scenarios covering restart, SoC/power limit violations, EMS link loss, alarm activation, repeated commands, and a 72-hour monitoring run",
  aboutScenarioProgress:
    "Scenario progress and individual step outcomes are reported live over the event stream",
  aboutGuidedDemo:
    "A one-click \"Prepare demo\" action drives the simulator into a ready, presentable state and waits for backend confirmation before the operator continues",
  aboutOpsHeading: "Diagnostics & operations",
  aboutDeterministicReset:
    "POST /reset restores the exact deterministic state right after process start, without restarting the process",
  aboutDiagnosticsReporting:
    "Commands the backend accepts but does not model an effect for are reported back as diagnostics rather than silently dropped",
  aboutHealthEndpoints:
    "Liveness and readiness endpoints (/health/live, /health/ready) for orchestration and monitoring",
  aboutEventLogPersisted:
    "The recent-events log is persisted across page reloads and can be downloaded as a file for offline analysis",
  aboutDeploymentHeading: "Deployment",
  aboutDeploymentDocker:
    "Ships as a single Docker Compose service; the control API stays loopback-only end to end, even behind Docker's port forwarding",
  aboutDeploymentLocalization:
    "Bilingual operator interface (English / Ukrainian) with a live language switch, no reload required",
  aboutTechnical: "Technical data (manufacturer specification)",
  aboutChemistry: "Chemistry: LFP",
  aboutCapacity: "System capacity: 261 kWh, nominal DC voltage 832 V",
  aboutDcRange: "DC voltage range: 676–936 V",
  aboutAcPower: "Nominal AC power: 130.5 kW (1.1× overload rating)",
  aboutCooling: "Cooling: liquid, 5 kW chiller",
  aboutUnconfirmed: "Unconfirmed parameters",
  aboutUnconfirmedPointer:
    "{count} configuration parameters are not yet confirmed by the manufacturer — see the \"Unconfirmed\" markers on the Overview configuration list.",
  aboutUnconfirmedNone:
    "No unconfirmed configuration parameters are currently reported by the backend.",
};
const uk: Text = {
  overview: "Огляд / Демонстрація",
  control: "Керування",
  lab: "Стенд",
  connected: "Підключено",
  offline: "Поза мережею",
  connecting: "Підключення",
  loadingTime: "Завантаження часу…",
  language: "English",
  liveView: "Перегляд симулятора",
  currentState: "Поточний стан із backend симулятора.",
  prepareDemo: "Підготувати демонстрацію",
  stateOfCharge: "Рівень заряду",
  charging: "Заряджання",
  discharging: "Розряджання",
  idle: "Очікування",
  actualPower: "Фактична потужність",
  requestedPower: "Запитана потужність",
  minusCharge: "Мінус = заряд",
  activeAlarms: "Активні аварії",
  noSeverity: "Без припущення про рівень",
  alarmInjected: "Аварію активовано:",
  physicalEffectUnconfirmed:
    "Її фізичний ефект не підтверджений документацією.",
  socAndPower: "SoC і потужність",
  batteryTemperature: "Температура батареї",
  systemState: "Стан системи",
  mode: "Режим",
  remote: "Дистанційний",
  manualAuto: "Ручний / Авто",
  modelTime: "Модельний час",
  awaitingStream: "Очікування потоку",
  controlReadiness: "Готовність керування",
  ready: "Готово",
  notReady: "Не готово",
  deviceSummary: "Підсумок пристроїв",
  online: "Онлайн",
  simulatorConfiguration: "Конфігурація симулятора",
  unconfirmed: "Непідтверджено",
  confirmedCommands: "Лише підтверджені команди",
  controlSubtitle: "Дії вважаються успішними лише після відповіді backend.",
  operatingState: "Робочий стан",
  power: "Живлення",
  powerOn: "Увімкнено",
  powerOff: "Вимкнено",
  operatingMode: "Робочий режим",
  manual: "Ручний",
  autoStrategy: "Автоматична стратегія",
  activePower: "Активна потужність",
  reactivePower: "Реактивна потужність",
  negativePowerHint:
    "Відʼємна активна потужність означає заряджання; додатна — розряджання.",
  operatingLimits: "Робочі ліміти",
  maximumChargeSoc: "Максимальний SoC заряду",
  minimumDischargeSoc: "Мінімальний SoC розряду",
  maximumChargePower: "Максимальна потужність заряду",
  maximumDischargePower: "Максимальна потужність розряду",
  sending: "Надсилання…",
  applyControls: "Застосувати керування",
  currentDispatch: "Поточна диспетчеризація",
  dispatched: "Диспетчеризовано",
  reason: "Причина",
  backendLimits: "Ліміти backend є авторитетними.",
  dangerZone: "Небезпечна зона",
  dangerDescription:
    "Небезпечні команди потребують підтвердження і можуть бути відхилені конфігурацією backend.",
  trip: "Trip",
  clearProtection: "Очистити захист",
  confirmationRequired: "Потрібне підтвердження",
  cancel: "Скасувати",
  confirmAction: "Підтвердити дію",
  confirmTrip: "Підтвердити Trip",
  confirmClearProtection: "Підтвердити очищення захисту",
  dangerDetail:
    "Це небезпечна команда симулятора. Backend може її відхилити, якщо небезпечні команди вимкнено.",
  faultProtocol: "Активація аварій і перевірка протоколів",
  labSubtitle:
    "Рівень серйозності аварії не визначається, якщо його немає в каталозі.",
  alarmInjection: "Активація аварії",
  searchAlarms: "Пошук аварій",
  inject: "Активувати",
  clear: "Очистити",
  noMatchingAlarms: "Немає відповідних аварій.",
  linkFaultSimulation: "Імітація збою звʼязку",
  protocol: "Протокол",
  drop: "Відключення",
  hang: "Зависання",
  delay: "Затримка",
  heartbeatPause: "Пауза heartbeat",
  apply: "Застосувати",
  restoreLink: "Відновити звʼязок",
  resetSimulator: "Скинути симулятор",
  resetDescription:
    "Повертає детермінований початковий стан і зупиняє активні сценарії.",
  reset: "Скинути…",
  confirmReset: "Підтвердити скидання",
  eventReset: "Скидання симулятора підтверджене backend.",
  eventDiagnostic:
    "Симулятор повідомив про прийняту, але непідтримувану команду.",
  eventScenarioStep: "Крок сценарію {index} завершено.",
  eventFault: "Стан аварії {device} / {slug} змінено.",
  linkFaultActive: "Активний збій звʼязку: {protocol} / {mode}.",
  prepareSuccess:
    "Середовище демонстрації підготовлено та підтверджено симулятором.",
  controlAccepted: "Значення керування прийняті backend симулятора.",
  dangerUnsupported:
    "{label} прийнято, але воно не має змодельованого фізичного ефекту.",
  dangerAccepted: "{label} прийнято backend.",
  alarmInjectedConfirmed: "Аварію активовано та підтверджено.",
  alarmClearedConfirmed: "Аварію очищено та підтверджено.",
  linkApplied: "Збій звʼязку застосовано та підтверджено.",
  linkRestored: "Звʼязок відновлено та підтверджено.",
  resetConfirmed: "Симулятор скинуто та підтверджено.",
  chartAria: "{title}: поточне значення {value} {unit}",
  chartAriaDual:
    "{primaryTitle}: {primaryValue} {primaryUnit}; {secondaryTitle}: {secondaryValue} {secondaryUnit}",
  heartbeatTitle: "Індикатор Periodic Heartbeat (EMS)",
  heartbeatLive: "Heartbeat активний",
  heartbeatWaiting: "Очікування першого тіку…",
  heartbeatLastTick: "Останній тік {seconds} с тому",
  scenarioProgressTitle: "Прогрес сценарію",
  recentEventsTitle: "Останні події",
  downloadLog: "Скачати лог",
  allConfigUnconfirmed:
    "Непідтверджено: усі параметри нижче ще не підтверджені виробником.",
  eventTypeFault: "Аварія",
  eventTypeReset: "Скидання",
  eventTypeDiagnostic: "Діагностика",
  eventTypeScenarioStep: "Крок сценарію",
  aboutButton: "Про програму",
  aboutEyebrow: "Консоль симулятора M261",
  aboutTitle: "Про цю програму",
  close: "Закрити",
  aboutOriginal:
    "Оригінальна консоль симулятора M261. Навігація та структура запозичують лише загальні організаційні принципи публічних прикладів бізнес-ПЗ — без сторонніх ассетів, текстів чи коду.",
  aboutCapabilities: "Можливості",
  aboutPointCount: "{count} змодельованих точок даних",
  aboutDeviceCount: "{count} симульованих пристроїв",
  aboutProtocols:
    "Три протокольні сервери над одним спільним станом: IEC-104, Modbus TCP та HTTP control API",
  aboutScenarioEngine:
    "Детермінований рушій сценаріїв на єдиному інжектованому годиннику — без реального часу в логіці симулятора",
  aboutLiveEvents:
    "Живий потік подій (Server-Sent Events) для телеметрії, аварій, прогресу сценарію та діагностики",
  aboutProtocolsHeading: "Протоколи та інтерфейси",
  aboutProtocolModbus:
    "Modbus TCP на порту 502 — читання/запис у спільне сховище точок (функції 02/03/04/06/16)",
  aboutProtocolIec104:
    "IEC-104 на порту 2404 — станційний/загальний опит, спонтанна передача, поодинокі команди та команди уставок",
  aboutProtocolControlApi:
    "HTTP control API на порту 8081 (лише loopback за замовчуванням) — активація аварій, імітація збою звʼязку, відтворення сценаріїв, керування годинником, скидання; аналога на реальному M261 немає",
  aboutAlarmsHeading: "Аварії та активація несправностей",
  aboutAlarmsCatalogDriven:
    "Активувати чи очистити можна будь-яку точку каталогу класу «аварія» — це не фіксований обмежений список",
  aboutAlarmsSeverity:
    "Рівень серйозності показується лише там, де він задокументований виробником у каталозі; аварії без задокументованого рівня показуються без припущення про нього",
  aboutAlarmsControlApi:
    "Аварії можна активувати чи очищати зі Стенду або напряму через POST /faults та DELETE /faults/{device}/{point}",
  aboutAlarmsSearch: "Повнотекстовий пошук/фільтр по каталогу аварій для швидкого пошуку під час тестування",
  aboutLinkFaultHeading: "Імітація збою звʼязку",
  aboutLinkFaultModes:
    "Чотири незалежні режими несправності на кожен протокол: відключення, зависання, затримка та пауза періодичного heartbeat EMS",
  aboutLinkFaultPerProtocol:
    "Звʼязок IEC-104 та Modbus TCP можна псувати незалежно, а очищати — по одному або всі одразу",
  aboutLinkFaultStatus:
    "Симулятор повідомляє авторитетний стан збою звʼязку, який він фактично застосував, а не лише отриманий запит",
  aboutDangerHeading: "Небезпечні команди",
  aboutDangerTripClear:
    "Trip та Clear Protection змодельовані, але приховані за прапорцем конфігурації backend allow_dangerous — за його вимкнення вони відхиляються повністю",
  aboutDangerConfirm:
    "Консоль вимагає явного підтвердження перед надсиланням будь-якої з цих команд",
  aboutScenarioHeading: "Рушій сценаріїв і супроводжувана демонстрація",
  aboutScenarioLibrary:
    "Вісім вбудованих сценаріїв: перезапуск, порушення лімітів SoC/потужності, втрата звʼязку з EMS, активація аварії, повторювані команди та 72-годинний моніторинг",
  aboutScenarioProgress:
    "Прогрес сценарію та результат кожного кроку повідомляються наживо через потік подій",
  aboutGuidedDemo:
    "Дія «Підготувати демонстрацію» одним кліком переводить симулятор у готовий, презентабельний стан і чекає на підтвердження backend, перш ніж оператор продовжить",
  aboutOpsHeading: "Діагностика та експлуатація",
  aboutDeterministicReset:
    "POST /reset повертає точний детермінований стан одразу після старту процесу — без перезапуску самого процесу",
  aboutDiagnosticsReporting:
    "Команди, які backend приймає, але не моделює для них ефекту, повідомляються як діагностика, а не тихо відкидаються",
  aboutHealthEndpoints:
    "Ендпоінти живучості та готовності (/health/live, /health/ready) для оркестрації та моніторингу",
  aboutEventLogPersisted:
    "Лог останніх подій зберігається між перезавантаженнями сторінки і може бути скачаний як файл для офлайн-аналізу",
  aboutDeploymentHeading: "Розгортання",
  aboutDeploymentDocker:
    "Постачається як єдиний сервіс Docker Compose; control API лишається доступним лише через loopback наскрізно, навіть за перенаправленням портів Docker",
  aboutDeploymentLocalization:
    "Двомовний інтерфейс оператора (англійська / українська) з живим перемиканням мови без перезавантаження",
  aboutTechnical: "Технічні дані (специфікація виробника)",
  aboutChemistry: "Хімія: LFP",
  aboutCapacity: "Ємність системи: 261 кВт·год, номінальна напруга DC 832 В",
  aboutDcRange: "Діапазон напруги DC: 676–936 В",
  aboutAcPower: "Номінальна потужність AC: 130,5 кВт (перевантаження 1,1×)",
  aboutCooling: "Охолодження: рідинне, чиллер 5 кВт",
  aboutUnconfirmed: "Непідтверджені параметри",
  aboutUnconfirmedPointer:
    "{count} параметрів конфігурації ще не підтверджені виробником — див. позначки «Непідтверджено» у списку конфігурації на Overview.",
  aboutUnconfirmedNone:
    "Наразі backend не повідомляє про непідтверджені параметри конфігурації.",
};
const uiText = { en, uk };
type LanguageState = {
  language: Language;
  locale: string;
  t: (key: string, values?: Record<string, string | number>) => string;
  switchLanguage: () => void;
};
const LanguageContext = createContext<LanguageState | null>(null);
function useLanguage() {
  const value = useContext(LanguageContext);
  if (!value) throw new Error("LanguageContext is unavailable");
  return value;
}
const point = (values: Record<string, number>, device: string, slug: string) =>
  values[`${device}/${slug}`] ?? 0;
const number = (value: number, locale: string, digits = 1) =>
  new Intl.NumberFormat(locale, {
    maximumFractionDigits: digits,
    minimumFractionDigits: digits,
  }).format(value);

function Chip({
  tone = "neutral",
  children,
}: {
  tone?: "neutral" | "good" | "active" | "warning" | "danger" | "offline";
  children: React.ReactNode;
}) {
  return (
    <span className={`chip chip--${tone}`}>
      <span aria-hidden="true">
        {tone === "warning"
          ? "!"
          : tone === "danger"
            ? "×"
            : tone === "good"
              ? "●"
              : "•"}
      </span>
      {children}
    </span>
  );
}
function Card({
  title,
  children,
  className = "",
}: {
  title: string;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <section className={`card ${className}`}>
      <h2>{title}</h2>
      {children}
    </section>
  );
}
function Metric({
  label,
  value,
  detail,
  tone = "neutral",
}: {
  label: string;
  value: string;
  detail?: string;
  tone?: "neutral" | "good" | "warning" | "danger";
}) {
  return (
    <section className={`metric metric--${tone}`}>
      <span>{label}</span>
      <strong>{value}</strong>
      {detail && <small>{detail}</small>}
    </section>
  );
}
// Task 10.1 item 5: shared modal shell -- both ConfirmationDialog and the
// new AboutDialog use the same focus-management/Escape/backdrop logic
// instead of duplicating it. Focus goes to the first focusable descendant
// on mount (for ConfirmationDialog that's still exactly the Cancel button,
// same as before this was extracted), Tab is trapped inside, Escape closes.
function Modal({
  labelId,
  onClose,
  className = "",
  children,
}: {
  labelId: string;
  onClose: () => void;
  className?: string;
  children: React.ReactNode;
}) {
  const dialogRef = useRef<HTMLElement>(null);
  useEffect(() => {
    const focusable = dialogRef.current?.querySelectorAll<HTMLElement>(
      'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
    );
    focusable?.[0]?.focus();
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = previousOverflow;
    };
  }, []);
  const trapFocus = (event: React.KeyboardEvent<HTMLElement>) => {
    if (event.key === "Escape") {
      event.preventDefault();
      onClose();
      return;
    }
    if (event.key !== "Tab") return;
    const focusable = dialogRef.current?.querySelectorAll<HTMLElement>(
      'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
    );
    if (!focusable?.length) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  };
  return (
    <div className="dialog-backdrop" role="presentation" onMouseDown={(event) => event.preventDefault()}>
      <section
        ref={dialogRef}
        className={`dialog ${className}`}
        role="dialog"
        aria-modal="true"
        aria-labelledby={labelId}
        onKeyDown={trapFocus}
      >
        {children}
      </section>
    </div>
  );
}
function ConfirmationDialog({
  title,
  detail,
  onCancel,
  onConfirm,
}: {
  title: string;
  detail: string;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const { t } = useLanguage();
  return (
    <Modal labelId="dialog-title" onClose={onCancel}>
      <p className="eyebrow">{t("confirmationRequired")}</p>
      <h2 id="dialog-title">{title}</h2>
      <p>{detail}</p>
      <div className="button-row">
        <button className="button button--secondary" onClick={onCancel}>
          {t("cancel")}
        </button>
        <button className="button button--danger" onClick={onConfirm}>
          {t("confirmAction")}
        </button>
      </div>
    </Modal>
  );
}
function AboutDialog({
  onClose,
  catalog,
  status,
}: {
  onClose: () => void;
  catalog: Point[];
  status: Status | null;
}) {
  const { t } = useLanguage();
  const pointCount = catalog.length;
  const deviceCount = useMemo(
    () => new Set(catalog.map((item) => item.device)).size,
    [catalog],
  );
  const unconfirmedCount = Object.values(status?.configuration ?? {}).filter(
    (setting) => setting.unconfirmed,
  ).length;
  return (
    <Modal labelId="about-title" onClose={onClose} className="about-dialog">
      <div className="about-header">
        <div>
          <p className="eyebrow">{t("aboutEyebrow")}</p>
          <h2 id="about-title">{t("aboutTitle")}</h2>
        </div>
        <button className="button button--secondary" onClick={onClose}>
          {t("close")}
        </button>
      </div>
      <p>{t("aboutOriginal")}</p>
      <section className="about-section">
        <h3>{t("aboutCapabilities")}</h3>
        <ul>
          <li>{t("aboutPointCount", { count: pointCount })}</li>
          <li>{t("aboutDeviceCount", { count: deviceCount })}</li>
          <li>{t("aboutProtocols")}</li>
          <li>{t("aboutScenarioEngine")}</li>
          <li>{t("aboutLiveEvents")}</li>
        </ul>
      </section>
      <section className="about-section">
        <h3>{t("aboutProtocolsHeading")}</h3>
        <ul>
          <li>{t("aboutProtocolModbus")}</li>
          <li>{t("aboutProtocolIec104")}</li>
          <li>{t("aboutProtocolControlApi")}</li>
        </ul>
      </section>
      <section className="about-section">
        <h3>{t("aboutAlarmsHeading")}</h3>
        <ul>
          <li>{t("aboutAlarmsCatalogDriven")}</li>
          <li>{t("aboutAlarmsSeverity")}</li>
          <li>{t("aboutAlarmsControlApi")}</li>
          <li>{t("aboutAlarmsSearch")}</li>
        </ul>
      </section>
      <section className="about-section">
        <h3>{t("aboutLinkFaultHeading")}</h3>
        <ul>
          <li>{t("aboutLinkFaultModes")}</li>
          <li>{t("aboutLinkFaultPerProtocol")}</li>
          <li>{t("aboutLinkFaultStatus")}</li>
        </ul>
      </section>
      <section className="about-section">
        <h3>{t("aboutDangerHeading")}</h3>
        <ul>
          <li>{t("aboutDangerTripClear")}</li>
          <li>{t("aboutDangerConfirm")}</li>
        </ul>
      </section>
      <section className="about-section">
        <h3>{t("aboutScenarioHeading")}</h3>
        <ul>
          <li>{t("aboutScenarioLibrary")}</li>
          <li>{t("aboutScenarioProgress")}</li>
          <li>{t("aboutGuidedDemo")}</li>
        </ul>
      </section>
      <section className="about-section">
        <h3>{t("aboutOpsHeading")}</h3>
        <ul>
          <li>{t("aboutDeterministicReset")}</li>
          <li>{t("aboutDiagnosticsReporting")}</li>
          <li>{t("aboutHealthEndpoints")}</li>
          <li>{t("aboutEventLogPersisted")}</li>
        </ul>
      </section>
      <section className="about-section">
        <h3>{t("aboutDeploymentHeading")}</h3>
        <ul>
          <li>{t("aboutDeploymentDocker")}</li>
          <li>{t("aboutDeploymentLocalization")}</li>
        </ul>
      </section>
      <section className="about-section">
        <h3>{t("aboutTechnical")}</h3>
        <ul>
          <li>{t("aboutChemistry")}</li>
          <li>{t("aboutCapacity")}</li>
          <li>{t("aboutDcRange")}</li>
          <li>{t("aboutAcPower")}</li>
          <li>{t("aboutCooling")}</li>
        </ul>
      </section>
      <section className="about-section">
        <h3>{t("aboutUnconfirmed")}</h3>
        <p>
          {unconfirmedCount > 0
            ? t("aboutUnconfirmedPointer", { count: unconfirmedCount })
            : t("aboutUnconfirmedNone")}
        </p>
      </section>
    </Modal>
  );
}
// Shared axis/grid/tooltip options so single- and dual-series charts read
// the same way: a real time axis (actual elapsed wall-clock time between
// observed samples, not an evenly-spaced fake index) with visible
// gridlines, so the chart is readable on its own, not just decorative.
function baseChartOption(locale: string) {
  return {
    animation: false,
    grid: { left: 40, right: 12, top: 28, bottom: 22 },
    tooltip: {
      trigger: "axis" as const,
      valueFormatter: (value: unknown) => number(Number(value), locale, 2),
    },
    xAxis: {
      type: "time" as const,
      axisLabel: {
        color: "#626862",
        fontSize: 10,
        formatter: (value: number) => new Date(value).toLocaleTimeString(locale, { hour12: false }),
      },
      axisLine: { lineStyle: { color: "#dddcd5" } },
      splitLine: { show: false },
    },
  };
}
function MiniChart({
  title,
  value,
  samples,
  unit,
  tone,
}: {
  title: string;
  value: number;
  samples: Sample[];
  unit: string;
  tone: string;
}) {
  const { locale, t } = useLanguage();
  const [chartNode, setChartNode] = useState<HTMLDivElement | null>(null);
  useEffect(() => {
    const node = chartNode;
    if (!node) return;
    const chart = echarts.init(node, undefined, { renderer: "svg" });
    const data = samples.length
      ? samples.map((s) => [s.t, s.value])
      : [[Date.now(), value]];
    chart.setOption({
      ...baseChartOption(locale),
      yAxis: {
        type: "value",
        axisLabel: { color: "#626862", fontSize: 10 },
        axisLine: { show: false },
        splitLine: { lineStyle: { color: "#f0f0eb" } },
      },
      series: [
        {
          name: title,
          type: "line",
          data,
          smooth: true,
          symbol: "none",
          lineStyle: { width: 2, color: tone },
          areaStyle: { color: `${tone}22` },
        },
      ],
    });
    const observer = new ResizeObserver(() => chart.resize());
    observer.observe(node);
    const frame = requestAnimationFrame(() => chart.resize());
    return () => {
      cancelAnimationFrame(frame);
      observer.disconnect();
      chart.dispose();
    };
  }, [chartNode, samples, tone, value, locale, title]);
  const display = number(value, locale);
  return (
    <Card title={title} className="chart-card">
      <div className="chart-value">
        {display} <small>{unit}</small>
      </div>
      <div
        ref={setChartNode}
        className="chart"
        role="img"
        aria-label={t("chartAria", { title, value: display, unit })}
      />
    </Card>
  );
}

// Task 10.1 item 2: the "SoC and power" card's real second series -- both
// samples arrays come from useSimulator's already-accumulated client-side
// history of actually-received point values, never synthesised.
function DualChart({
  title,
  primaryLabel,
  primaryValue,
  primarySamples,
  primaryUnit,
  primaryTone,
  secondaryLabel,
  secondaryValue,
  secondarySamples,
  secondaryUnit,
  secondaryTone,
}: {
  title: string;
  primaryLabel: string;
  primaryValue: number;
  primarySamples: Sample[];
  primaryUnit: string;
  primaryTone: string;
  secondaryLabel: string;
  secondaryValue: number;
  secondarySamples: Sample[];
  secondaryUnit: string;
  secondaryTone: string;
}) {
  const { locale, t } = useLanguage();
  const [chartNode, setChartNode] = useState<HTMLDivElement | null>(null);
  useEffect(() => {
    const node = chartNode;
    if (!node) return;
    const chart = echarts.init(node, undefined, { renderer: "svg" });
    const now = Date.now();
    const primaryData = primarySamples.length
      ? primarySamples.map((s) => [s.t, s.value])
      : [[now, primaryValue]];
    const secondaryData = secondarySamples.length
      ? secondarySamples.map((s) => [s.t, s.value])
      : [[now, secondaryValue]];
    chart.setOption({
      ...baseChartOption(locale),
      legend: {
        data: [primaryLabel, secondaryLabel],
        top: 0,
        left: 0,
        icon: "circle",
        itemWidth: 8,
        itemHeight: 8,
        textStyle: { color: "#626862", fontSize: 11 },
      },
      grid: { left: 40, right: 40, top: 48, bottom: 22 },
      yAxis: [
        {
          type: "value",
          axisLabel: { color: primaryTone, fontSize: 10 },
          axisLine: { show: false },
          splitLine: { lineStyle: { color: "#f0f0eb" } },
        },
        {
          type: "value",
          axisLabel: { color: secondaryTone, fontSize: 10 },
          axisLine: { show: false },
          splitLine: { show: false },
        },
      ],
      series: [
        {
          name: primaryLabel,
          type: "line",
          yAxisIndex: 0,
          data: primaryData,
          smooth: true,
          symbol: "none",
          lineStyle: { width: 2, color: primaryTone },
          areaStyle: { color: `${primaryTone}22` },
        },
        {
          name: secondaryLabel,
          type: "line",
          yAxisIndex: 1,
          data: secondaryData,
          smooth: true,
          symbol: "none",
          lineStyle: { width: 2, color: secondaryTone },
        },
      ],
    });
    const observer = new ResizeObserver(() => chart.resize());
    observer.observe(node);
    const frame = requestAnimationFrame(() => chart.resize());
    return () => {
      cancelAnimationFrame(frame);
      observer.disconnect();
      chart.dispose();
    };
  }, [
    chartNode,
    primarySamples,
    secondarySamples,
    primaryTone,
    secondaryTone,
    primaryValue,
    secondaryValue,
    primaryLabel,
    secondaryLabel,
    locale,
  ]);
  const primaryDisplay = number(primaryValue, locale);
  const secondaryDisplay = number(secondaryValue, locale);
  return (
    <Card title={title} className="chart-card">
      <div className="chart-value chart-value--dual">
        <span style={{ color: primaryTone }}>
          {primaryDisplay} <small>{primaryUnit}</small>
        </span>
        <span style={{ color: secondaryTone }}>
          {secondaryDisplay} <small>{secondaryUnit}</small>
        </span>
      </div>
      <div
        ref={setChartNode}
        className="chart chart--dual"
        role="img"
        aria-label={t("chartAriaDual", {
          primaryTitle: primaryLabel,
          primaryValue: primaryDisplay,
          primaryUnit,
          secondaryTitle: secondaryLabel,
          secondaryValue: secondaryDisplay,
          secondaryUnit,
        })}
      />
    </Card>
  );
}

// Task 10.1 item 3: heartbeat is identified by useSimulator's catalog-driven
// heartbeatKey (see HEARTBEAT_POINT_NAME there) -- this component only ever
// receives a timestamp, never a point identifier of its own.
function HeartbeatIndicator({ lastTickAt }: { lastTickAt: number | null }) {
  const { t } = useLanguage();
  const [prefersReducedMotion, setPrefersReducedMotion] = useState(
    () =>
      typeof window !== "undefined" &&
      window.matchMedia("(prefers-reduced-motion: reduce)").matches,
  );
  useEffect(() => {
    const media = window.matchMedia("(prefers-reduced-motion: reduce)");
    const handler = () => setPrefersReducedMotion(media.matches);
    media.addEventListener("change", handler);
    return () => media.removeEventListener("change", handler);
  }, []);
  const [secondsAgo, setSecondsAgo] = useState<number | null>(null);
  useEffect(() => {
    if (lastTickAt === null) return;
    const update = () =>
      setSecondsAgo(Math.max(0, Math.round((Date.now() - lastTickAt) / 1000)));
    update();
    const timer = window.setInterval(update, 1000);
    return () => window.clearInterval(timer);
  }, [lastTickAt]);
  // Always render the same element shape, whether or not a tick has been
  // observed yet -- toggling between null and a real element shifts page
  // layout depending purely on real-world timing of the first physics
  // tick, which made this element source of nondeterministic screenshot
  // diffs. The "awaiting" label is still honest: it never claims a tick
  // happened before one actually did.
  return (
    <span className="heartbeat" title={t("heartbeatTitle")}>
      {lastTickAt !== null && !prefersReducedMotion && (
        <span key={lastTickAt} className="heartbeat-pulse" aria-hidden="true" />
      )}
      <span className="heartbeat-label">
        {lastTickAt === null
          ? t("heartbeatWaiting")
          : prefersReducedMotion
            ? t("heartbeatLastTick", { seconds: String(secondsAgo ?? 0) })
            : t("heartbeatLive")}
      </span>
    </span>
  );
}

// Reused by both the toast effect in App() and RecentEvents below, so the
// two never describe the same event differently.
function describeEvent(
  event: RareEvent,
  t: (key: string, values?: Record<string, string | number>) => string,
) {
  const p = event.payload;
  switch (event.type) {
    case "reset":
      return t("eventReset");
    case "diagnostic":
      return t("eventDiagnostic");
    case "scenario_step":
      return t("eventScenarioStep", { index: String(p.index ?? "") });
    case "fault":
      return t("eventFault", {
        device: String(p.device ?? ""),
        slug: String(p.slug ?? ""),
      });
    default:
      return "";
  }
}
function eventTypeLabel(
  type: RareEvent["type"],
  t: (key: string, values?: Record<string, string | number>) => string,
) {
  switch (type) {
    case "fault":
      return t("eventTypeFault");
    case "reset":
      return t("eventTypeReset");
    case "diagnostic":
      return t("eventTypeDiagnostic");
    case "scenario_step":
      return t("eventTypeScenarioStep");
  }
}

// Task 10.1 item 1: only ever renders steps that have already executed
// (each entry arrives from a real scenario_step SSE event) -- never a
// predicted/future step list. The most recent non-failed step is marked
// "latest", not "current": the event stream only tells us a step finished,
// never that a later one is currently in progress.
function ScenarioProgress({ steps }: { steps: ScenarioStepEvent[] }) {
  const { t } = useLanguage();
  if (steps.length === 0) return null;
  const lastIndex = steps.length - 1;
  return (
    <Card title={t("scenarioProgressTitle")}>
      <ol className="scenario-progress">
        {steps.map((step, index) => (
          <li
            key={`${step.scenario}-${step.index}`}
            className={
              step.result === "failed"
                ? "scenario-step scenario-step--failed"
                : index === lastIndex
                  ? "scenario-step scenario-step--latest"
                  : "scenario-step scenario-step--passed"
            }
          >
            <span className="scenario-step-index">{step.index + 1}</span>
            <span className="scenario-step-action">{step.action}</span>
            {step.result === "failed" && step.error && (
              <span className="scenario-step-error">{step.error}</span>
            )}
          </li>
        ))}
      </ol>
    </Card>
  );
}

// Task 10.1 item 4: capped, most-recent-first log -- useSimulator already
// gates this list on initial_replay_complete the same way toasts are
// gated, so a fresh connection's history replay never shows up here as
// something that just happened.
// Client-side only: serialises exactly the events currently shown, no
// backend involved.
function downloadEventsLog(events: RareEvent[]) {
  const blob = new Blob([JSON.stringify(events, null, 2)], {
    type: "application/json",
  });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = `m261-events-${new Date().toISOString().replace(/[:.]/g, "-")}.json`;
  document.body.appendChild(link);
  link.click();
  document.body.removeChild(link);
  URL.revokeObjectURL(url);
}
function RecentEvents({ events }: { events: RareEvent[] }) {
  const { t, locale } = useLanguage();
  if (events.length === 0) return null;
  return (
    <section className="card">
      <div className="events-header">
        <h2>{t("recentEventsTitle")}</h2>
        <button
          className="button button--secondary button--small"
          onClick={() => downloadEventsLog(events)}
        >
          {t("downloadLog")}
        </button>
      </div>
      <ul className="recent-events">
        {events.map((event) => (
          <li
            key={event.id}
            className={`recent-event recent-event--${event.type}`}
          >
            <span className="recent-event-time">
              {new Date(event.timestamp).toLocaleTimeString(locale)}
            </span>
            <span className="recent-event-type">
              {eventTypeLabel(event.type, t)}
            </span>
            <span className="recent-event-detail">{describeEvent(event, t)}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}

export function App() {
  const simulator = useSimulator();
  const [page, setPage] = useState<Page>("overview");
  const [language, setLanguage] = useState<Language>(() =>
    localStorage.getItem("m261-language") === "uk" ? "uk" : "en",
  );
  const [toast, setToast] = useState<Toast>(null);
  const locale = language === "uk" ? "uk-UA" : "en-US";
  const t = (key: string, values: Record<string, string | number> = {}) =>
    uiText[language][key].replace(/\{(\w+)\}/g, (_, name) =>
      String(values[name] ?? ""),
    );
  const switchLanguage = () =>
    setLanguage((current) => {
      const next = current === "en" ? "uk" : "en";
      localStorage.setItem("m261-language", next);
      return next;
    });
  const notify = (next: Toast) => {
    setToast(next);
    if (next) window.setTimeout(() => setToast(null), 5000);
  };
  const [aboutOpen, setAboutOpen] = useState(false);
  useEffect(() => {
    if (!simulator.lastEvent) return;
    notify({
      tone: simulator.lastEvent.type === "fault" ? "error" : "success",
      text: describeEvent(simulator.lastEvent, t),
    });
  }, [simulator.lastEvent?.id, language]);
  const props = { ...simulator, notify };
  return (
    <LanguageContext.Provider value={{ language, locale, t, switchLanguage }}>
      <div className="app-shell" lang={language}>
        <header className="topbar">
          <div className="brand">
            M261 <b>SIMULATOR</b>
          </div>
          <nav aria-label="Console">
            <button
              className={page === "overview" ? "is-active" : ""}
              onClick={() => setPage("overview")}
            >
              {t("overview")}
            </button>
            <button
              className={page === "control" ? "is-active" : ""}
              onClick={() => setPage("control")}
            >
              {t("control")}
            </button>
            <button
              className={page === "lab" ? "is-active" : ""}
              onClick={() => setPage("lab")}
            >
              {t("lab")}
            </button>
          </nav>
          <div className="topbar-status">
            <span className="model-time">
              {simulator.modelTime
                ? new Date(simulator.modelTime).toLocaleString(locale)
                : t("loadingTime")}
            </span>
            <HeartbeatIndicator lastTickAt={simulator.heartbeatTick} />
            <Chip
              tone={
                simulator.stream === "connected"
                  ? "good"
                  : simulator.stream === "offline"
                    ? "offline"
                    : "warning"
              }
            >
              {t(
                simulator.stream === "connected"
                  ? "connected"
                  : simulator.stream === "offline"
                    ? "offline"
                    : "connecting",
              )}
            </Chip>
            <button
              className="language-switch"
              onClick={switchLanguage}
              aria-label="Switch interface language"
            >
              {t("language")}
            </button>
            <button
              className="topbar-button"
              onClick={() => setAboutOpen(true)}
            >
              {t("aboutButton")}
            </button>
          </div>
        </header>
        <main>
          {page === "overview" && <Overview {...props} />}{" "}
          {page === "control" && <Control {...props} />}{" "}
          {page === "lab" && <TestLab {...props} />}
        </main>
        {toast && (
          <div className={`toast toast--${toast.tone}`} role="status">
            {toast.text}
          </div>
        )}
        {aboutOpen && (
          <AboutDialog
            onClose={() => setAboutOpen(false)}
            catalog={simulator.catalog}
            status={simulator.status}
          />
        )}
      </div>
    </LanguageContext.Provider>
  );
}

function Overview({
  points,
  catalog,
  history,
  status,
  modelTime,
  notify,
  lastEvent,
  events,
  scenarioProgress,
}: ReturnType<typeof useSimulator> & { notify: (toast: Toast) => void }) {
  const { locale, t } = useLanguage();
  const soc = point(points, "BMS", "soc"),
    power = point(points, "EMS", "last_charge_discharge_power_kw"),
    requested = point(points, "EMS", "desired_active_power_kw"),
    temperature = point(points, "BMS", "average_temperature_c"),
    alarmCount = catalog.filter(
      (item) =>
        item.class === "alarm" && point(points, item.device, item.slug) !== 0,
    ).length;
  const prepare = async () => {
    try {
      await api.demoPrepare();
      notify({ tone: "success", text: t("prepareSuccess") });
    } catch (error) {
      notify({ tone: "error", text: (error as Error).message });
    }
  };
  return (
    <>
      <div className="page-heading">
        <div>
          <p className="eyebrow">{t("liveView")}</p>
          <h1>{t("overview")}</h1>
          <p>{t("currentState")}</p>
        </div>
        <button
          className="button button--primary"
          onClick={() => void prepare()}
        >
          {t("prepareDemo")}
        </button>
      </div>
      <div className="metric-grid">
        <Metric
          label={t("stateOfCharge")}
          value={`${number(soc, locale)} %`}
          detail="BMS"
          tone="good"
        />
        <Metric
          label={
            power < 0 ? t("charging") : power > 0 ? t("discharging") : t("idle")
          }
          value={`${number(Math.abs(power), locale)} kW`}
          detail={t("actualPower")}
          tone={power === 0 ? "neutral" : "good"}
        />
        <Metric
          label={t("requestedPower")}
          value={`${number(requested, locale)} kW`}
          detail={t("minusCharge")}
        />
        <Metric
          label={t("activeAlarms")}
          value={String(alarmCount)}
          detail={t("noSeverity")}
          tone={alarmCount ? "danger" : "good"}
        />
      </div>
      {lastEvent?.type === "fault" && Number(lastEvent.payload.value) !== 0 && (
        <div className="event-banner" role="alert">
          <b>{t("alarmInjected")}</b> {String(lastEvent.payload.device)}/
          {String(lastEvent.payload.slug)}. {t("physicalEffectUnconfirmed")}
        </div>
      )}
      {Object.entries(status?.link_faults ?? {}).map(([protocol, mode]) => (
        <div className="event-banner" role="status" key={protocol}>
          <b>{t("linkFaultActive", { protocol, mode: mode.join(", ") })}</b>
        </div>
      ))}
      <div className="dashboard-grid">
        <DualChart
          title={t("socAndPower")}
          primaryLabel={t("stateOfCharge")}
          primaryValue={soc}
          primarySamples={history["BMS/soc"] ?? []}
          primaryUnit="%"
          primaryTone="#2367D1"
          secondaryLabel={t("actualPower")}
          secondaryValue={power}
          secondarySamples={history["EMS/last_charge_discharge_power_kw"] ?? []}
          secondaryUnit="kW"
          secondaryTone="#1C7C54"
        />
        <MiniChart
          title={t("batteryTemperature")}
          value={temperature}
          samples={history["BMS/average_temperature_c"] ?? []}
          unit="°C"
          tone="#A86500"
        />
        <Card title={t("systemState")}>
          <dl className="definition-list">
            <dt>{t("mode")}</dt>
            <dd>
              {point(points, "EMS", "set_operating_mode") === 2
                ? t("remote")
                : t("manualAuto")}
            </dd>
            <dt>{t("modelTime")}</dt>
            <dd>
              {modelTime
                ? new Date(modelTime).toLocaleString(locale)
                : t("awaitingStream")}
            </dd>
            <dt>{t("controlReadiness")}</dt>
            <dd>
              <Chip tone={status?.ready ? "good" : "warning"}>
                {status?.ready ? t("ready") : t("notReady")}
              </Chip>
            </dd>
          </dl>
        </Card>
        <Card title={t("deviceSummary")}>
          <div className="device-list">
            {["EMS", "PCS", "BMS", "PCS_METER"].map((device) => (
              <div key={device}>
                <span>{device}</span>
                <Chip tone="good">{t("online")}</Chip>
              </div>
            ))}
          </div>
        </Card>
      </div>
      <Card title={t("simulatorConfiguration")}>
        {(() => {
          const configEntries = Object.entries(status?.configuration ?? {});
          // Right now every listed parameter is unconfirmed (§7) -- an
          // identical badge on every single row is noise, not information.
          // A single explicit note carries the same meaning without
          // repeating it. If a parameter is ever actually confirmed, this
          // falls back to per-row badges automatically, so a genuinely
          // mixed state is never silently flattened into "nothing is
          // marked".
          const allUnconfirmed =
            configEntries.length > 0 &&
            configEntries.every(([, setting]) => setting.unconfirmed);
          return (
            <>
              {allUnconfirmed && (
                <p className="config-note">{t("allConfigUnconfirmed")}</p>
              )}
              <div className="config-list">
                {configEntries.map(([key, setting]) => (
                  <div key={key}>
                    <span>{key}</span>
                    <span>{String(setting.value)}</span>
                    {!allUnconfirmed && setting.unconfirmed && (
                      <Chip tone="warning">{t("unconfirmed")}</Chip>
                    )}
                  </div>
                ))}
              </div>
            </>
          );
        })()}
      </Card>
      <ScenarioProgress steps={scenarioProgress} />
      <RecentEvents events={events} />
    </>
  );
}

function useSyncedField(backendValue: number) {
  const touched = useRef(false);
  const [value, setValue] = useState(String(backendValue));
  useEffect(() => {
    if (!touched.current) setValue(String(backendValue));
  }, [backendValue]);
  return {
    value,
    set: (next: string) => {
      touched.current = true;
      setValue(next);
    },
    release: () => {
      touched.current = false;
      setValue(String(backendValue));
    },
  };
}

function Control({
  points,
  notify,
}: ReturnType<typeof useSimulator> & { notify: (toast: Toast) => void }) {
  const { locale, t } = useLanguage();
  const power = useSyncedField(point(points, "EMS", "set_active_power_kw"));
  const reactive = useSyncedField(point(points, "EMS", "set_reactive_power_kvar"));
  const mode = useSyncedField(Math.round(point(points, "EMS", "set_operating_mode")));
  const on = useSyncedField(Math.round(point(points, "EMS", "power_on_off")));
  const maxChargeSOC = useSyncedField(Math.round(point(points, "EMS", "maximum_charge_soc")));
  const minDischargeSOC = useSyncedField(Math.round(point(points, "EMS", "minimum_discharge_soc")));
  const maxChargePower = useSyncedField(Math.round(point(points, "EMS", "system_maximum_charge_power")));
  const maxDischargePower = useSyncedField(Math.round(point(points, "EMS", "system_maximum_discharge_power")));
  const [pending, setPending] = useState(false),
    [danger, setDanger] = useState<{ slug: string; labelKey: string } | null>(
      null,
    );
  const send = async (event: FormEvent) => {
    event.preventDefault();
    setPending(true);
    try {
      await api.command("EMS", "power_on_off", Number(on.value));
      await api.command("EMS", "set_operating_mode", Number(mode.value));
      await api.command("EMS", "set_active_power_kw", Number(power.value));
      await api.command("EMS", "set_reactive_power_kvar", Number(reactive.value));
      await api.command("EMS", "maximum_charge_soc", Number(maxChargeSOC.value));
      await api.command(
        "EMS",
        "minimum_discharge_soc",
        Number(minDischargeSOC.value),
      );
      await api.command(
        "EMS",
        "system_maximum_charge_power",
        Math.round(Number(maxChargePower.value)),
      );
      await api.command(
        "EMS",
        "system_maximum_discharge_power",
        Math.round(Number(maxDischargePower.value)),
      );
      notify({ tone: "success", text: t("controlAccepted") });
      [power, reactive, mode, on, maxChargeSOC, minDischargeSOC, maxChargePower, maxDischargePower].forEach((field) => field.release());
    } catch (error) {
      notify({ tone: "error", text: (error as Error).message });
    } finally {
      setPending(false);
    }
  };
  const confirmDanger = async () => {
    if (!danger) return;
    try {
      const response = await api.command("EMS", danger.slug, 1),
        label = t(danger.labelKey);
      notify({
        tone:
          response.diagnostic?.code === "accepted_but_unsupported"
            ? "error"
            : "success",
        text:
          response.diagnostic?.code === "accepted_but_unsupported"
            ? t("dangerUnsupported", { label })
            : t("dangerAccepted", { label }),
      });
    } catch (error) {
      notify({ tone: "error", text: (error as Error).message });
    } finally {
      setDanger(null);
    }
  };
  return (
    <>
      <div className="page-heading">
        <div>
          <p className="eyebrow">{t("confirmedCommands")}</p>
          <h1>{t("control")}</h1>
          <p>{t("controlSubtitle")}</p>
        </div>
      </div>
      <form onSubmit={send} className="control-grid">
        <Card title={t("operatingState")}>
          <label>
            {t("power")}
            <select value={on.value} onChange={(e) => on.set(e.target.value)}>
              <option value="1">{t("powerOn")}</option>
              <option value="0">{t("powerOff")}</option>
            </select>
          </label>
          <label>
            {t("operatingMode")}
            <select value={mode.value} onChange={(e) => mode.set(e.target.value)}>
              <option value="0">{t("manual")}</option>
              <option value="1">{t("autoStrategy")}</option>
              <option value="2">{t("remote")}</option>
            </select>
          </label>
          <label>
            {t("activePower")} <span>kW</span>
            <input
              type="number"
              value={power.value}
              onChange={(e) => power.set(e.target.value)}
              step="1"
            />
          </label>
          <label>
            {t("reactivePower")} <span>kvar</span>
            <input
              type="number"
              value={reactive.value}
              onChange={(e) => reactive.set(e.target.value)}
              step="1"
            />
          </label>
          <p className="form-hint">{t("negativePowerHint")}</p>
        </Card>
        <Card title={t("operatingLimits")}>
          <label>
            {t("maximumChargeSoc")} <span>%</span>
            <input
              type="number"
              value={maxChargeSOC.value}
              onChange={(e) => maxChargeSOC.set(e.target.value)}
            />
          </label>
          <label>
            {t("minimumDischargeSoc")} <span>%</span>
            <input
              type="number"
              value={minDischargeSOC.value}
              onChange={(e) => minDischargeSOC.set(e.target.value)}
            />
          </label>
          <label>
            {t("maximumChargePower")} <span>kW</span>
            <input
              type="number"
              value={maxChargePower.value}
              onChange={(e) => maxChargePower.set(e.target.value)}
            />
          </label>
          <label>
            {t("maximumDischargePower")} <span>kW</span>
            <input
              type="number"
              value={maxDischargePower.value}
              onChange={(e) => maxDischargePower.set(e.target.value)}
            />
          </label>
          <button className="button button--primary" disabled={pending}>
            {pending ? t("sending") : t("applyControls")}
          </button>
        </Card>
        <Card title={t("currentDispatch")}>
          <dl className="definition-list">
            <dt>{t("requestedPower")}</dt>
            <dd>
              {number(point(points, "EMS", "desired_active_power_kw"), locale)}{" "}
              kW
            </dd>
            <dt>{t("dispatched")}</dt>
            <dd>
              {number(
                point(points, "EMS", "last_charge_discharge_power_kw"),
                locale,
              )}{" "}
              kW
            </dd>
            <dt>{t("reason")}</dt>
            <dd>{t("backendLimits")}</dd>
          </dl>
        </Card>
        <Card title={t("dangerZone")} className="danger-card">
          <p>{t("dangerDescription")}</p>
          <div className="button-row">
            <button
              type="button"
              className="button button--danger"
              onClick={() => setDanger({ slug: "trip", labelKey: "trip" })}
            >
              {t("trip")}…
            </button>
            <button
              type="button"
              className="button button--secondary"
              onClick={() =>
                setDanger({
                  slug: "clear_protection",
                  labelKey: "clearProtection",
                })
              }
            >
              {t("clearProtection")}…
            </button>
          </div>
        </Card>
      </form>
      {danger && (
        <ConfirmationDialog
          title={t(
            danger.labelKey === "trip"
              ? "confirmTrip"
              : "confirmClearProtection",
          )}
          detail={t("dangerDetail")}
          onCancel={() => setDanger(null)}
          onConfirm={() => void confirmDanger()}
        />
      )}
    </>
  );
}

function TestLab({
  catalog,
  notify,
}: ReturnType<typeof useSimulator> & { notify: (toast: Toast) => void }) {
  const { t } = useLanguage();
  const [query, setQuery] = useState(""),
    [protocol, setProtocol] = useState("modbus"),
    [mode, setMode] = useState("drop"),
    [delay, setDelay] = useState("200"),
    [confirmReset, setConfirmReset] = useState(false);
  const alarms = useMemo(
      () =>
        catalog
          .filter(
            (item) =>
              item.class === "alarm" &&
              item.name_raw.toLowerCase().includes(query.toLowerCase()),
          )
          .slice(0, 8),
      [catalog, query],
    ),
    selected = alarms[0];
  const run = async (action: () => Promise<unknown>, success: string) => {
    try {
      await action();
      notify({ tone: "success", text: success });
    } catch (error) {
      notify({ tone: "error", text: (error as Error).message });
    }
  };
  return (
    <>
      <div className="page-heading">
        <div>
          <p className="eyebrow">{t("faultProtocol")}</p>
          <h1>{t("lab")}</h1>
          <p>{t("labSubtitle")}</p>
        </div>
      </div>
      <div className="lab-grid">
        <Card title={t("alarmInjection")}>
          <label>
            {t("searchAlarms")}
            <input value={query} onChange={(e) => setQuery(e.target.value)} />
          </label>
          <div className="alarm-results">
            {alarms.map((alarm) => (
              <div key={`${alarm.device}/${alarm.slug}`}>
                <div>
                  <b>{alarm.device}</b> {alarm.name_raw}
                </div>
                <div className="button-row">
                  <button
                    className="button button--secondary"
                    onClick={() =>
                      void run(
                        () => api.injectFault(alarm.device, alarm.slug, 1),
                        t("alarmInjectedConfirmed"),
                      )
                    }
                  >
                    {t("inject")}
                  </button>
                  <button
                    className="button button--secondary"
                    onClick={() =>
                      void run(
                        () => api.clearFault(alarm.device, alarm.slug),
                        t("alarmClearedConfirmed"),
                      )
                    }
                  >
                    {t("clear")}
                  </button>
                </div>
              </div>
            ))}
            {!selected && <p className="empty">{t("noMatchingAlarms")}</p>}
          </div>
        </Card>
        <Card title={t("linkFaultSimulation")}>
          <label>
            {t("protocol")}
            <select
              value={protocol}
              onChange={(e) => setProtocol(e.target.value)}
            >
              <option value="iec104">IEC-104</option>
              <option value="modbus">Modbus</option>
            </select>
          </label>
          <label>
            {t("mode")}
            <select value={mode} onChange={(e) => setMode(e.target.value)}>
              <option value="drop">{t("drop")}</option>
              <option value="hang">{t("hang")}</option>
              <option value="delay">{t("delay")}</option>
              <option value="heartbeat_pause">{t("heartbeatPause")}</option>
            </select>
          </label>
          {mode === "delay" && (
            <label>
              {t("delay")}
              <span>ms</span>
              <input
                type="number"
                value={delay}
                onChange={(e) => setDelay(e.target.value)}
              />
            </label>
          )}
          <div className="button-row">
            <button
              className="button button--primary"
              onClick={() =>
                void run(
                  () => api.link(protocol, mode, Number(delay)),
                  t("linkApplied"),
                )
              }
            >
              {t("apply")}
            </button>
            <button
              className="button button--secondary"
              onClick={() =>
                void run(() => api.clearLink(protocol), t("linkRestored"))
              }
            >
              {t("restoreLink")}
            </button>
          </div>
        </Card>
        <Card title={t("resetSimulator")}>
          <p>{t("resetDescription")}</p>
          <button
            className="button button--danger"
            onClick={() => setConfirmReset(true)}
          >
            {t("reset")}
          </button>
        </Card>
      </div>
      {confirmReset && (
        <ConfirmationDialog
          title={t("confirmReset")}
          detail={t("resetDescription")}
          onCancel={() => setConfirmReset(false)}
          onConfirm={() => {
            setConfirmReset(false);
            void run(api.reset, t("resetConfirmed"));
          }}
        />
      )}
    </>
  );
}
