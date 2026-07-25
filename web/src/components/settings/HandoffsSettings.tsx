
import { useState } from "react";
import { AlertTriangle, Check, Clipboard, Database, FileText, GitBranch, RefreshCw } from "lucide-react";
import { useSettingsStore } from "@/lib/settings-store";
import { useHarnessStatus } from "@/hooks/useHarnessStatus";
import { SectionActionStrip, SectionHeader, SettingRow } from "./SettingRow";
import { cn } from "@/lib/utils";

const HANDOFF_BLOCKS = [
  {
    label: "Current state",
    detail: "What changed, what is verified, and where the next session should resume.",
    icon: FileText,
  },
  {
    label: "Decision trail",
    detail: "Architecture choices and rationale that should not be rediscovered.",
    icon: GitBranch,
  },
  {
    label: "Memory index",
    detail: "Long-lived facts stored compactly so future sessions can search them.",
    icon: Database,
  },
];

export function HandoffsSettings() {
  const { settings } = useSettingsStore();
  const harness = useHarnessStatus(settings.apiUrl);
  const [copiedBrief, setCopiedBrief] = useState(false);
  const [copiedNextAction, setCopiedNextAction] = useState(false);
  const workstreams = harness.data?.workstreams.workstreams ?? [];
  const latestWorkstream = workstreams[0];
  const activeWorkstreams = workstreams.filter((item) => item.status !== "done").length;
  const handoffOnline = harness.status === "success" && Boolean(harness.data);
  const handoffTone = activeWorkstreams > 0 ? "warn" : handoffOnline ? "good" : "warn";
  const handoffTitle = activeWorkstreams > 0
    ? "Active workstreams need resumable handoff"
    : handoffOnline
      ? "Handoff surface is ready"
      : "Connect the runtime handoff state";
  const handoffDescription = handoffOnline
    ? "Capture current state, decision trail, memory index, and owner choices before ending long-running work."
    : harness.error || "The proxy exposes workstream state and workspace paths for resumable automation.";

  async function copyHandoffBrief() {
    const brief = [
      `Workspace: ${harness.data?.workstreams.workDir || "unknown"}`,
      `Workstreams: ${workstreams.length}`,
      `Active: ${activeWorkstreams}`,
      `Latest: ${latestWorkstream?.title || latestWorkstream?.id || "none"}`,
      `Next action: ${latestWorkstream?.nextAction || "none"}`,
    ].join("\n");

    try {
      await navigator.clipboard.writeText(brief);
      setCopiedBrief(true);
      window.setTimeout(() => setCopiedBrief(false), 1600);
    } catch {
      setCopiedBrief(false);
    }
  }

  async function copyLatestNextAction() {
    try {
      await navigator.clipboard.writeText(latestWorkstream?.nextAction || "No pending next action");
      setCopiedNextAction(true);
      window.setTimeout(() => setCopiedNextAction(false), 1600);
    } catch {
      setCopiedNextAction(false);
    }
  }

  return (
    <div>
      <SectionHeader
        eyebrow="Automation"
        title="Handoffs"
        description="Handoffs make long-running harness work resumable without depending on conversation memory."
      >
        <button
          onClick={harness.refresh}
          className="flex h-11 w-11 items-center justify-center rounded border border-surface-800 text-surface-400 transition-colors hover:border-surface-700 hover:text-surface-200 active:translate-y-px"
          title="Refresh handoff state"
          type="button"
        >
          <RefreshCw className={cn("h-3.5 w-3.5", harness.status === "loading" && "animate-spin")} />
        </button>
      </SectionHeader>

      <SectionActionStrip
        icon={FileText}
        title={handoffTitle}
        description={handoffDescription}
        stateLabel={handoffOnline ? `${activeWorkstreams} active` : harness.status}
        stateTone={handoffTone}
        metrics={[
          {
            label: "Workstreams",
            value: `${workstreams.length}`,
            tone: workstreams.length > 0 ? "good" : "neutral",
          },
          {
            label: "Active",
            value: `${activeWorkstreams}`,
            tone: activeWorkstreams > 0 ? "warn" : "good",
          },
          {
            label: "Workspace",
            value: harness.data?.workstreams.workDir ? "known" : "unknown",
            tone: harness.data?.workstreams.workDir ? "good" : "neutral",
          },
        ]}
        actions={[
          {
            label: "Refresh handoff state",
            icon: RefreshCw,
            onClick: harness.refresh,
            disabled: harness.status === "loading",
            tone: handoffOnline ? "good" : "warn",
          },
          {
            label: copiedBrief ? "Copied handoff" : "Copy handoff brief",
            icon: copiedBrief ? Check : Clipboard,
            onClick: copyHandoffBrief,
            tone: copiedBrief ? "good" : "neutral",
          },
          {
            label: copiedNextAction ? "Copied next action" : "Copy next action",
            icon: GitBranch,
            onClick: copyLatestNextAction,
            disabled: !latestWorkstream?.nextAction,
            tone: latestWorkstream?.nextAction ? "warn" : "neutral",
          },
        ]}
      />

      <div className="mb-4 grid gap-3 md:grid-cols-3">
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Workspace
          </p>
          <p className="mt-2 truncate font-mono text-xs text-surface-200">
            {harness.data?.workstreams.workDir || "unknown"}
          </p>
          <p className="mt-1 text-xs text-surface-500">{harness.error || "Proxy work directory"}</p>
        </div>
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Workstreams
          </p>
          <p className="mt-2 text-sm font-semibold text-surface-100">{workstreams.length}</p>
          <p className="mt-1 text-xs text-surface-500">Resumable automation tracks</p>
        </div>
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Latest
          </p>
          <p className="mt-2 truncate text-sm font-semibold text-surface-100">
            {latestWorkstream?.title || latestWorkstream?.id || "No workstream"}
          </p>
          <p className="mt-1 text-xs text-surface-500">
            {latestWorkstream?.nextAction || "Nothing pending in the proxy store"}
          </p>
        </div>
      </div>

      <div className="grid gap-3 md:grid-cols-3">
        {HANDOFF_BLOCKS.map(({ label, detail, icon: Icon }) => (
          <div
            key={label}
            className="rounded-lg border border-surface-800 bg-surface-950/30 p-3"
          >
            <Icon className="mb-2 h-4 w-4 text-surface-500" />
            <p className="text-sm font-semibold text-surface-100">{label}</p>
            <p className="mt-1 text-xs leading-relaxed text-surface-500">{detail}</p>
          </div>
        ))}
      </div>

      <SettingRow
        label="Owner decision brief"
        description="Parked automation issues should arrive with evidence, tradeoffs, recommendation, and exact choices."
        stack
        scope="Automation"
        risk="medium"
      >
        <div className="rounded-lg border border-amber-500/20 bg-amber-500/10 p-3">
          <div className="mb-2 flex items-center gap-2">
            <AlertTriangle className="h-4 w-4 text-amber-300" />
            <p className="text-sm font-medium text-amber-100">Parked issue shape</p>
          </div>
          <div className="grid gap-2 text-xs leading-relaxed text-amber-100/80 md:grid-cols-3">
            <span>What changed and who is affected</span>
            <span>Evidence already collected</span>
            <span>Recommended owner choice</span>
          </div>
        </div>
      </SettingRow>
    </div>
  );
}
