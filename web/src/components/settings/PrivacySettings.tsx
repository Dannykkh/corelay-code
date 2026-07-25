
import { useState } from "react";
import { Check, Clipboard, EyeOff, RadioTower, RotateCcw, Shield } from "lucide-react";
import { useSettingsStore } from "@/lib/settings-store";
import { SectionActionStrip, SectionHeader, SettingRow, Toggle } from "./SettingRow";

export function PrivacySettings() {
  const { settings, updateSettings, resetSettings, conversations } = useSettingsStore();
  const [copiedSummary, setCopiedSummary] = useState(false);
  const telemetryOn = settings.telemetryEnabled;
  const hasApiKey = Boolean(settings.apiKey);
  const retainedConversations = conversations.length;
  const privacyTone = telemetryOn ? "warn" : "good";

  async function copyPrivacySummary() {
    const summary = [
      `Telemetry: ${telemetryOn ? "enabled" : "disabled"}`,
      `API key: ${hasApiKey ? `masked ...${settings.apiKey.slice(-4)}` : "none"}`,
      `Local conversations: ${retainedConversations}`,
    ].join("\n");

    try {
      await navigator.clipboard.writeText(summary);
      setCopiedSummary(true);
      window.setTimeout(() => setCopiedSummary(false), 1600);
    } catch {
      setCopiedSummary(false);
    }
  }

  return (
    <div>
      <SectionHeader
        eyebrow="Safety"
        title="Privacy"
        description="Keep exposure controls visible instead of burying them under data management or advanced settings."
      />

      <SectionActionStrip
        icon={Shield}
        title={telemetryOn ? "Telemetry is enabled" : "Privacy defaults are conservative"}
        description={
          telemetryOn
            ? "Anonymous telemetry can be turned off here. Conversation content is still excluded."
            : "Telemetry is off, credentials are masked, and local record deletion remains isolated in Memory & Data."
        }
        stateLabel={telemetryOn ? "telemetry on" : "private"}
        stateTone={privacyTone}
        metrics={[
          {
            label: "Telemetry",
            value: telemetryOn ? "enabled" : "off",
            tone: telemetryOn ? "warn" : "good",
          },
          {
            label: "Credential",
            value: hasApiKey ? "masked" : "none",
            tone: hasApiKey ? "good" : "neutral",
          },
          {
            label: "Local records",
            value: `${retainedConversations}`,
            tone: retainedConversations > 0 ? "neutral" : "good",
          },
        ]}
        actions={[
          {
            label: copiedSummary ? "Copied summary" : "Copy privacy summary",
            icon: copiedSummary ? Check : Clipboard,
            onClick: copyPrivacySummary,
            tone: copiedSummary ? "good" : "neutral",
          },
          {
            label: "Disable telemetry",
            icon: EyeOff,
            onClick: () => updateSettings({ telemetryEnabled: false }),
            disabled: !telemetryOn,
            tone: telemetryOn ? "warn" : "good",
          },
          {
            label: "Reset privacy defaults",
            icon: RotateCcw,
            onClick: () => resetSettings("privacy"),
            tone: "neutral",
          },
        ]}
      />

      <SettingRow
        label="Anonymous telemetry"
        description="Share anonymous usage data. Conversation content is not included."
        scope="Persisted Data"
        risk="medium"
      >
        <Toggle
          checked={settings.telemetryEnabled}
          onChange={(value) => updateSettings({ telemetryEnabled: value })}
        />
      </SettingRow>

      <div className="mt-4 grid gap-3 md:grid-cols-3">
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <div className="mb-2 flex items-center gap-2">
            <Shield className="h-4 w-4 text-surface-500" />
            <p className="text-sm font-medium text-surface-200">Credential display</p>
          </div>
          <p className="text-xs leading-relaxed text-surface-500">
            API keys stay masked by default. Current key:{" "}
            <span className="font-mono text-surface-400">
              {settings.apiKey ? `...${settings.apiKey.slice(-4)}` : "none"}
            </span>
          </p>
        </div>
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <div className="mb-2 flex items-center gap-2">
            <EyeOff className="h-4 w-4 text-surface-500" />
            <p className="text-sm font-medium text-surface-200">Screen-safe mode</p>
          </div>
          <p className="text-xs leading-relaxed text-surface-500">
            Reserved for demo and screenshot masking. This belongs here when implemented.
          </p>
        </div>
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <div className="mb-2 flex items-center gap-2">
            <RadioTower className="h-4 w-4 text-surface-500" />
            <p className="text-sm font-medium text-surface-200">Local records</p>
          </div>
          <p className="text-xs leading-relaxed text-surface-500">
            {conversations.length} conversations are retained in this browser. Deletion lives in
            Memory & Data.
          </p>
        </div>
      </div>
    </div>
  );
}
