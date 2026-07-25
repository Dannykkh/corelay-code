
import { useMemo, useState } from "react";
import { Check, Clipboard, GitBranch, RefreshCw, RotateCcw } from "lucide-react";
import { useSettingsStore } from "@/lib/settings-store";
import { MODELS } from "@/lib/constants";
import { formatRuntimeGroup, useRuntimeStatus } from "@/hooks/useRuntimeStatus";
import { SectionActionStrip, SectionHeader, SettingRow } from "./SettingRow";
import { cn } from "@/lib/utils";

function inferRoute(modelName: string) {
  const value = modelName.trim().toLowerCase();
  if (!value) {
    return {
      group: "No model string",
      detail: "Enter a model name to preview routing.",
      tone: "neutral" as const,
    };
  }
  if (value.startsWith("claude-")) {
    return {
      group: "Claude account group",
      detail: "Claude-style model names stay on the Anthropic/Claude path.",
      tone: "good" as const,
    };
  }
  if (
    value.startsWith("gpt-") ||
    value.startsWith("codex") ||
    value.startsWith("o1") ||
    value.startsWith("o3") ||
    value.startsWith("o4")
  ) {
    return {
      group: "Codex account group",
      detail: "GPT/Codex-style model names can be routed to a Codex-compatible adapter.",
      tone: "warn" as const,
    };
  }
  return {
    group: "Default provider group",
    detail: "No explicit prefix match. The runtime should use the configured fallback.",
    tone: "neutral" as const,
  };
}

export function RoutingSettings() {
  const { settings, updateSettings, resetSettings } = useSettingsStore();
  const runtime = useRuntimeStatus(settings.apiUrl);
  const [probe, setProbe] = useState(settings.model);
  const [copiedRoute, setCopiedRoute] = useState(false);
  const selectedModel = MODELS.find((model) => model.id === settings.model);
  const route = useMemo(() => inferRoute(probe), [probe]);
  const runtimeOnline = runtime.status === "success" && Boolean(runtime.data);
  const routingTone = runtimeOnline ? route.tone : runtime.status === "loading" ? "neutral" : "warn";
  const routingTitle = runtimeOnline ? "Routing preview is ready" : "Connect the runtime to confirm live routing";
  const routingDescription = runtimeOnline
    ? "Model-name routing keeps the harness stable while Claude, Codex, or fallback providers change behind it."
    : runtime.error || "The local preview still explains model-name grouping before the proxy is online.";

  async function copyRouteSummary() {
    const summary = [
      `Default model: ${settings.model}`,
      `Probe: ${probe.trim() || "none"}`,
      `Preview group: ${route.group}`,
      `Preview detail: ${route.detail}`,
      `Proxy model: ${runtime.data?.active.model || "not connected"}`,
      `Proxy group: ${formatRuntimeGroup(runtime.data?.active.modelGroup)}`,
    ].join("\n");

    try {
      await navigator.clipboard.writeText(summary);
      setCopiedRoute(true);
      window.setTimeout(() => setCopiedRoute(false), 1600);
    } catch {
      setCopiedRoute(false);
    }
  }

  return (
    <div>
      <SectionHeader
        eyebrow="Runtime"
        title="Routing"
        description="Choose models by policy and preview provider groups without touching agent or workflow definitions."
        onReset={() => resetSettings("model")}
      />

      <SectionActionStrip
        icon={GitBranch}
        title={routingTitle}
        description={routingDescription}
        stateLabel={runtimeOnline ? "ready" : runtime.status === "loading" ? "checking" : "offline"}
        stateTone={routingTone}
        metrics={[
          {
            label: "Default",
            value: selectedModel?.label || settings.model,
            tone: "good",
          },
          {
            label: "Probe group",
            value: route.group,
            tone: route.tone,
          },
          {
            label: "Proxy",
            value: runtimeOnline ? "online" : "offline",
            tone: runtimeOnline ? "good" : "warn",
          },
        ]}
        actions={[
          {
            label: copiedRoute ? "Copied route" : "Copy route summary",
            icon: copiedRoute ? Check : Clipboard,
            onClick: copyRouteSummary,
            tone: copiedRoute ? "good" : "neutral",
          },
          {
            label: "Refresh routing state",
            icon: RefreshCw,
            onClick: runtime.refresh,
            disabled: runtime.status === "loading",
            tone: runtimeOnline ? "good" : "warn",
          },
          {
            label: "Reset model defaults",
            icon: RotateCcw,
            onClick: () => resetSettings("model"),
            tone: "neutral",
          },
        ]}
      />

      <SettingRow
        label="Default model"
        description="The model used when a new conversation starts."
        scope="Runtime"
        risk="medium"
      >
        <select
          value={settings.model}
          onChange={(e) => {
            updateSettings({ model: e.target.value });
            setProbe(e.target.value);
          }}
          className={cn(
            "min-h-11 max-w-full rounded-md border border-surface-700 bg-surface-800 px-3 py-2 text-sm",
            "text-surface-200 focus:outline-none focus:ring-1 focus:ring-brand-500"
          )}
        >
          {MODELS.map((model) => (
            <option key={model.id} value={model.id}>
              {model.label} - {model.description}
            </option>
          ))}
        </select>
      </SettingRow>

      {selectedModel && (
        <div className="mb-4 rounded-lg border border-surface-800 bg-surface-950/30 px-3 py-2 text-xs text-surface-400">
          <span className="font-medium text-surface-300">{selectedModel.label}</span>
          <span> - {selectedModel.description}</span>
        </div>
      )}

      <div className="mb-4 grid gap-3 md:grid-cols-3">
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Proxy Model
          </p>
          <p className="mt-2 truncate font-mono text-xs text-surface-200">
            {runtime.data?.active.model || runtime.error || "not connected"}
          </p>
        </div>
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Model Group
          </p>
          <p className="mt-2 text-sm font-semibold text-surface-100">
            {formatRuntimeGroup(runtime.data?.active.modelGroup)}
          </p>
        </div>
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Selected Account
          </p>
          <p className="mt-2 truncate font-mono text-xs text-surface-200">
            {runtime.data?.selection.accountId || runtime.data?.selection.reason || "unknown"}
          </p>
        </div>
      </div>

      <SettingRow
        label="Model route preview"
        description="Test a model string before saving it. This mirrors the local proxy pattern without making a network call."
        stack
        scope="Runtime"
        risk="low"
      >
        <div className="grid gap-3 md:grid-cols-[minmax(0,1fr)_260px]">
          <input
            value={probe}
            onChange={(e) => setProbe(e.target.value)}
            placeholder="claude-sonnet-4-20250514 or gpt-5.5"
            className={cn(
              "min-h-11 rounded-md border border-surface-700 bg-surface-800 px-3 py-2 font-mono text-sm",
              "text-surface-200 placeholder-surface-600 focus:outline-none focus:ring-1 focus:ring-brand-500"
            )}
          />
          <div
            className={cn(
              "rounded-lg border px-3 py-2",
              route.tone === "good" && "border-green-500/20 bg-green-500/10",
              route.tone === "warn" && "border-amber-500/20 bg-amber-500/10",
              route.tone === "neutral" && "border-surface-800 bg-surface-950/30"
            )}
          >
            <div className="flex items-center gap-2">
              <GitBranch className="h-3.5 w-3.5 text-surface-400" />
              <p className="text-xs font-medium text-surface-200">{route.group}</p>
            </div>
            <p className="mt-1 text-[11px] leading-snug text-surface-500">{route.detail}</p>
          </div>
        </div>
      </SettingRow>
    </div>
  );
}
