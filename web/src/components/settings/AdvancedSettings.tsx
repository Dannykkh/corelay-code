
import { useMemo, useState } from "react";
import { AlertTriangle, Check, Clipboard, FileJson, RotateCcw } from "lucide-react";
import { useSettingsStore } from "@/lib/settings-store";
import { SectionActionStrip, SectionHeader, SettingRow } from "./SettingRow";
import { cn } from "@/lib/utils";

export function AdvancedSettings() {
  const { settings, resetSettings } = useSettingsStore();
  const [confirmReset, setConfirmReset] = useState(false);
  const [copiedJson, setCopiedJson] = useState(false);

  const safeSettings = useMemo(() => {
    return {
      ...settings,
      apiKey: settings.apiKey ? `********${settings.apiKey.slice(-4)}` : "",
    };
  }, [settings]);
  const safeSettingsJson = useMemo(() => JSON.stringify(safeSettings, null, 2), [safeSettings]);

  function resetAll() {
    resetSettings();
    setConfirmReset(false);
  }

  async function copyMaskedJson() {
    try {
      await navigator.clipboard.writeText(safeSettingsJson);
      setCopiedJson(true);
      window.setTimeout(() => setCopiedJson(false), 1600);
    } catch {
      setCopiedJson(false);
    }
  }

  return (
    <div>
      <SectionHeader
        eyebrow="Workspace"
        title="Advanced"
        description="Expert-only diagnostics and reset controls. Credentials are masked in previews."
      />

      <SectionActionStrip
        icon={FileJson}
        title={confirmReset ? "Reset confirmation is armed" : "Advanced tools are read-only until reset is armed"}
        description={
          confirmReset
            ? "Use the reset row below to confirm or cancel. Conversation history is not deleted by this action."
            : "Copy the masked JSON for handoff/debugging. Reset requires an explicit second confirmation."
        }
        stateLabel={confirmReset ? "armed" : "masked"}
        stateTone={confirmReset ? "danger" : "good"}
        metrics={[
          {
            label: "JSON preview",
            value: "masked",
            tone: "good",
          },
          {
            label: "API key",
            value: settings.apiKey ? "masked" : "none",
            tone: settings.apiKey ? "good" : "neutral",
          },
          {
            label: "Reset",
            value: confirmReset ? "armed" : "safe",
            tone: confirmReset ? "danger" : "good",
          },
        ]}
        actions={[
          {
            label: copiedJson ? "Copied JSON" : "Copy masked JSON",
            icon: copiedJson ? Check : Clipboard,
            onClick: copyMaskedJson,
            tone: copiedJson ? "good" : "neutral",
          },
          {
            label: confirmReset ? "Reset armed below" : "Arm reset settings",
            icon: AlertTriangle,
            onClick: () => setConfirmReset(true),
            disabled: confirmReset,
            tone: "danger",
          },
          {
            label: "Cancel reset arm",
            icon: RotateCcw,
            onClick: () => setConfirmReset(false),
            disabled: !confirmReset,
            tone: "neutral",
          },
        ]}
      />

      <SettingRow
        label="Settings JSON preview"
        description="Read-only snapshot of local settings for debugging or handoff notes."
        stack
        scope="Persisted Data"
        risk="medium"
      >
        <pre className="max-h-72 overflow-auto rounded-lg border border-surface-800 bg-surface-950/60 p-3 font-mono text-xs leading-relaxed text-surface-400">
          {safeSettingsJson}
        </pre>
      </SettingRow>

      <SettingRow
        label="Reset all settings"
        description="Return local settings to defaults. Conversation history is not deleted."
        scope="Persisted Data"
        risk="high"
      >
        {confirmReset ? (
          <div className="flex flex-wrap items-center justify-end gap-2">
            <span className="flex items-center gap-1 text-xs text-red-400">
              <AlertTriangle className="h-3.5 w-3.5" />
              Reset local settings?
            </span>
            <button
              onClick={resetAll}
              className="min-h-11 rounded-md bg-red-600 px-3 py-2 text-xs text-white transition-colors hover:bg-red-700 active:translate-y-px"
              type="button"
            >
              Reset
            </button>
            <button
              onClick={() => setConfirmReset(false)}
              className="min-h-11 rounded-md px-3 py-2 text-xs text-surface-400 transition-colors hover:text-surface-200"
              type="button"
            >
              Cancel
            </button>
          </div>
        ) : (
          <button
            onClick={() => setConfirmReset(true)}
            className={cn(
              "flex min-h-11 items-center gap-1.5 rounded-md border border-red-500/30 px-3 py-2 text-xs",
              "text-red-400 transition-colors hover:bg-red-500/10 active:translate-y-px"
            )}
            type="button"
          >
            <RotateCcw className="h-3.5 w-3.5" />
            Reset settings
          </button>
        )}
      </SettingRow>
    </div>
  );
}
