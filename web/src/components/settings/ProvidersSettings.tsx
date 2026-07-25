
import { useState } from "react";
import { Check, Clipboard, RefreshCw, RotateCcw, Server } from "lucide-react";
import { useSettingsStore } from "@/lib/settings-store";
import { formatRuntimeGroup, useRuntimeStatus } from "@/hooks/useRuntimeStatus";
import { SectionActionStrip, SectionHeader, SettingRow, Slider, Toggle } from "./SettingRow";
import { cn } from "@/lib/utils";

export function ProvidersSettings() {
  const { settings, updateSettings, resetSettings } = useSettingsStore();
  const runtime = useRuntimeStatus(settings.apiUrl);
  const [copiedEnv, setCopiedEnv] = useState(false);
  const modelCount = runtime.data?.providers.reduce((sum, provider) => sum + provider.models.length, 0) ?? 0;
  const runtimeBaseUrl = settings.apiUrl.trim() || "http://localhost:3001";
  const runtimeOnline = runtime.status === "success" && Boolean(runtime.data);
  const hasProviders = (runtime.data?.providers.length ?? 0) > 0;
  const providerState = runtimeOnline ? "ready" : runtime.status === "loading" ? "checking" : "needs setup";
  const providerTone = runtimeOnline ? "good" : runtime.status === "loading" ? "neutral" : "warn";
  const providerNextTitle = runtimeOnline
    ? "Provider endpoint is ready"
    : settings.apiUrl.trim()
      ? "Start or fix the local proxy"
      : "Set the API base URL";
  const providerNextDetail = runtimeOnline
    ? "The provider inventory is available. Use Routing next if model-name dispatch needs adjustment."
    : runtime.error || "Use the local proxy endpoint first, then refresh provider inventory.";

  async function copyEnv() {
    try {
      await navigator.clipboard.writeText(`ANTHROPIC_BASE_URL=${runtimeBaseUrl}`);
      setCopiedEnv(true);
      window.setTimeout(() => setCopiedEnv(false), 1600);
    } catch {
      setCopiedEnv(false);
    }
  }

  return (
    <div>
      <SectionHeader
        eyebrow="Runtime"
        title="Providers"
        description="Configure transport and request behavior for the upstream provider or local proxy."
        onReset={() => resetSettings("api")}
      >
        <button
          onClick={runtime.refresh}
          className="flex h-11 w-11 items-center justify-center rounded border border-surface-800 text-surface-400 transition-colors hover:border-surface-700 hover:text-surface-200 active:translate-y-px"
          title="Refresh provider inventory"
          type="button"
        >
          <RefreshCw className={cn("h-3.5 w-3.5", runtime.status === "loading" && "animate-spin")} />
        </button>
      </SectionHeader>

      <SectionActionStrip
        icon={Server}
        title={providerNextTitle}
        description={providerNextDetail}
        stateLabel={providerState}
        stateTone={providerTone}
        metrics={[
          {
            label: "Base URL",
            value: runtimeBaseUrl.replace(/^https?:\/\//, ""),
            tone: settings.apiUrl.trim() ? "good" : "warn",
          },
          {
            label: "Providers",
            value: `${runtime.data?.providers.length ?? 0}`,
            tone: hasProviders ? "good" : "neutral",
          },
          {
            label: "Models",
            value: `${modelCount}`,
            tone: modelCount > 0 ? "good" : "neutral",
          },
        ]}
        actions={[
          {
            label: copiedEnv ? "Copied env" : "Copy ANTHROPIC_BASE_URL",
            icon: copiedEnv ? Check : Clipboard,
            onClick: copyEnv,
            tone: copiedEnv ? "good" : "neutral",
          },
          {
            label: "Refresh inventory",
            icon: RefreshCw,
            onClick: runtime.refresh,
            disabled: runtime.status === "loading",
            tone: runtimeOnline ? "good" : "warn",
          },
          {
            label: "Reset provider defaults",
            icon: RotateCcw,
            onClick: () => resetSettings("api"),
            tone: "neutral",
          },
        ]}
      />

      <div className="mb-4 grid gap-3 md:grid-cols-3">
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Active Provider
          </p>
          <p className="mt-2 truncate text-sm font-semibold text-surface-100">
            {runtime.data?.active.providerDisplayName || runtime.data?.active.provider || "Unknown"}
          </p>
          <p className="mt-1 font-mono text-xs text-surface-500">
            {runtime.data?.active.model || runtime.error || "Runtime not connected"}
          </p>
        </div>
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Inventory
          </p>
          <p className="mt-2 text-sm font-semibold text-surface-100">
            {runtime.data?.providers.length ?? 0} providers
          </p>
          <p className="mt-1 font-mono text-xs text-surface-500">{modelCount} known models</p>
        </div>
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Selection Group
          </p>
          <p className="mt-2 text-sm font-semibold text-surface-100">
            {formatRuntimeGroup(runtime.data?.active.selectionGroup)}
          </p>
          <p className="mt-1 text-xs text-surface-500">
            {runtime.data?.routerEnabled ? "Smart router enabled" : "Smart router disabled or unknown"}
          </p>
        </div>
      </div>

      <SettingRow
        label="API base URL"
        description="Use a direct provider endpoint or a local proxy endpoint such as an Anthropic-compatible harness."
        stack
        scope="Provider"
        risk="medium"
      >
        <input
          type="url"
          value={settings.apiUrl}
          onChange={(e) => updateSettings({ apiUrl: e.target.value })}
          placeholder="http://localhost:3001"
          className={cn(
            "min-h-11 w-full rounded-md border border-surface-700 bg-surface-800 px-3 py-2 font-mono text-sm",
            "text-surface-200 placeholder-surface-600 focus:outline-none focus:ring-1 focus:ring-brand-500"
          )}
        />
      </SettingRow>

      <SettingRow
        label="Streaming"
        description="Stream responses token by token as they are generated."
        scope="Provider"
        risk="low"
      >
        <Toggle
          checked={settings.streamingEnabled}
          onChange={(value) => updateSettings({ streamingEnabled: value })}
        />
      </SettingRow>

      <SettingRow
        label="Max tokens"
        description="Maximum number of tokens in the model response."
        stack
        scope="Provider"
        risk="medium"
      >
        <div className="flex items-center gap-3">
          <Slider
            value={settings.maxTokens}
            min={1000}
            max={200000}
            step={1000}
            onChange={(value) => updateSettings({ maxTokens: value })}
            showValue={false}
            className="flex-1"
          />
          <input
            type="number"
            value={settings.maxTokens}
            min={1000}
            max={200000}
            step={1000}
            onChange={(e) => updateSettings({ maxTokens: Number(e.target.value) })}
            className={cn(
              "min-h-11 w-28 rounded-md border border-surface-700 bg-surface-800 px-2 py-2 text-right font-mono text-sm",
              "text-surface-200 focus:outline-none focus:ring-1 focus:ring-brand-500"
            )}
          />
        </div>
      </SettingRow>

      <SettingRow
        label="Temperature"
        description="Controls response randomness. Lower values are more deterministic."
        stack
        scope="Provider"
        risk="low"
      >
        <Slider
          value={settings.temperature}
          min={0}
          max={1}
          step={0.05}
          onChange={(value) => updateSettings({ temperature: value })}
        />
      </SettingRow>

      <SettingRow
        label="System prompt"
        description="Custom instructions prepended to every conversation."
        stack
        scope="Runtime"
        risk="medium"
      >
        <textarea
          value={settings.systemPrompt}
          onChange={(e) => updateSettings({ systemPrompt: e.target.value })}
          placeholder="You are a helpful assistant..."
          rows={4}
          className={cn(
            "w-full resize-none rounded-md border border-surface-700 bg-surface-800 px-3 py-2 font-mono text-sm",
            "text-surface-200 placeholder-surface-600 focus:outline-none focus:ring-1 focus:ring-brand-500"
          )}
        />
      </SettingRow>
    </div>
  );
}
