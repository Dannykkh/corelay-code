
import { useState } from "react";
import { Check, Clipboard, Clock3, FileText, RefreshCw, Repeat, RotateCcw, ShieldCheck } from "lucide-react";
import { useSettingsStore } from "@/lib/settings-store";
import { useHarnessStatus } from "@/hooks/useHarnessStatus";
import { SectionActionStrip, SectionHeader, SettingRow } from "./SettingRow";
import { cn } from "@/lib/utils";

const LOOP_LAYERS = [
  {
    label: "Persistence",
    value: "goal",
    detail: "Keeps the session alive until a measurable end state is reached.",
  },
  {
    label: "Discipline",
    value: "chronos",
    detail: "Runs FIND, FIX, VERIFY, and LOG with one issue per cycle.",
  },
  {
    label: "Recurring work",
    value: "loop",
    detail: "Schedules repeated prompts when the work is intentionally periodic.",
  },
];

export function LoopsSettings() {
  const { settings } = useSettingsStore();
  const harness = useHarnessStatus(settings.apiUrl);
  const [maxIterations, setMaxIterations] = useState(50);
  const [promise, setPromise] = useState("Targeted verification passes");
  const [copiedContract, setCopiedContract] = useState(false);
  const planStatus =
    typeof harness.data?.plan.status === "string"
      ? harness.data.plan.status
      : harness.data?.plan
        ? "active"
        : "unknown";
  const activeLoops = harness.data?.loops.count ?? 0;
  const maxConcurrent = harness.data?.loops.maxConcurrent ?? 3;
  const loopsOnline = harness.status === "success" && Boolean(harness.data);
  const loopTone = activeLoops > 0 ? "warn" : loopsOnline ? "good" : harness.status === "loading" ? "neutral" : "warn";
  const loopTitle = activeLoops > 0
    ? "Automation is currently running"
    : loopsOnline
      ? "Loop control plane is idle"
      : harness.status === "loading"
        ? "Checking loop control plane"
        : "Connect the runtime to inspect loops";
  const loopDescription = activeLoops > 0
    ? "Watch proof gates and handoff receipts before starting more recurring work."
    : loopsOnline
      ? "Use the run contract below to keep long-running work bounded by proof, not self-judgment."
      : harness.error || "The runtime exposes active loops, concurrency caps, and plan gate state.";

  function resetRunContract() {
    setMaxIterations(50);
    setPromise("Targeted verification passes");
  }

  async function copyRunContract() {
    const contract = [
      `Max cycles: ${maxIterations}`,
      `Completion promise: ${promise.trim() || "defined promise"}`,
      `Active loops: ${activeLoops}/${maxConcurrent}`,
      `Plan status: ${planStatus}`,
    ].join("\n");

    try {
      await navigator.clipboard.writeText(contract);
      setCopiedContract(true);
      window.setTimeout(() => setCopiedContract(false), 1600);
    } catch {
      setCopiedContract(false);
    }
  }

  return (
    <div>
      <SectionHeader
        eyebrow="Automation"
        title="Loops"
        description="Separate persistence, recurring prompts, and proof gates so long-running automation does not rely on self-judgment."
      >
        <button
          onClick={harness.refresh}
          className="flex h-11 w-11 items-center justify-center rounded border border-surface-800 text-surface-400 transition-colors hover:border-surface-700 hover:text-surface-200 active:translate-y-px"
          title="Refresh loop registry"
          type="button"
        >
          <RefreshCw className={cn("h-3.5 w-3.5", harness.status === "loading" && "animate-spin")} />
        </button>
      </SectionHeader>

      <SectionActionStrip
        icon={Repeat}
        title={loopTitle}
        description={loopDescription}
        stateLabel={activeLoops > 0 ? `${activeLoops} running` : loopsOnline ? "idle" : harness.status}
        stateTone={loopTone}
        metrics={[
          {
            label: "Active loops",
            value: `${activeLoops}`,
            tone: activeLoops > 0 ? "warn" : "good",
          },
          {
            label: "Concurrency",
            value: `${maxConcurrent}`,
            tone: "neutral",
          },
          {
            label: "Plan",
            value: planStatus,
            tone: planStatus === "unknown" ? "neutral" : "good",
          },
        ]}
        actions={[
          {
            label: "Refresh loop registry",
            icon: RefreshCw,
            onClick: harness.refresh,
            disabled: harness.status === "loading",
            tone: loopsOnline ? "good" : "warn",
          },
          {
            label: copiedContract ? "Copied contract" : "Copy run contract",
            icon: copiedContract ? Check : Clipboard,
            onClick: copyRunContract,
            tone: copiedContract ? "good" : "neutral",
          },
          {
            label: "Reset run contract",
            icon: RotateCcw,
            onClick: resetRunContract,
            tone: "neutral",
          },
        ]}
      />

      <div className="mb-4 grid gap-3 md:grid-cols-3">
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Active Loops
          </p>
          <p className="mt-2 text-sm font-semibold text-surface-100">{harness.data?.loops.count ?? 0}</p>
          <p className="mt-1 text-xs text-surface-500">{harness.error || "Current agent loop registry"}</p>
        </div>
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Concurrency
          </p>
          <p className="mt-2 text-sm font-semibold text-surface-100">
            {harness.data?.loops.maxConcurrent ?? 3}
          </p>
          <p className="mt-1 text-xs text-surface-500">Server-side loop cap</p>
        </div>
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Plan Mode
          </p>
          <p className="mt-2 font-mono text-sm text-surface-100">{planStatus}</p>
          <p className="mt-1 text-xs text-surface-500">Approval gate status</p>
        </div>
      </div>

      <div className="grid gap-3 md:grid-cols-3">
        {LOOP_LAYERS.map((layer) => (
          <div
            key={layer.label}
            className="rounded-lg border border-surface-800 bg-surface-950/30 p-3"
          >
            <div className="mb-2 flex items-center justify-between gap-3">
              <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
                {layer.label}
              </p>
              <Repeat className="h-3.5 w-3.5 text-surface-500" />
            </div>
            <p className="font-mono text-sm text-surface-100">{layer.value}</p>
            <p className="mt-1 text-xs leading-relaxed text-surface-500">{layer.detail}</p>
          </div>
        ))}
      </div>

      <SettingRow
        label="Chronos run contract"
        description="Preview the evidence contract a long-running implementation loop should carry."
        stack
        scope="Automation"
        risk="medium"
      >
        <div className="grid gap-3 md:grid-cols-[160px_minmax(0,1fr)_220px]">
          <label>
            <span className="mb-1 block text-xs text-surface-400">Max cycles</span>
            <input
              type="number"
              min={1}
              max={100}
              value={maxIterations}
              onChange={(event) => setMaxIterations(Number(event.target.value))}
              className={cn(
                "min-h-11 w-full rounded-md border border-surface-700 bg-surface-800 px-3 py-2 font-mono text-sm",
                "text-surface-200 focus:outline-none focus:ring-1 focus:ring-brand-500"
              )}
            />
          </label>
          <label>
            <span className="mb-1 block text-xs text-surface-400">Completion promise</span>
            <input
              value={promise}
              onChange={(event) => setPromise(event.target.value)}
              className={cn(
                "min-h-11 w-full rounded-md border border-surface-700 bg-surface-800 px-3 py-2 text-sm",
                "text-surface-200 focus:outline-none focus:ring-1 focus:ring-brand-500"
              )}
            />
          </label>
          <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
            <div className="mb-1 flex items-center gap-2">
              <ShieldCheck className="h-3.5 w-3.5 text-green-400" />
              <p className="text-xs font-medium text-surface-200">Stop condition</p>
            </div>
            <p className="text-[11px] leading-relaxed text-surface-500">
              {maxIterations} cycles or proof of{" "}
              <span className="font-mono text-surface-300">
                {promise.trim() || "defined promise"}
              </span>
              .
            </p>
          </div>
        </div>
      </SettingRow>

      <div className="mt-4 grid gap-3 md:grid-cols-2">
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-4">
          <Clock3 className="mb-2 h-4 w-4 text-surface-500" />
          <p className="text-sm font-medium text-surface-100">Re-entry rule</p>
          <p className="mt-1 text-xs leading-relaxed text-surface-500">
            A resumed loop reads the Chronos log first, then verifies current state before finding the next issue.
          </p>
        </div>
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-4">
          <FileText className="mb-2 h-4 w-4 text-surface-500" />
          <p className="text-sm font-medium text-surface-100">Parked issue brief</p>
          <p className="mt-1 text-xs leading-relaxed text-surface-500">
            Blocked work ends with evidence, tradeoffs, recommendation, and exact owner choices.
          </p>
        </div>
      </div>
    </div>
  );
}
