
import { useState } from "react";
import {
  Activity,
  AlertTriangle,
  ArrowRight,
  Check,
  CheckCircle2,
  Clipboard,
  Database,
  FilePlus2,
  GitBranch,
  KeyRound,
  Radio,
  RefreshCw,
  Server,
  Shield,
} from "lucide-react";
import { useSettingsStore } from "@/lib/settings-store";
import { MODELS } from "@/lib/constants";
import { formatRuntimeGroup, useRuntimeStatus } from "@/hooks/useRuntimeStatus";
import { SectionHeader } from "./SettingRow";
import type { SettingsSection } from "./SettingsNav";
import { cn } from "@/lib/utils";

function Metric({
  label,
  value,
  detail,
  tone = "neutral",
}: {
  label: string;
  value: string;
  detail: string;
  tone?: "neutral" | "good" | "warn";
}) {
  return (
    <div className="rounded-lg border border-surface-800 bg-surface-950/40 p-3">
      <div className="mb-2 flex items-center justify-between gap-3">
        <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-500">
          {label}
        </p>
        <span
          className={cn(
            "h-2 w-2 rounded-full",
            tone === "good" && "bg-green-400",
            tone === "warn" && "bg-amber-400",
            tone === "neutral" && "bg-surface-600"
          )}
        />
      </div>
      <p className="break-words text-xs font-semibold text-surface-100">{value}</p>
      <p className="mt-1 text-xs leading-snug text-surface-500">{detail}</p>
    </div>
  );
}

function StatusRow({
  icon: Icon,
  label,
  value,
}: {
  icon: React.ElementType;
  label: string;
  value: string;
}) {
  return (
    <div className="flex items-center justify-between gap-4 border-b border-surface-800/80 py-3 last:border-0">
      <div className="flex min-w-0 items-center gap-2.5">
        <Icon className="h-4 w-4 flex-shrink-0 text-surface-500" />
        <span className="text-sm text-surface-300">{label}</span>
      </div>
      <span className="truncate text-right font-mono text-xs text-surface-500">{value}</span>
    </div>
  );
}

function SetupStep({
  icon: Icon,
  label,
  value,
  detail,
  copyLabel,
  copied,
  onCopy,
}: {
  icon: React.ElementType;
  label: string;
  value: string;
  detail: string;
  copyLabel?: string;
  copied?: boolean;
  onCopy?: () => void;
}) {
  return (
    <div className="min-w-0 border-b border-surface-800/80 py-3 last:border-0 md:border-b-0 md:border-r md:px-4 md:first:pl-0 md:last:border-r-0 md:last:pr-0">
      <div className="mb-2 flex items-center justify-between gap-2">
        <div className="flex min-w-0 items-center gap-2">
          <Icon className="h-4 w-4 flex-shrink-0 text-amber-200" />
          <p className="min-w-0 break-words text-xs font-medium leading-snug text-surface-100">{label}</p>
        </div>
        {onCopy && (
          <button
            onClick={onCopy}
            className="flex h-11 min-w-11 flex-shrink-0 items-center justify-center rounded border border-surface-800 text-surface-500 transition-colors hover:border-surface-700 hover:text-surface-200 active:translate-y-px"
            title={copyLabel}
            type="button"
          >
            {copied ? <Check className="h-3.5 w-3.5" /> : <Clipboard className="h-3.5 w-3.5" />}
          </button>
        )}
      </div>
      <p className="break-words font-mono text-xs text-surface-300">{value}</p>
      <p className="mt-1 break-words text-xs leading-relaxed text-surface-500">{detail}</p>
    </div>
  );
}

function ConceptRow({
  label,
  value,
  owner,
}: {
  label: string;
  value: string;
  owner: string;
}) {
  return (
    <div className="grid gap-1 border-b border-surface-800/80 py-2.5 last:border-0 md:grid-cols-[9rem_minmax(0,1fr)_8rem] md:items-center">
      <p className="text-sm font-medium text-surface-200">{label}</p>
      <p className="text-xs leading-relaxed text-surface-500">{value}</p>
      <p className="font-mono text-[11px] uppercase tracking-[0.08em] text-surface-600">{owner}</p>
    </div>
  );
}

type DoctorState = "ready" | "needs" | "offline" | "checking";
type QuickActionStatus = "idle" | "running" | "success" | "error";

interface DoctorQuickAction {
  label: string;
  icon: React.ElementType;
  onClick: () => void;
  title?: string;
  disabled?: boolean;
  tone?: "neutral" | "good" | "warn";
}

interface QuotaSourceSampleResponse {
  path?: string;
  source?: {
    name?: string;
    type?: "file" | "http" | string;
    path?: string;
    url?: string;
    intervalSeconds?: number;
    timeoutSeconds?: number;
  };
}

interface QuotaSourceTestResponse {
  accountCount?: number;
  accounts?: Array<{ provider?: string; group?: string }>;
}

const DOCTOR_LABELS: Record<DoctorState, string> = {
  ready: "Ready",
  needs: "Needs setup",
  offline: "Offline",
  checking: "Checking",
};

const DOCTOR_STYLES: Record<DoctorState, { badge: string; icon: string; dot: string }> = {
  ready: {
    badge: "border-green-500/20 bg-green-500/10 text-green-300",
    icon: "border-green-500/20 bg-green-500/10 text-green-300",
    dot: "bg-green-400",
  },
  needs: {
    badge: "border-amber-500/20 bg-amber-500/10 text-amber-300",
    icon: "border-amber-500/20 bg-amber-500/10 text-amber-300",
    dot: "bg-amber-400",
  },
  offline: {
    badge: "border-red-500/20 bg-red-500/10 text-red-300",
    icon: "border-red-500/20 bg-red-500/10 text-red-300",
    dot: "bg-red-400",
  },
  checking: {
    badge: "border-surface-700 bg-surface-900 text-surface-400",
    icon: "border-surface-700 bg-surface-900 text-surface-400",
    dot: "bg-surface-500",
  },
};

function SetupDoctorItem({
  icon: Icon,
  title,
  state,
  detail,
  section,
  actionLabel,
  quickActions,
  result,
  onNavigate,
}: {
  icon: React.ElementType;
  title: string;
  state: DoctorState;
  detail: string;
  section: SettingsSection;
  actionLabel: string;
  quickActions?: DoctorQuickAction[];
  result?: { tone: "good" | "warn"; message: string } | null;
  onNavigate?: (section: SettingsSection) => void;
}) {
  const styles = DOCTOR_STYLES[state];

  return (
    <div className="flex min-w-0 flex-col gap-3 border-b border-surface-800/80 py-3 last:border-0 md:grid md:grid-cols-[2rem_minmax(0,1fr)_auto] md:items-start md:gap-3">
      <div
        className={cn(
          "flex h-8 w-8 flex-shrink-0 items-center justify-center rounded-md border",
          styles.icon
        )}
      >
        <Icon className="h-4 w-4" />
      </div>

      <div className="min-w-0">
        <div className="flex flex-wrap items-center gap-2">
          <p className="text-sm font-medium text-surface-100">{title}</p>
          <span
            className={cn(
              "inline-flex items-center gap-1.5 rounded border px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-[0.08em]",
              styles.badge
            )}
          >
            <span className={cn("h-1.5 w-1.5 rounded-full", styles.dot)} />
            {DOCTOR_LABELS[state]}
          </span>
        </div>
        <p className="mt-1 text-xs leading-relaxed text-surface-500">{detail}</p>
        {result?.message && (
          <p
            className={cn(
              "mt-2 break-all text-xs leading-relaxed",
              result.tone === "good" ? "text-green-300" : "text-amber-300"
            )}
          >
            {result.message}
          </p>
        )}
      </div>

      <div className="flex min-w-0 flex-col gap-2 sm:flex-row sm:flex-wrap md:justify-end">
        {quickActions?.map((action) => {
          const ActionIcon = action.icon;
          return (
            <button
              key={action.label}
              onClick={action.onClick}
              disabled={action.disabled}
              className={cn(
                "inline-flex min-h-11 w-full items-center justify-center gap-1.5 rounded border px-2.5 py-2 text-xs transition-colors active:translate-y-px disabled:cursor-not-allowed disabled:opacity-50 sm:w-auto",
                action.tone === "good"
                  ? "border-green-500/20 bg-green-500/10 text-green-300 hover:border-green-500/40"
                  : action.tone === "warn"
                    ? "border-amber-500/20 bg-amber-500/10 text-amber-300 hover:border-amber-500/40"
                    : "border-surface-800 text-surface-400 hover:border-surface-700 hover:text-surface-100"
              )}
              title={action.title}
              type="button"
            >
              <ActionIcon className="h-3.5 w-3.5" />
              {action.label}
            </button>
          );
        })}
        <button
          onClick={() => onNavigate?.(section)}
          className="inline-flex min-h-11 w-full items-center justify-center gap-1.5 rounded border border-surface-800 px-2.5 py-2 text-xs text-surface-400 transition-colors hover:border-surface-700 hover:text-surface-100 active:translate-y-px sm:w-auto"
          type="button"
        >
          {actionLabel}
          <ArrowRight className="h-3.5 w-3.5" />
        </button>
      </div>
    </div>
  );
}

function normalizeApiUrl(apiUrl: string) {
  return apiUrl.trim().replace(/\/+$/, "");
}

function formatCheckTime(value?: string | null) {
  if (!value) return "not checked";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "not checked";
  return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

export function OverviewSettings({
  onNavigate,
}: {
  onNavigate?: (section: SettingsSection) => void;
}) {
  const { settings, conversations } = useSettingsStore();
  const [copiedKey, setCopiedKey] = useState<string | null>(null);
  const [runtimeCheckedAt, setRuntimeCheckedAt] = useState<string | null>(null);
  const [quotaQuickAction, setQuotaQuickAction] = useState<{
    status: QuickActionStatus;
    message: string | null;
  }>({ status: "idle", message: null });
  const runtime = useRuntimeStatus(settings.apiUrl);
  const activeRuntime = runtime.data?.active;
  const runtimeLeases = runtime.data?.leases ?? [];
  const quotaCollectors = runtime.data?.quotaCollectors ?? [];
  const enabledCollectors = quotaCollectors.filter((collector) => collector.enabled);
  const selectedModel = MODELS.find((model) => model.id === settings.model);
  const enabledTools = Object.values(settings.permissions.autoApprove).filter(Boolean).length;
  const runtimeBaseUrl = settings.apiUrl.trim() || "http://localhost:3001";
  const anthropicBaseEnv = `ANTHROPIC_BASE_URL=${runtimeBaseUrl}`;
  const powerShellLaunch = `$env:ANTHROPIC_BASE_URL="${runtimeBaseUrl}"; claude`;
  const isRuntimeOnline = runtime.status === "success" && Boolean(runtime.data);
  const accountCount = runtime.data?.accounts.length ?? 0;
  const providerCount = runtime.data?.providers.length ?? 0;
  const hasApiUrl = Boolean(settings.apiUrl.trim());
  const totalMessages = conversations.reduce((sum, conversation) => {
    return sum + conversation.messages.length;
  }, 0);
  const runtimeTone = runtime.status === "success" ? "good" : runtime.status === "error" ? "warn" : "neutral";
  const lastRuntimeCheck = runtimeCheckedAt || runtime.data?.generatedAt;
  const quotaActionBusy = quotaQuickAction.status === "running";
  const doctorItems: Array<{
    icon: React.ElementType;
    title: string;
    state: DoctorState;
    detail: string;
    section: SettingsSection;
    actionLabel: string;
    quickActions?: DoctorQuickAction[];
    result?: { tone: "good" | "warn"; message: string } | null;
  }> = [
    {
      icon: Server,
      title: "Proxy reachable",
      state: runtime.status === "loading" || runtime.status === "idle" ? "checking" : isRuntimeOnline ? "ready" : "offline",
      detail: isRuntimeOnline
        ? `Local proxy responded from ${runtimeBaseUrl}.`
        : runtime.error || "Start the local proxy and confirm the API base URL.",
      section: "providers",
      actionLabel: "Providers",
    },
    {
      icon: Clipboard,
      title: "Claude Code launch ready",
      state: hasApiUrl ? "ready" : "needs",
      detail: hasApiUrl
        ? "The launch command is assembled from the active API base URL."
        : "Set the API base URL so the launch command is explicit.",
      section: "providers",
      actionLabel: "Providers",
      quickActions: [
        {
          label: copiedKey === "base-url" ? "Copied env" : "Copy env",
          icon: copiedKey === "base-url" ? Check : Clipboard,
          onClick: () => copyRuntimeText("base-url", anthropicBaseEnv),
          title: "Copy Anthropic base URL",
          tone: copiedKey === "base-url" ? "good" : "neutral",
        },
        {
          label: copiedKey === "launch-command" ? "Copied launch" : "Copy launch",
          icon: copiedKey === "launch-command" ? Check : Clipboard,
          onClick: () => copyRuntimeText("launch-command", powerShellLaunch),
          title: "Copy PowerShell launch command",
          tone: copiedKey === "launch-command" ? "good" : "neutral",
        },
      ],
    },
    {
      icon: KeyRound,
      title: "Provider account ready",
      state: !isRuntimeOnline ? "offline" : accountCount > 0 && Boolean(activeRuntime?.provider) ? "ready" : "needs",
      detail: accountCount > 0
        ? `${accountCount} account${accountCount === 1 ? "" : "s"} available for ${activeRuntime?.provider ?? "runtime"} traffic.`
        : "Add at least one account identity before relying on quota-aware switching.",
      section: "accounts",
      actionLabel: "Accounts",
    },
    {
      icon: GitBranch,
      title: "Model routing ready",
      state: !isRuntimeOnline ? "offline" : providerCount > 0 ? "ready" : "needs",
      detail: runtime.data?.routerEnabled
        ? "Router policy is enabled for model-name based provider selection."
        : providerCount > 0
          ? "Provider catalog is loaded; direct model selection remains available."
          : "Add providers before expecting claude-*, gpt-*, or local model families.",
      section: "routing",
      actionLabel: "Routing",
    },
    {
      icon: Database,
      title: "Quota balancing ready",
      state: !isRuntimeOnline ? "offline" : enabledCollectors.length > 0 || runtimeLeases.length > 0 ? "ready" : "needs",
      detail: enabledCollectors.length > 0 || runtimeLeases.length > 0
        ? `${enabledCollectors.length} collector${enabledCollectors.length === 1 ? "" : "s"} and ${runtimeLeases.length} lease${runtimeLeases.length === 1 ? "" : "s"} are feeding scheduler decisions.`
        : "Create or test a quota source so the scheduler can see reset windows before switching accounts.",
      section: "scheduler",
      actionLabel: "Scheduler",
      quickActions: [
        {
          label: quotaActionBusy ? "Testing sample" : "Sample + test",
          icon: quotaActionBusy ? RefreshCw : FilePlus2,
          onClick: quickTestSampleQuotaSource,
          title: "Create a sample quota file and test it without saving a source",
          disabled: quotaActionBusy || !isRuntimeOnline,
          tone: quotaQuickAction.status === "success" ? "good" : quotaQuickAction.status === "error" ? "warn" : "neutral",
        },
      ],
      result: quotaQuickAction.message
        ? {
            tone: quotaQuickAction.status === "error" ? "warn" : "good",
            message: quotaQuickAction.message,
          }
        : null,
    },
  ];
  const readyDoctorItems = doctorItems.filter((item) => item.state === "ready").length;
  const blockingDoctorItems = doctorItems.filter((item) => item.state === "needs" || item.state === "offline").length;
  const checkingDoctorItems = doctorItems.filter((item) => item.state === "checking").length;
  const nextDoctorItem =
    doctorItems.find((item) => item.state === "offline" || item.state === "needs") ??
    doctorItems.find((item) => item.state === "checking") ??
    doctorItems[0]!;
  const setupState: DoctorState =
    blockingDoctorItems > 0 ? "needs" : checkingDoctorItems > 0 ? "checking" : "ready";
  const setupStyles = DOCTOR_STYLES[setupState];
  const setupProgressPercent = Math.round((readyDoctorItems / doctorItems.length) * 100);
  const setupHeadline =
    setupState === "ready"
      ? "Ready to launch through the local proxy"
      : setupState === "checking"
        ? "Checking the runtime path"
        : "Next setup action";
  const setupDetail =
    setupState === "ready"
      ? "The proxy, launch command, account surface, routing, and quota loop all report ready."
      : nextDoctorItem.detail;

  async function copyRuntimeText(key: string, value: string) {
    try {
      await navigator.clipboard.writeText(value);
      setCopiedKey(key);
      window.setTimeout(() => setCopiedKey((current) => (current === key ? null : current)), 1600);
    } catch {
      setCopiedKey(null);
    }
  }

  async function runtimeQuotaRequest<T>(path: string, init: RequestInit): Promise<T> {
    const baseUrl = normalizeApiUrl(runtimeBaseUrl);
    if (!baseUrl) {
      throw new Error("Runtime API URL is empty");
    }
    const headers = new Headers(init.headers);
    headers.set("Content-Type", "application/json");
    const response = await fetch(`${baseUrl}${path}`, {
      ...init,
      headers,
    });
    if (!response.ok) {
      const text = await response.text();
      throw new Error(text || `Runtime API returned ${response.status}`);
    }
    return response.json() as Promise<T>;
  }

  async function quickTestSampleQuotaSource() {
    setQuotaQuickAction({ status: "running", message: null });
    try {
      const sample = await runtimeQuotaRequest<QuotaSourceSampleResponse>("/api/runtime/quota-sources/sample", {
        method: "POST",
      });
      const source = sample.source ?? { type: "file", path: sample.path };
      const path = source.path || sample.path;
      if (!path) {
        throw new Error("Runtime API did not return a sample path");
      }
      const test = await runtimeQuotaRequest<QuotaSourceTestResponse>("/api/runtime/quota-sources/test", {
        method: "POST",
        body: JSON.stringify({
          name: source.name || "local quota sample",
          type: source.type || "file",
          path,
          url: source.url,
          intervalSeconds: source.intervalSeconds,
          timeoutSeconds: source.timeoutSeconds,
        }),
      });
      await runtime.refresh();
      const accountCount = test.accountCount ?? test.accounts?.length ?? 0;
      const firstProvider = test.accounts?.[0]?.provider;
      setQuotaQuickAction({
        status: "success",
        message: `Sample tested: ${accountCount} account${accountCount === 1 ? "" : "s"} parsed${firstProvider ? `, first ${firstProvider}` : ""}.`,
      });
    } catch (error) {
      setQuotaQuickAction({
        status: "error",
        message: error instanceof Error ? error.message : "Could not create and test sample quota source.",
      });
    }
  }

  async function recheckRuntime() {
    await runtime.refresh();
    setRuntimeCheckedAt(new Date().toISOString());
  }

  return (
    <div>
      <SectionHeader
        eyebrow="Settings Center"
        title="Overview"
        description="Start here when setup feels unclear. The doctor shows the next action before the detailed settings."
      />

      <div className="overflow-hidden rounded-lg border border-surface-800 bg-surface-950/30">
        <div className="grid lg:grid-cols-[minmax(0,1fr)_20rem]">
          <div className="min-w-0 p-4">
            <div className="mb-4 flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
              <div className="min-w-0">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="inline-flex items-center gap-1.5 rounded border border-amber-500/20 bg-amber-500/10 px-2 py-1 text-[10px] font-semibold uppercase tracking-[0.12em] text-amber-200">
                    <CheckCircle2 className="h-3.5 w-3.5" />
                    Setup Doctor
                  </span>
                  <span
                    className={cn(
                      "inline-flex items-center gap-1.5 rounded border px-2 py-1 text-[10px] font-semibold uppercase tracking-[0.08em]",
                      setupStyles.badge
                    )}
                  >
                    <span className={cn("h-1.5 w-1.5 rounded-full", setupStyles.dot)} />
                    {DOCTOR_LABELS[setupState]}
                  </span>
                </div>
                <h3 className="mt-3 text-lg font-semibold tracking-normal text-surface-100">
                  {setupHeadline}
                </h3>
                <p className="mt-1 max-w-2xl text-xs leading-relaxed text-surface-500">
                  One proxy endpoint, model-name routing, account quota, then harness automation.
                </p>
              </div>
              <div className="flex flex-shrink-0 items-center gap-2">
                <span className="rounded border border-surface-800 bg-surface-900 px-2 py-1 font-mono text-[11px] text-surface-400">
                  {readyDoctorItems}/{doctorItems.length} ready
                </span>
                <button
                  onClick={recheckRuntime}
                  className="inline-flex min-h-11 items-center justify-center gap-1.5 rounded border border-surface-800 px-2.5 py-2 text-[11px] text-surface-400 transition-colors hover:border-surface-700 hover:text-surface-100 active:translate-y-px"
                  type="button"
                >
                  <RefreshCw className={cn("h-3.5 w-3.5", runtime.status === "loading" && "animate-spin")} />
                  Recheck
                </button>
              </div>
            </div>

            <div className="h-1 overflow-hidden rounded-full bg-surface-800">
              <div
                className="h-full rounded-full bg-amber-300 transition-[width] duration-300"
                style={{ width: `${setupProgressPercent}%` }}
              />
            </div>

            <div className="mt-4 grid md:grid-cols-2 2xl:grid-cols-4">
              <SetupStep
                icon={Server}
                label="1. Connect"
                value={anthropicBaseEnv}
                detail="Claude Code keeps its harness; traffic goes through the local proxy."
                copyLabel="Copy Anthropic base URL"
                copied={copiedKey === "base-url"}
                onCopy={() => copyRuntimeText("base-url", anthropicBaseEnv)}
              />
              <SetupStep
                icon={GitBranch}
                label="2. Route"
                value="claude-* / gpt-* / local"
                detail="The requested model name chooses the backend family."
              />
              <SetupStep
                icon={KeyRound}
                label="3. Spend"
                value={`${accountCount} account${accountCount === 1 ? "" : "s"}`}
                detail="Accounts are the identities that consume provider quota."
              />
              <SetupStep
                icon={Database}
                label="4. Balance"
                value={`${enabledCollectors.length} collector${enabledCollectors.length === 1 ? "" : "s"}, ${runtimeLeases.length} lease${runtimeLeases.length === 1 ? "" : "s"}`}
                detail="Quota snapshots and session leases steer account selection."
              />
            </div>
          </div>

          <div className="border-t border-surface-800 bg-surface-900/60 p-4 lg:border-l lg:border-t-0">
            <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
              Next action
            </p>
            <p className="mt-2 text-sm font-semibold text-surface-100">
              {setupState === "ready" ? "Launch Claude Code" : nextDoctorItem.title}
            </p>
            <p className="mt-2 text-xs leading-relaxed text-surface-500">{setupDetail}</p>
            <div className="mt-4 flex flex-col gap-2">
              <button
                onClick={() => {
                  if (setupState === "ready") {
                    void copyRuntimeText("launch-command", powerShellLaunch);
                    return;
                  }
                  onNavigate?.(nextDoctorItem.section);
                }}
                disabled={setupState !== "ready" && !onNavigate}
                className="inline-flex min-h-11 items-center justify-center gap-1.5 rounded border border-amber-500/30 bg-amber-500/10 px-2.5 py-2 text-xs font-medium text-amber-200 transition-colors hover:border-amber-500/50 active:translate-y-px disabled:cursor-not-allowed disabled:opacity-50"
                type="button"
              >
                {setupState === "ready" && copiedKey === "launch-command" ? (
                  <Check className="h-3.5 w-3.5" />
                ) : setupState === "ready" ? (
                  <Clipboard className="h-3.5 w-3.5" />
                ) : (
                  <ArrowRight className="h-3.5 w-3.5" />
                )}
                {setupState === "ready"
                  ? copiedKey === "launch-command"
                    ? "Copied launch"
                    : "Copy launch"
                  : `Open ${nextDoctorItem.actionLabel}`}
              </button>
              <button
                onClick={() => copyRuntimeText("base-url", anthropicBaseEnv)}
                className="inline-flex min-h-11 items-center justify-center gap-1.5 rounded border border-surface-800 px-2.5 py-2 text-xs text-surface-400 transition-colors hover:border-surface-700 hover:text-surface-100 active:translate-y-px"
                type="button"
              >
                {copiedKey === "base-url" ? <Check className="h-3.5 w-3.5" /> : <Clipboard className="h-3.5 w-3.5" />}
                {copiedKey === "base-url" ? "Copied env" : "Copy env"}
              </button>
            </div>
            <p className="mt-3 font-mono text-[11px] text-surface-600">
              Last check {formatCheckTime(lastRuntimeCheck)}
            </p>
          </div>
        </div>

        <div className="flex flex-col gap-2 border-t border-surface-800 bg-surface-900/70 px-4 py-3 md:flex-row md:items-center">
          <span className="flex-shrink-0 text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            PowerShell
          </span>
          <code className="min-w-0 flex-1 break-all font-mono text-xs text-surface-300">
            {powerShellLaunch}
          </code>
          <button
            onClick={() => copyRuntimeText("launch-command", powerShellLaunch)}
            className="inline-flex min-h-11 items-center justify-center gap-1.5 rounded border border-surface-800 px-2.5 py-2 text-xs text-surface-400 transition-colors hover:border-surface-700 hover:text-surface-100 active:translate-y-px"
            title="Copy launch command"
            type="button"
          >
            {copiedKey === "launch-command" ? <Check className="h-3.5 w-3.5" /> : <Clipboard className="h-3.5 w-3.5" />}
            Copy
          </button>
        </div>
      </div>

      <div className="mt-5 grid gap-3 md:grid-cols-4">
        <Metric
          label="Runtime"
          value={
            activeRuntime
              ? `${activeRuntime.provider || "none"} / ${formatRuntimeGroup(activeRuntime.selectionGroup)}`
              : runtime.status === "loading"
                ? "loading"
                : "offline"
          }
          detail={activeRuntime?.model || runtime.error || "Waiting for local proxy status"}
          tone={runtimeTone}
        />
        <Metric
          label="Model"
          value={selectedModel?.label ?? settings.model}
          detail={selectedModel?.description ?? "Custom model string"}
          tone="good"
        />
        <Metric
          label="Endpoint"
          value={settings.apiUrl.replace(/^https?:\/\//, "")}
          detail={settings.streamingEnabled ? "Streaming enabled" : "Streaming disabled"}
          tone={settings.apiUrl ? "good" : "warn"}
        />
        <Metric
          label="Data"
          value={`${conversations.length} conversations`}
          detail={`${totalMessages} retained messages in local storage`}
          tone={conversations.length > 0 ? "neutral" : "good"}
        />
      </div>

      <div className="mt-5 rounded-lg border border-surface-800 bg-surface-950/30 p-4">
        <div className="mb-2 flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
          <div>
            <div className="flex items-center gap-2">
              <CheckCircle2 className="h-4 w-4 text-amber-200" />
              <p className="text-sm font-medium text-surface-200">Setup Doctor</p>
            </div>
            <p className="mt-1 text-xs leading-relaxed text-surface-500">
              A quick readiness pass for the proxy, launch command, accounts, routing, and quota loop.
            </p>
            <p className="mt-1 font-mono text-[11px] text-surface-600">
              Last check {formatCheckTime(lastRuntimeCheck)}
            </p>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <span className="rounded border border-surface-800 bg-surface-900 px-2 py-1 font-mono text-[11px] text-surface-500">
              {readyDoctorItems}/{doctorItems.length} ready
            </span>
            {blockingDoctorItems > 0 && (
              <span className="inline-flex items-center gap-1.5 rounded border border-amber-500/20 bg-amber-500/10 px-2 py-1 text-[11px] text-amber-300">
                <AlertTriangle className="h-3.5 w-3.5" />
                {blockingDoctorItems} needs action
              </span>
            )}
            <button
              onClick={recheckRuntime}
              className="inline-flex min-h-11 items-center justify-center gap-1.5 rounded border border-surface-800 px-2.5 py-2 text-[11px] text-surface-400 transition-colors hover:border-surface-700 hover:text-surface-100 active:translate-y-px"
              type="button"
            >
              <RefreshCw className={cn("h-3.5 w-3.5", runtime.status === "loading" && "animate-spin")} />
              Recheck
            </button>
          </div>
        </div>
        <div className="mt-2">
          {doctorItems.map((item) => (
            <SetupDoctorItem
              key={item.title}
              icon={item.icon}
              title={item.title}
              state={item.state}
              detail={item.detail}
              section={item.section}
              actionLabel={item.actionLabel}
              quickActions={item.quickActions}
              result={item.result}
              onNavigate={onNavigate}
            />
          ))}
        </div>
      </div>

      <div className="mt-5 rounded-lg border border-surface-800 bg-surface-950/30 px-4">
        <StatusRow
          icon={Activity}
          label="Runtime selection"
          value={runtime.data?.selection.accountId ?? runtime.data?.selection.reason ?? "not available"}
        />
        <StatusRow
          icon={KeyRound}
          label="Account credential"
          value={settings.apiKey ? `configured ...${settings.apiKey.slice(-4)}` : "not configured"}
        />
        <StatusRow
          icon={Radio}
          label="Streaming"
          value={settings.streamingEnabled ? "enabled" : "disabled"}
        />
        <StatusRow
          icon={Shield}
          label="Auto-approved tools"
          value={`${enabledTools} enabled`}
        />
        <StatusRow
          icon={GitBranch}
          label="Router"
          value={runtime.data ? (runtime.data.routerEnabled ? "enabled" : "disabled") : "unknown"}
        />
        <StatusRow
          icon={Server}
          label="MCP integrations"
          value={`${settings.mcpServers.length} configured`}
        />
        <StatusRow
          icon={Database}
          label="Telemetry"
          value={settings.telemetryEnabled ? "enabled" : "disabled"}
        />
      </div>

      <div className="mt-5 rounded-lg border border-surface-800 bg-surface-950/30 p-4">
        <div className="mb-2 flex items-center gap-2">
          <Activity className="h-4 w-4 text-surface-500" />
          <p className="text-sm font-medium text-surface-200">What lives where</p>
        </div>
        <div className="mt-2">
          <ConceptRow
            label="Providers"
            value="Network endpoints and model catalogs. Change base URLs and transport behavior here."
            owner="Runtime"
          />
          <ConceptRow
            label="Accounts"
            value="Credentials and identities that spend quota. Check active leases and health here."
            owner="Runtime"
          />
          <ConceptRow
            label="Routing"
            value="Model string policy. Claude names stay Claude; GPT names move to Codex-compatible providers."
            owner="Runtime"
          />
          <ConceptRow
            label="Scheduler"
            value="Account choice, quota windows, cooldowns, collector sources, and session stickiness."
            owner="Runtime"
          />
          <ConceptRow
            label="Harness"
            value="Agents, commands, skills, loops, verification, and handoffs. These should survive model swaps."
            owner="Harness"
          />
        </div>
      </div>
    </div>
  );
}
