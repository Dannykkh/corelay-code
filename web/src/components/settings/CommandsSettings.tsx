
import { useState } from "react";
import { Check, Clipboard, Keyboard, RefreshCw, Search, Terminal, Wrench } from "lucide-react";
import { useSettingsStore } from "@/lib/settings-store";
import { useHarnessStatus } from "@/hooks/useHarnessStatus";
import { SectionActionStrip, SectionHeader, SettingRow } from "./SettingRow";
import { cn } from "@/lib/utils";

const COMMAND_SURFACES = [
  {
    name: "Slash commands",
    detail: "User-invoked command prompts such as model, memory, review, or workflow commands.",
    owner: "Harness",
  },
  {
    name: "Command palette",
    detail: "Searchable UI surface for frequently used actions and settings jumps.",
    owner: "Workspace",
  },
  {
    name: "Skill commands",
    detail: "Reusable workflow prompts that can be invoked directly or preloaded by agents.",
    owner: "Harness",
  },
];

export function CommandsSettings() {
  const { settings } = useSettingsStore();
  const harness = useHarnessStatus(settings.apiUrl);
  const [copiedCommands, setCopiedCommands] = useState(false);
  const commands = harness.data?.commands ?? [];
  const registryOnline = harness.status === "success" && Boolean(harness.data);
  const commandTone = registryOnline ? "good" : harness.status === "loading" ? "neutral" : "warn";
  const commandTitle = registryOnline
    ? "Command registry is loaded"
    : harness.status === "loading"
      ? "Checking command registry"
      : "Connect the harness command API";
  const commandDescription = registryOnline
    ? "Use slash commands as stable workflow entry points; keep them separate from provider and account setup."
    : harness.error || "The runtime exposes slash commands, skill commands, and command palette metadata.";

  async function copyCommandList() {
    const commandList = commands.length
      ? commands.map((command) => `/${command.name}${command.skillName ? ` (${command.skillName})` : ""}`).join("\n")
      : "No commands loaded";

    try {
      await navigator.clipboard.writeText(commandList);
      setCopiedCommands(true);
      window.setTimeout(() => setCopiedCommands(false), 1600);
    } catch {
      setCopiedCommands(false);
    }
  }

  return (
    <div>
      <SectionHeader
        eyebrow="Harness"
        title="Commands"
        description="Commands are part of the harness capital. They should survive provider swaps and stay separate from account configuration."
      >
        <button
          onClick={harness.refresh}
          className="flex h-11 w-11 items-center justify-center rounded border border-surface-800 text-surface-400 transition-colors hover:border-surface-700 hover:text-surface-200 active:translate-y-px"
          title="Refresh commands"
          type="button"
        >
          <RefreshCw className={cn("h-3.5 w-3.5", harness.status === "loading" && "animate-spin")} />
        </button>
      </SectionHeader>

      <SectionActionStrip
        icon={Terminal}
        title={commandTitle}
        description={commandDescription}
        stateLabel={registryOnline ? "ready" : harness.status === "loading" ? "checking" : "offline"}
        stateTone={commandTone}
        metrics={[
          {
            label: "Commands",
            value: `${commands.length}`,
            tone: commands.length > 0 ? "good" : "neutral",
          },
          {
            label: "Surfaces",
            value: `${COMMAND_SURFACES.length}`,
            tone: "good",
          },
          {
            label: "Status",
            value: harness.status,
            tone: registryOnline ? "good" : harness.status === "error" ? "warn" : "neutral",
          },
        ]}
        actions={[
          {
            label: "Refresh commands",
            icon: RefreshCw,
            onClick: harness.refresh,
            disabled: harness.status === "loading",
            tone: registryOnline ? "good" : "warn",
          },
          {
            label: copiedCommands ? "Copied commands" : "Copy command list",
            icon: copiedCommands ? Check : Clipboard,
            onClick: copyCommandList,
            disabled: commands.length === 0,
            tone: copiedCommands ? "good" : "neutral",
          },
          {
            label: "Copy lookup summary",
            icon: Search,
            onClick: copyCommandList,
            disabled: commands.length === 0,
            tone: "neutral",
          },
        ]}
      />

      <div className="mb-4 rounded-lg border border-surface-800 bg-surface-950/30 p-3">
        <div className="flex items-center justify-between gap-3">
          <div>
            <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
              Command Registry
            </p>
            <p className="mt-1 text-sm font-semibold text-surface-100">
              {commands.length} commands loaded
            </p>
          </div>
          <span className="rounded border border-surface-800 bg-surface-900 px-2 py-1 font-mono text-xs text-surface-400">
            {harness.status}
          </span>
        </div>
        <div className="mt-3 flex flex-wrap gap-2">
          {commands.slice(0, 8).map((command) => (
            <span
              key={`${command.skillName ?? command.name}-${command.name}`}
              className="rounded border border-surface-800 bg-surface-900 px-2 py-1 font-mono text-xs text-surface-400"
            >
              /{command.name}
            </span>
          ))}
          {commands.length === 0 && (
            <span className="text-xs text-surface-500">{harness.error || "No command registry loaded."}</span>
          )}
        </div>
      </div>

      <div className="grid gap-3 md:grid-cols-3">
        {COMMAND_SURFACES.map((surface) => (
          <div
            key={surface.name}
            className="rounded-lg border border-surface-800 bg-surface-950/30 p-3"
          >
            <div className="mb-2 flex items-center gap-2">
              <Terminal className="h-4 w-4 text-surface-500" />
              <p className="text-sm font-semibold text-surface-100">{surface.name}</p>
            </div>
            <p className="text-xs leading-relaxed text-surface-500">{surface.detail}</p>
            <p className="mt-3 font-mono text-[11px] uppercase tracking-[0.08em] text-surface-600">
              {surface.owner}
            </p>
          </div>
        ))}
      </div>

      <SettingRow
        label="Command lookup path"
        description="Search should make commands discoverable by canonical name, alias, and workflow intent."
        stack
        scope="Harness"
        risk="low"
      >
        <div className="grid gap-2 md:grid-cols-3">
          <div className="rounded-md border border-surface-800 bg-surface-950/30 p-3">
            <Search className="mb-2 h-4 w-4 text-surface-500" />
            <p className="text-sm font-medium text-surface-100">Intent aliases</p>
            <p className="mt-1 text-xs text-surface-500">chronos, goal, verify, handoff</p>
          </div>
          <div className="rounded-md border border-surface-800 bg-surface-950/30 p-3">
            <Wrench className="mb-2 h-4 w-4 text-surface-500" />
            <p className="text-sm font-medium text-surface-100">Skill backed</p>
            <p className="mt-1 text-xs text-surface-500">Commands can resolve into workflow prompts.</p>
          </div>
          <div className="rounded-md border border-surface-800 bg-surface-950/30 p-3">
            <Keyboard className="mb-2 h-4 w-4 text-surface-500" />
            <p className="text-sm font-medium text-surface-100">Shortcut safe</p>
            <p className="mt-1 text-xs text-surface-500">Keybindings belong in Workspace, not Runtime.</p>
          </div>
        </div>
      </SettingRow>
    </div>
  );
}
