
import { useState } from "react";
import {
  CheckCircle2,
  Clock3,
  Database,
  FilePlus2,
  Plus,
  Power,
  PowerOff,
  RefreshCw,
  ShieldCheck,
  Trash2,
} from "lucide-react";
import { useSettingsStore } from "@/lib/settings-store";
import {
  formatRuntimeGroup,
  type RuntimeProviderGroup,
  type RuntimeQuotaCollectorInfo,
  useRuntimeStatus,
} from "@/hooks/useRuntimeStatus";
import { SectionActionStrip, SectionHeader } from "./SettingRow";
import { cn } from "@/lib/utils";

type SourceType = "file" | "http";

interface QuotaSourceForm {
  name: string;
  type: SourceType;
  path: string;
  url: string;
  intervalSeconds: string;
  timeoutSeconds: string;
  headerName: string;
  headerValue: string;
}

interface QuotaSourceSampleResponse {
  path?: string;
  source?: RuntimeQuotaCollectorInfo;
}

const EMPTY_SOURCE_FORM: QuotaSourceForm = {
  name: "",
  type: "file",
  path: "",
  url: "",
  intervalSeconds: "60",
  timeoutSeconds: "5",
  headerName: "",
  headerValue: "",
};

function formatCollectorTarget(collector: { type: string; path?: string; url?: string }) {
  if (collector.type === "file") {
    return collector.path || "file path not set";
  }
  if (collector.type === "http") {
    return collector.url || "URL not set";
  }
  return collector.path || collector.url || "source target not set";
}

function formatCollectorInterval(seconds?: number) {
  if (!seconds || seconds <= 0) {
    return "60s default";
  }
  if (seconds < 60) {
    return `${seconds}s`;
  }
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) {
    return `${minutes}m`;
  }
  return `${Math.round(minutes / 60)}h`;
}

function normalizeApiUrl(apiUrl: string) {
  return apiUrl.trim().replace(/\/+$/, "");
}

function sourcePayload(form: QuotaSourceForm) {
  const intervalSeconds = Number(form.intervalSeconds);
  const timeoutSeconds = Number(form.timeoutSeconds);
  const headers =
    form.type === "http" && form.headerName.trim() && form.headerValue.trim()
      ? { [form.headerName.trim()]: form.headerValue.trim() }
      : undefined;

  return {
    name: form.name.trim(),
    type: form.type,
    path: form.type === "file" ? form.path.trim() : undefined,
    url: form.type === "http" ? form.url.trim() : undefined,
    intervalSeconds: Number.isFinite(intervalSeconds) && intervalSeconds > 0 ? intervalSeconds : undefined,
    timeoutSeconds: Number.isFinite(timeoutSeconds) && timeoutSeconds > 0 ? timeoutSeconds : undefined,
    headers,
  };
}

export function SchedulerSettings() {
  const { settings } = useSettingsStore();
  const runtime = useRuntimeStatus(settings.apiUrl);
  const scheduler = runtime.data?.scheduler;
  const selection = runtime.data?.selection;
  const quotaCollectors = runtime.data?.quotaCollectors ?? [];
  const enabledCollectors = quotaCollectors.filter((collector) => collector.enabled);
  const [sourceForm, setSourceForm] = useState<QuotaSourceForm>(EMPTY_SOURCE_FORM);
  const [sourceStatus, setSourceStatus] = useState<"idle" | "saving" | "testing" | "error" | "success">("idle");
  const [sourceError, setSourceError] = useState<string | null>(null);
  const [sourceResult, setSourceResult] = useState<string | null>(null);

  async function quotaSourceRequest(path: string, init: RequestInit) {
    const baseUrl = normalizeApiUrl(settings.apiUrl);
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
    return response.json();
  }

  async function mutateQuotaSource(path: string, init: RequestInit) {
    await quotaSourceRequest(path, init);
    await runtime.refresh();
  }

  function describeTestResult(data: { accountCount?: number; accounts?: Array<{ provider?: string; group?: RuntimeProviderGroup }> }) {
    const first = data.accounts?.[0];
    const accountCount = data.accountCount ?? data.accounts?.length ?? 0;
    const suffix = first?.provider ? `, first: ${first.provider}${first.group ? `/${formatRuntimeGroup(first.group)}` : ""}` : "";
    return `Test passed: ${accountCount} account${accountCount === 1 ? "" : "s"} parsed${suffix}`;
  }

  async function addQuotaSource() {
    setSourceStatus("saving");
    setSourceError(null);
    setSourceResult(null);
    try {
      await mutateQuotaSource("/api/runtime/quota-sources", {
        method: "POST",
        body: JSON.stringify(sourcePayload(sourceForm)),
      });
      setSourceForm(EMPTY_SOURCE_FORM);
      setSourceStatus("success");
      setSourceResult("Quota source added and collector refreshed.");
    } catch (error) {
      setSourceStatus("error");
      setSourceError(error instanceof Error ? error.message : "Could not save quota source");
    }
  }

  async function testQuotaSourceFromForm() {
    setSourceStatus("testing");
    setSourceError(null);
    setSourceResult(null);
    try {
      const data = await quotaSourceRequest("/api/runtime/quota-sources/test", {
        method: "POST",
        body: JSON.stringify(sourcePayload(sourceForm)),
      });
      setSourceStatus("success");
      setSourceResult(describeTestResult(data));
    } catch (error) {
      setSourceStatus("error");
      setSourceError(error instanceof Error ? error.message : "Could not test quota source");
    }
  }

  async function testSavedQuotaSource(index: number) {
    setSourceStatus("testing");
    setSourceError(null);
    setSourceResult(null);
    try {
      const data = await quotaSourceRequest(`/api/runtime/quota-sources/${index}/test`, {
        method: "POST",
      });
      setSourceStatus("success");
      setSourceResult(describeTestResult(data));
    } catch (error) {
      setSourceStatus("error");
      setSourceError(error instanceof Error ? error.message : "Could not test quota source");
    }
  }

  async function createSampleSource() {
    setSourceStatus("saving");
    setSourceError(null);
    setSourceResult(null);
    try {
      const data = (await quotaSourceRequest("/api/runtime/quota-sources/sample", {
        method: "POST",
      })) as QuotaSourceSampleResponse;
      const path = data.path || data.source?.path;
      if (!path) {
        throw new Error("Runtime API did not return a sample path");
      }
      setSourceForm((current) => ({
        ...current,
        name: data.source?.name || "local quota sample",
        type: "file",
        path,
        url: "",
        intervalSeconds: String(data.source?.intervalSeconds || current.intervalSeconds || "60"),
        timeoutSeconds: String(data.source?.timeoutSeconds || current.timeoutSeconds || "5"),
        headerName: "",
        headerValue: "",
      }));
      setSourceStatus("success");
      setSourceResult(`Sample file ready: ${path}`);
    } catch (error) {
      setSourceStatus("error");
      setSourceError(error instanceof Error ? error.message : "Could not create sample quota file");
    }
  }

  async function toggleQuotaSource(index: number, enabled: boolean) {
    setSourceStatus("saving");
    setSourceError(null);
    setSourceResult(null);
    try {
      await mutateQuotaSource(`/api/runtime/quota-sources/${index}`, {
        method: "PATCH",
        body: JSON.stringify({ disabled: enabled }),
      });
      setSourceStatus("success");
    } catch (error) {
      setSourceStatus("error");
      setSourceError(error instanceof Error ? error.message : "Could not update quota source");
    }
  }

  async function deleteQuotaSource(index: number) {
    setSourceStatus("saving");
    setSourceError(null);
    setSourceResult(null);
    try {
      await mutateQuotaSource(`/api/runtime/quota-sources/${index}`, {
        method: "DELETE",
      });
      setSourceStatus("success");
    } catch (error) {
      setSourceStatus("error");
      setSourceError(error instanceof Error ? error.message : "Could not delete quota source");
    }
  }

  const sourceTargetFilled = sourceForm.type === "file" ? sourceForm.path.trim() : sourceForm.url.trim();
  const sourceBusy = sourceStatus === "saving" || sourceStatus === "testing";
  const canUseSourceForm = Boolean(sourceTargetFilled) && !sourceBusy;
  const runtimeOnline = runtime.status === "success" && Boolean(runtime.data);
  const schedulerState = !runtimeOnline
    ? runtime.status === "loading"
      ? "checking"
      : "offline"
    : enabledCollectors.length > 0
      ? "ready"
      : "needs source";
  const schedulerTone = runtimeOnline && enabledCollectors.length > 0
    ? "good"
    : runtime.status === "loading"
      ? "neutral"
      : "warn";
  const schedulerTitle =
    runtimeOnline && enabledCollectors.length > 0
      ? "Quota snapshots are feeding scheduler decisions"
      : runtimeOnline
        ? "Create or test a quota source"
        : "Start the runtime before scheduler setup";
  const schedulerDetail =
    runtimeOnline && enabledCollectors.length > 0
      ? "Collectors are active. Test a source if quota windows look stale before a long run."
      : runtimeOnline
        ? "Use a sample file first, then test and add the quota source when the parsed accounts look correct."
        : runtime.error || "The scheduler needs the local proxy API before it can create samples or test quota sources.";
  const policies = [
    {
      icon: Clock3,
      label: "Quota windows",
      value: scheduler
        ? `${Math.round(scheduler.fiveHourMax * 100)}% / ${Math.round(scheduler.sevenDayMax * 100)}%`
        : "5 hour / 7 day",
      description: "Use quota that will reset soon before it expires unused.",
      tone: "good",
    },
    {
      icon: RefreshCw,
      label: "Session stickiness",
      value: scheduler ? `${Math.round(scheduler.switchMargin * 100)}% margin` : "Conversation scoped",
      description: "Keep a session on the same account unless health or quota requires a move.",
      tone: "neutral",
    },
    {
      icon: ShieldCheck,
      label: "Stale usage gate",
      value: scheduler ? `${Math.round(scheduler.staleAfterSeconds / 60)} min` : "15 min",
      description: "Treat old usage snapshots as unsafe unless the account explicitly allows stale data.",
      tone: "warn",
    },
  ];

  return (
    <div>
      <SectionHeader
        eyebrow="Runtime"
        title="Scheduler"
        description="A dedicated home for account-picking behavior, quota burn-down, cooldowns, and session stickiness."
      >
        <button
          onClick={runtime.refresh}
          className="flex h-11 w-11 items-center justify-center rounded border border-surface-800 text-surface-400 transition-colors hover:border-surface-700 hover:text-surface-200 active:translate-y-px"
          title="Refresh scheduler status"
          type="button"
        >
          <RefreshCw className={cn("h-3.5 w-3.5", runtime.status === "loading" && "animate-spin")} />
        </button>
      </SectionHeader>

      <SectionActionStrip
        icon={Database}
        title={schedulerTitle}
        description={schedulerDetail}
        stateLabel={schedulerState}
        stateTone={schedulerTone}
        metrics={[
          {
            label: "Collectors",
            value: `${enabledCollectors.length}/${quotaCollectors.length}`,
            tone: enabledCollectors.length > 0 ? "good" : "warn",
          },
          {
            label: "Accounts",
            value: `${runtime.data?.accounts.length ?? 0}`,
            tone: (runtime.data?.accounts.length ?? 0) > 0 ? "good" : "neutral",
          },
          {
            label: "Selection",
            value: selection?.action ?? "unknown",
            tone: selection?.accountId ? "good" : "neutral",
          },
        ]}
        actions={[
          {
            label: sourceBusy && sourceStatus === "saving" ? "Creating sample" : "Create sample source",
            icon: FilePlus2,
            onClick: createSampleSource,
            disabled: sourceBusy || !runtimeOnline,
            tone: quotaCollectors.length === 0 ? "warn" : "neutral",
          },
          {
            label: sourceBusy && sourceStatus === "testing" ? "Testing source" : "Test draft source",
            icon: CheckCircle2,
            onClick: testQuotaSourceFromForm,
            disabled: !canUseSourceForm || !runtimeOnline,
            tone: canUseSourceForm ? "good" : "neutral",
          },
          {
            label: "Add draft source",
            icon: Plus,
            onClick: addQuotaSource,
            disabled: !canUseSourceForm || !runtimeOnline,
            tone: canUseSourceForm ? "good" : "neutral",
          },
          {
            label: runtime.status === "loading" ? "Refreshing scheduler" : "Refresh scheduler",
            icon: RefreshCw,
            onClick: runtime.refresh,
            disabled: runtime.status === "loading",
            tone: runtimeOnline ? "good" : "neutral",
          },
        ]}
      />

      <div
        className={cn(
          "rounded-lg border p-3 text-xs leading-relaxed",
          runtime.status === "success"
            ? "border-green-500/20 bg-green-500/10 text-green-200"
            : runtime.status === "error"
              ? "border-amber-500/20 bg-amber-500/10 text-amber-200"
              : "border-surface-800 bg-surface-950/30 text-surface-400"
        )}
      >
        {runtime.status === "success"
          ? runtime.data?.quotaSource
          : runtime.status === "error"
            ? runtime.error
            : "Loading scheduler contract from the local proxy runtime."}
      </div>

      <div className="mt-4 rounded-lg border border-surface-800 bg-surface-950/30 p-4">
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <Database className="h-4 w-4 text-surface-500" />
              <p className="text-sm font-medium text-surface-100">Quota collectors</p>
            </div>
            <p className="mt-1 text-xs leading-relaxed text-surface-500">
              Snapshot sources that feed five-hour and seven-day quota into account selection.
            </p>
          </div>
          <span className="rounded border border-surface-800 bg-surface-900 px-2 py-1 font-mono text-xs text-surface-400">
            {enabledCollectors.length}/{quotaCollectors.length}
          </span>
        </div>
        <div className="mt-3 divide-y divide-surface-800">
          {quotaCollectors.map((collector) => (
            <div
              key={`${collector.index}-${collector.type}-${collector.name || collector.path || collector.url}`}
              className="grid gap-2 py-2.5 md:grid-cols-[8rem_minmax(0,1fr)_6rem_7.5rem] md:items-center"
            >
              <div className="flex items-center gap-2">
                <span
                  className={cn(
                    "h-2 w-2 rounded-full",
                    collector.enabled ? "bg-green-400" : "bg-surface-600"
                  )}
                />
                <span className="truncate text-sm text-surface-200">{collector.name || collector.type}</span>
              </div>
              <p className="min-w-0 break-all font-mono text-xs text-surface-500">
                {formatCollectorTarget(collector)}
                {collector.headerNames?.length ? (
                  <span className="ml-2 text-surface-600">
                    headers: {collector.headerNames.join(", ")}
                  </span>
                ) : null}
              </p>
              <p className="font-mono text-xs text-surface-600 md:text-right">
                {formatCollectorInterval(collector.intervalSeconds)}
              </p>
              <div className="flex items-center gap-1 md:justify-end">
                <button
                  onClick={() => testSavedQuotaSource(collector.index)}
                  disabled={sourceBusy}
                  className="flex h-11 w-11 items-center justify-center rounded border border-surface-800 text-surface-400 transition-colors hover:border-green-500/40 hover:text-green-300 active:translate-y-px disabled:opacity-50"
                  title="Test quota source"
                  type="button"
                >
                  <CheckCircle2 className="h-3.5 w-3.5" />
                </button>
                <button
                  onClick={() => toggleQuotaSource(collector.index, collector.enabled)}
                  disabled={sourceBusy}
                  className="flex h-11 w-11 items-center justify-center rounded border border-surface-800 text-surface-400 transition-colors hover:border-surface-700 hover:text-surface-200 active:translate-y-px disabled:opacity-50"
                  title={collector.enabled ? "Disable quota source" : "Enable quota source"}
                  type="button"
                >
                  {collector.enabled ? <PowerOff className="h-3.5 w-3.5" /> : <Power className="h-3.5 w-3.5" />}
                </button>
                <button
                  onClick={() => deleteQuotaSource(collector.index)}
                  disabled={sourceBusy}
                  className="flex h-11 w-11 items-center justify-center rounded border border-surface-800 text-surface-500 transition-colors hover:border-red-500/40 hover:text-red-300 active:translate-y-px disabled:opacity-50"
                  title="Delete quota source"
                  type="button"
                >
                  <Trash2 className="h-3.5 w-3.5" />
                </button>
              </div>
            </div>
          ))}
          {quotaCollectors.length === 0 && (
            <div className="py-2.5">
              <p className="text-sm text-surface-300">No collector source configured</p>
              <p className="mt-1 text-xs leading-relaxed text-surface-500">
                Provider response headers and manual telemetry pushes still update short-window state.
              </p>
            </div>
          )}
        </div>
        <div className="mt-4 border-t border-surface-800 pt-4">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Add Source
          </p>
          <div className="mt-3 grid gap-2 lg:grid-cols-[7rem_minmax(10rem,1fr)_minmax(0,1.6fr)_5.5rem_5.5rem]">
            <select
              value={sourceForm.type}
              onChange={(event) => setSourceForm((current) => ({ ...current, type: event.target.value as SourceType }))}
              className="min-h-11 rounded-md border border-surface-700 bg-surface-800 px-2 py-2 text-sm text-surface-200 focus:outline-none focus:ring-1 focus:ring-brand-500"
            >
              <option value="file">file</option>
              <option value="http">http</option>
            </select>
            <input
              value={sourceForm.name}
              onChange={(event) => setSourceForm((current) => ({ ...current, name: event.target.value }))}
              placeholder="name"
              className="min-h-11 truncate rounded-md border border-surface-700 bg-surface-800 px-2 py-2 text-sm text-surface-200 placeholder-surface-600 focus:outline-none focus:ring-1 focus:ring-brand-500"
            />
            <textarea
              value={sourceForm.type === "file" ? sourceForm.path : sourceForm.url}
              onChange={(event) =>
                setSourceForm((current) => ({
                  ...current,
                  [current.type === "file" ? "path" : "url"]: event.target.value,
                }))
              }
              placeholder={sourceForm.type === "file" ? "snapshot.json" : "https://.../quota"}
              rows={2}
              className="min-h-11 resize-none rounded-md border border-surface-700 bg-surface-800 px-2 py-2 font-mono text-sm leading-snug text-surface-200 placeholder-surface-600 focus:outline-none focus:ring-1 focus:ring-brand-500"
            />
            <input
              value={sourceForm.intervalSeconds}
              onChange={(event) => setSourceForm((current) => ({ ...current, intervalSeconds: event.target.value }))}
              placeholder="60"
              inputMode="numeric"
              className="min-h-11 rounded-md border border-surface-700 bg-surface-800 px-2 py-2 font-mono text-sm text-surface-200 placeholder-surface-600 focus:outline-none focus:ring-1 focus:ring-brand-500"
            />
            <input
              value={sourceForm.timeoutSeconds}
              onChange={(event) => setSourceForm((current) => ({ ...current, timeoutSeconds: event.target.value }))}
              placeholder="5"
              inputMode="numeric"
              className="min-h-11 rounded-md border border-surface-700 bg-surface-800 px-2 py-2 font-mono text-sm text-surface-200 placeholder-surface-600 focus:outline-none focus:ring-1 focus:ring-brand-500"
            />
          </div>
          <div className="mt-2 flex flex-wrap gap-2">
            <button
              onClick={createSampleSource}
              disabled={sourceBusy}
              className="inline-flex min-h-11 items-center gap-1.5 rounded-md border border-surface-700 px-2.5 py-2 text-xs text-surface-300 transition-colors hover:bg-surface-800 hover:text-surface-100 active:translate-y-px disabled:cursor-not-allowed disabled:opacity-50"
              title="Create sample quota file"
              type="button"
            >
              <FilePlus2 className="h-3.5 w-3.5" />
              Create sample
            </button>
            <button
              onClick={testQuotaSourceFromForm}
              disabled={!canUseSourceForm}
              className="inline-flex min-h-11 items-center gap-1.5 rounded-md border border-surface-700 px-2.5 py-2 text-xs text-surface-300 transition-colors hover:bg-surface-800 hover:text-surface-100 active:translate-y-px disabled:cursor-not-allowed disabled:opacity-50"
              type="button"
            >
              <CheckCircle2 className="h-3.5 w-3.5" />
              Test source
            </button>
            <button
              onClick={addQuotaSource}
              disabled={!canUseSourceForm}
              className={cn(
                "inline-flex min-h-11 items-center gap-1.5 rounded-md border px-2.5 py-2 text-xs transition-colors active:translate-y-px disabled:cursor-not-allowed disabled:opacity-50",
                canUseSourceForm
                  ? "border-amber-500/30 bg-amber-500/10 text-amber-200 hover:border-amber-500/50"
                  : "border-surface-700 text-surface-500"
              )}
              type="button"
            >
              <Plus className="h-3.5 w-3.5" />
              Add source
            </button>
          </div>
          {sourceForm.type === "http" && (
            <div className="mt-2 grid gap-2 md:grid-cols-2">
              <input
                value={sourceForm.headerName}
                onChange={(event) => setSourceForm((current) => ({ ...current, headerName: event.target.value }))}
                placeholder="optional header name"
                className="min-h-11 rounded-md border border-surface-700 bg-surface-800 px-2 py-2 text-sm text-surface-200 placeholder-surface-600 focus:outline-none focus:ring-1 focus:ring-brand-500"
              />
              <input
                value={sourceForm.headerValue}
                onChange={(event) => setSourceForm((current) => ({ ...current, headerValue: event.target.value }))}
                placeholder="optional header value"
                type="password"
                className="min-h-11 rounded-md border border-surface-700 bg-surface-800 px-2 py-2 text-sm text-surface-200 placeholder-surface-600 focus:outline-none focus:ring-1 focus:ring-brand-500"
              />
            </div>
          )}
          {sourceError && <p className="mt-2 break-all text-xs text-red-300">{sourceError}</p>}
          {sourceResult && !sourceError && <p className="mt-2 break-all text-xs text-green-300">{sourceResult}</p>}
          {sourceStatus === "success" && !sourceError && !sourceResult && (
            <p className="mt-2 text-xs text-green-300">Quota source updated.</p>
          )}
        </div>
      </div>

      <div className="mt-4 divide-y divide-surface-800 rounded-lg border border-surface-800 bg-surface-950/30">
        {policies.map(({ icon: Icon, label, value, description, tone }) => (
          <div key={label} className="flex items-start justify-between gap-4 p-4">
            <div className="flex min-w-0 gap-3">
              <div
                className={cn(
                  "mt-0.5 flex h-8 w-8 flex-shrink-0 items-center justify-center rounded-md border",
                  tone === "good" && "border-green-500/20 bg-green-500/10 text-green-300",
                  tone === "warn" && "border-amber-500/20 bg-amber-500/10 text-amber-300",
                  tone === "neutral" && "border-surface-800 bg-surface-900 text-surface-400"
                )}
              >
                <Icon className="h-4 w-4" />
              </div>
              <div>
                <p className="text-sm font-medium text-surface-100">{label}</p>
                <p className="mt-1 text-xs leading-relaxed text-surface-500">{description}</p>
              </div>
            </div>
            <span className="flex-shrink-0 rounded border border-surface-800 bg-surface-900 px-2 py-1 font-mono text-xs text-surface-400">
              {value}
            </span>
          </div>
        ))}
      </div>

      <div className="mt-4 grid gap-3 lg:grid-cols-[minmax(0,0.9fr)_minmax(0,1.1fr)]">
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-4">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Selection Preview
          </p>
          <p className="mt-2 text-sm font-medium text-surface-100">
            {selection?.accountId ?? "No eligible account"}
          </p>
          <p className="mt-1 text-xs leading-relaxed text-surface-500">
            {selection?.reason ?? "Runtime status is not available yet."}
          </p>
          <div className="mt-3 flex flex-wrap gap-2">
            <span className="rounded border border-surface-800 bg-surface-900 px-2 py-1 font-mono text-xs text-surface-400">
              {selection?.action ?? "unknown"}
            </span>
            <span className="rounded border border-surface-800 bg-surface-900 px-2 py-1 font-mono text-xs text-surface-400">
              score {selection?.score?.toFixed(3) ?? "n/a"}
            </span>
          </div>
        </div>

        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-4">
          <div className="flex items-center justify-between gap-3">
            <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
              Runtime Accounts
            </p>
            <span className="font-mono text-[11px] text-surface-600">
              {runtime.data?.accounts.length ?? 0}
            </span>
          </div>
          <div className="mt-3 divide-y divide-surface-800">
            {(runtime.data?.accounts ?? []).map((account) => (
              <div key={account.id} className="flex items-center justify-between gap-3 py-2">
                <div className="min-w-0">
                  <p className="truncate text-sm text-surface-200">
                    {account.displayName || account.provider}
                  </p>
                  <p className="font-mono text-[11px] text-surface-600">{account.id}</p>
                </div>
                <span className="flex-shrink-0 rounded border border-surface-800 bg-surface-900 px-2 py-1 text-xs text-surface-400">
                  {formatRuntimeGroup(account.group)}
                </span>
              </div>
            ))}
            {!runtime.data?.accounts.length && (
              <p className="py-2 text-xs text-surface-500">No account snapshot loaded.</p>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
