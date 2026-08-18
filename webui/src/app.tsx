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
import { api } from "./api";
import { useSimulator } from "./use-simulator";

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
  const dialogRef = useRef<HTMLElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    cancelRef.current?.focus();
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = previousOverflow;
    };
  }, []);
  const trapFocus = (event: React.KeyboardEvent<HTMLElement>) => {
    if (event.key === "Escape") {
      event.preventDefault();
      onCancel();
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
        className="dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="dialog-title"
        onKeyDown={trapFocus}
      >
        <p className="eyebrow">{t("confirmationRequired")}</p>
        <h2 id="dialog-title">{title}</h2>
        <p>{detail}</p>
        <div className="button-row">
          <button ref={cancelRef} className="button button--secondary" onClick={onCancel}>
            {t("cancel")}
          </button>
          <button className="button button--danger" onClick={onConfirm}>
            {t("confirmAction")}
          </button>
        </div>
      </section>
    </div>
  );
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
  samples: number[];
  unit: string;
  tone: string;
}) {
  const { locale, t } = useLanguage();
  const [chartNode, setChartNode] = useState<HTMLDivElement | null>(null);
  useEffect(() => {
    const node = chartNode;
    if (!node) return;
    const chart = echarts.init(node, undefined, { renderer: "svg" });
    chart.setOption({
      animation: false,
      grid: { left: 4, right: 4, top: 10, bottom: 2 },
      xAxis: { type: "category", show: false, data: samples.map(String) },
      yAxis: { type: "value", show: false },
      series: [
        {
          type: "line",
          data: samples.length ? samples : [value],
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
  }, [chartNode, samples, tone, value]);
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
  useEffect(() => {
    if (!simulator.lastEvent) return;
    const p = simulator.lastEvent.payload;
    const messages: Record<string, string> = {
      reset: t("eventReset"),
      diagnostic: t("eventDiagnostic"),
      scenario_step: t("eventScenarioStep", { index: String(p.index ?? "") }),
      fault: t("eventFault", {
        device: String(p.device ?? ""),
        slug: String(p.slug ?? ""),
      }),
    };
    notify({
      tone: simulator.lastEvent.type === "fault" ? "error" : "success",
      text: messages[simulator.lastEvent.type],
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
        <MiniChart
          title={t("stateOfCharge")}
          value={soc}
          samples={history["BMS/soc"] ?? []}
          unit="%"
          tone="#2367D1"
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
        <div className="config-list">
          {Object.entries(status?.configuration ?? {}).map(([key, setting]) => (
            <div key={key}>
              <span>{key}</span>
              <span>{String(setting.value)}</span>
              {setting.unconfirmed && (
                <Chip tone="warning">{t("unconfirmed")}</Chip>
              )}
            </div>
          ))}
        </div>
      </Card>
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
