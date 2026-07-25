
import { useState } from "react";
import { AlertTriangle, Check, Clipboard, Clock3, FileCheck, RefreshCw, ShieldCheck, TestTube2 } from "lucide-react";
import { useSettingsStore } from "@/lib/settings-store";
import { type EvidencePolicyMode, useEvidenceStatus } from "@/hooks/useEvidenceStatus";
import { useHarnessStatus } from "@/hooks/useHarnessStatus";
import { SectionActionStrip, SectionHeader, SettingRow } from "./SettingRow";
import { cn } from "@/lib/utils";

const POLICY_OPTIONS: Array<{
  value: EvidencePolicyMode;
  label: string;
  detail: string;
}> = [
  {
    value: "measure",
    label: "Measure",
    detail: "Record would-block decisions without interrupting runs.",
  },
  {
    value: "advisory",
    label: "Advisory",
    detail: "Warn when deep changes finish without proof.",
  },
  {
    value: "block",
    label: "Block",
    detail: "Stop deep changed runs until verification evidence exists.",
  },
  {
    value: "off",
    label: "Off",
    detail: "Disable evidence gates while keeping receipts readable.",
  },
];

export function VerificationSettings() {
  const { settings } = useSettingsStore();
  const harness = useHarnessStatus(settings.apiUrl);
  const evidence = useEvidenceStatus(settings.apiUrl);
  const [copiedEvidence, setCopiedEvidence] = useState(false);
  const workstreams = harness.data?.workstreams.workstreams ?? [];
  const recent = evidence.recent?.items ?? [];
  const activeWorkstreams = workstreams.filter((item) => item.status !== "done").length;
  const planStatus =
    typeof harness.data?.plan.status === "string"
      ? harness.data.plan.status
      : harness.data?.plan
        ? "active"
        : "unknown";
  const policy = evidence.policy?.policy ?? "measure";
  const proofCount = recent.filter((item) => item.status === "passed").length;
  const unresolvedCount = recent.filter((item) => item.status !== "passed").length;
  const evidenceScope = evidence.recent?.scope ?? "current";
  const runningLoops = harness.data?.loops.count ?? 0;
  const evidenceOnline = evidence.status === "success" && Boolean(evidence.policy || evidence.recent);
  const evidenceTone =
    policy === "block" ? "danger" : unresolvedCount > 0 ? "warn" : evidenceOnline ? "good" : "warn";
  const evidenceTitle =
    policy === "block"
      ? "Evidence gate can block completion"
      : unresolvedCount > 0
        ? "Unresolved receipts need review"
        : evidenceOnline
          ? "Evidence gate is measuring proof"
          : "Connect the runtime evidence API";
  const evidenceDescription = evidenceOnline
    ? "Receipts separate completion claims from proof. Keep Measure while calibrating, then move to Advisory or Block when the team trusts the signal."
    : evidence.error || "The proxy exposes policy and recent receipt endpoints for proof-backed automation.";
  const refreshAll = () => {
    void harness.refresh();
    void evidence.refresh();
  };

  async function copyEvidenceSummary() {
    const summary = [
      `Policy: ${policy}`,
      `Stop budget: ${evidence.policy?.maxStopBlocks ?? 2}`,
      `Receipts: ${recent.length}`,
      `Passed: ${proofCount}`,
      `Unresolved: ${unresolvedCount}`,
      `Scope: ${evidenceScope}`,
    ].join("\n");

    try {
      await navigator.clipboard.writeText(summary);
      setCopiedEvidence(true);
      window.setTimeout(() => setCopiedEvidence(false), 1600);
    } catch {
      setCopiedEvidence(false);
    }
  }

  return (
    <div>
      <SectionHeader
        eyebrow="Automation"
        title="Evidence gate"
        description="Completion is separated from proof. Deep changed runs are classified, recorded, and optionally blocked until verification evidence exists."
      >
        <button
          onClick={refreshAll}
          className="flex h-11 w-11 items-center justify-center rounded border border-surface-800 text-surface-400 transition-colors hover:border-surface-700 hover:text-surface-200 active:translate-y-px"
          title="Refresh verification state"
          type="button"
        >
          <RefreshCw
            className={cn(
              "h-3.5 w-3.5",
              (harness.status === "loading" || evidence.status === "loading") && "animate-spin"
            )}
          />
        </button>
      </SectionHeader>

      <SectionActionStrip
        icon={ShieldCheck}
        title={evidenceTitle}
        description={evidenceDescription}
        stateLabel={evidenceOnline ? policy : evidence.status}
        stateTone={evidenceTone}
        metrics={[
          {
            label: "Policy",
            value: policy,
            tone: policy === "block" ? "danger" : policy === "off" ? "warn" : "good",
          },
          {
            label: "Receipts",
            value: `${recent.length}`,
            tone: recent.length > 0 ? "good" : "neutral",
          },
          {
            label: "Unresolved",
            value: `${unresolvedCount}`,
            tone: unresolvedCount > 0 ? "warn" : "good",
          },
        ]}
        actions={[
          {
            label: "Refresh evidence state",
            icon: RefreshCw,
            onClick: refreshAll,
            disabled: harness.status === "loading" || evidence.status === "loading",
            tone: evidenceOnline ? "good" : "warn",
          },
          {
            label: copiedEvidence ? "Copied evidence" : "Copy evidence summary",
            icon: copiedEvidence ? Check : Clipboard,
            onClick: copyEvidenceSummary,
            tone: copiedEvidence ? "good" : "neutral",
          },
          {
            label: "Set Measure policy",
            icon: TestTube2,
            onClick: () => void evidence.updatePolicy("measure").catch(() => evidence.refresh()),
            disabled: policy === "measure",
            tone: policy === "measure" ? "good" : "warn",
          },
        ]}
      />

      <div className="mb-4 grid gap-3 md:grid-cols-3">
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Running Loops
          </p>
          <p className="mt-2 text-sm font-semibold text-surface-100">{runningLoops}</p>
          <p className="mt-1 text-xs text-surface-500">Evidence sources currently active</p>
        </div>
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Proof Receipts
          </p>
          <p className="mt-2 text-sm font-semibold text-surface-100">{proofCount}</p>
          <p className="mt-1 text-xs text-surface-500">
            {unresolvedCount} unresolved in {evidenceScope} scope
          </p>
        </div>
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Active Workstreams
          </p>
          <p className="mt-2 text-sm font-semibold text-surface-100">{activeWorkstreams}</p>
          <p className="mt-1 text-xs text-surface-500">Plan gate: {planStatus}</p>
        </div>
      </div>

      <SettingRow
        label="Policy"
        description="Measure is the safe default: it records evidence quality before enforcing blocks."
        stack
        scope="Automation"
        risk="high"
      >
        <div className="grid gap-2 md:grid-cols-4">
          {POLICY_OPTIONS.map((option) => {
            const selected = policy === option.value;
            return (
              <button
                key={option.value}
                onClick={() => void evidence.updatePolicy(option.value)}
                className={cn(
                  "rounded-lg border p-3 text-left transition-colors active:translate-y-px",
                  selected
                    ? "border-amber-300 bg-amber-300/10 text-surface-100"
                    : "border-surface-800 bg-surface-950/30 text-surface-400 hover:border-surface-700 hover:text-surface-200"
                )}
                type="button"
              >
                <span className="text-sm font-semibold">{option.label}</span>
                <span className="mt-1 block text-xs leading-relaxed text-surface-500">{option.detail}</span>
              </button>
            );
          })}
        </div>
        {evidence.error ? (
          <p className="mt-2 text-xs text-amber-200">{evidence.error}</p>
        ) : (
          <p className="mt-2 text-xs text-surface-500">
            Stop budget: {evidence.policy?.maxStopBlocks ?? 2} per run before warning-only fallback.
          </p>
        )}
      </SettingRow>

      <SettingRow
        label="Recent evidence"
        description="Receipts are the durable proof layer for agent, team, and Chronos runs."
        stack
        scope="Automation"
        risk="high"
      >
        <div className="space-y-2">
          {recent.length === 0 ? (
            <div className="rounded-lg border border-dashed border-surface-800 p-4 text-xs text-surface-500">
              No receipts recorded for this workspace yet.
            </div>
          ) : (
            recent.map((item) => (
              <div key={item.receiptPath} className="rounded-lg border border-surface-800 bg-surface-950/40 p-3">
                <div className="flex flex-wrap items-center gap-2">
                  {item.status === "passed" ? (
                    <ShieldCheck className="h-4 w-4 text-emerald-300" />
                  ) : item.status === "failed" ? (
                    <AlertTriangle className="h-4 w-4 text-amber-300" />
                  ) : (
                    <TestTube2 className="h-4 w-4 text-surface-500" />
                  )}
                  <span className="text-sm font-semibold text-surface-100">
                    {item.kind}
                    {item.workspace ? <span className="text-surface-500"> / {item.workspace}</span> : null}
                  </span>
                  <span className="rounded border border-surface-800 px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.12em] text-surface-400">
                    {item.status}
                  </span>
                  {item.gate ? (
                    <span className="rounded border border-surface-800 px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.12em] text-surface-400">
                      gate {item.gate}
                    </span>
                  ) : null}
                  {item.mode ? (
                    <span className="rounded border border-surface-800 px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.12em] text-surface-400">
                      {item.mode}
                    </span>
                  ) : null}
                  {item.createdAt ? (
                    <span className="ml-auto flex items-center gap-1 text-[11px] text-surface-600">
                      <Clock3 className="h-3 w-3" />
                      {formatEvidenceTime(item.createdAt)}
                    </span>
                  ) : null}
                </div>
                <div className="mt-2 flex items-start gap-2 text-xs text-surface-500">
                  <FileCheck className="mt-0.5 h-3.5 w-3.5 flex-shrink-0" />
                  <p className="min-w-0 flex-1">
                    {item.summary || item.command || "No verification evidence recorded"}
                  </p>
                </div>
                <div className="mt-2 flex flex-wrap gap-1.5">
                  <EvidenceChip value={item.source} />
                  <EvidenceChip value={item.provider} />
                  <EvidenceChip value={item.model} />
                  {item.evidenceCount > 0 ? <EvidenceChip value={`${item.evidenceCount} evidence`} /> : null}
                  {item.editedFileCount ? <EvidenceChip value={`${item.editedFileCount} edits`} /> : null}
                  {item.taskCount ? <EvidenceChip value={`${item.completed ?? 0}/${item.taskCount} tasks`} /> : null}
                </div>
                {item.command && item.summary !== item.command ? (
                  <p className="mt-2 truncate font-mono text-[11px] text-surface-600">
                    {item.command}
                  </p>
                ) : null}
                {item.editedFiles?.length ? (
                  <p className="mt-2 truncate font-mono text-[11px] text-surface-600">
                    {item.editedFiles.length} files: {item.editedFiles.join(", ")}
                  </p>
                ) : null}
              </div>
            ))
          )}
        </div>
      </SettingRow>
    </div>
  );
}

function EvidenceChip({ value }: { value?: string }) {
  if (!value) return null;
  return (
    <span className="rounded border border-surface-800 px-1.5 py-0.5 text-[11px] text-surface-500">
      {value}
    </span>
  );
}

function formatEvidenceTime(value: string) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return new Intl.DateTimeFormat(undefined, {
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  }).format(date);
}
