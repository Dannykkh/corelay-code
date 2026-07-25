
import { useState } from "react";
import { BookOpen, Bot, Check, Clipboard, FileCheck2, GitBranch, RefreshCw, Route, ShieldCheck, Users } from "lucide-react";
import { useSettingsStore } from "@/lib/settings-store";
import { useHarnessStatus } from "@/hooks/useHarnessStatus";
import { SectionActionStrip, SectionHeader, SettingRow } from "./SettingRow";
import { cn } from "@/lib/utils";

const AGENT_ROLES = [
  {
    name: "Explore",
    purpose: "Read-only discovery and codebase navigation.",
    boundary: "No edits, fresh file reads, slim context.",
    tone: "neutral",
  },
  {
    name: "Plan",
    purpose: "Break work into implementable slices.",
    boundary: "Read-only, hands output back to the parent.",
    tone: "neutral",
  },
  {
    name: "Implement",
    purpose: "Make scoped code changes.",
    boundary: "Uses approved tools and inherited runtime policy.",
    tone: "warn",
  },
  {
    name: "Review",
    purpose: "Find regressions and missing tests.",
    boundary: "Reports findings before changes land.",
    tone: "good",
  },
  {
    name: "Verify",
    purpose: "Run evidence checks and try to break the result.",
    boundary: "No project writes, command-output based verdict.",
    tone: "good",
  },
];

const CONTRACTS = [
  {
    label: "Context inheritance",
    value: "Explicit",
    detail: "Forked workers receive a bounded copy of the parent context and directive.",
  },
  {
    label: "Tool boundary",
    value: "Role scoped",
    detail: "Agent definitions decide which tools are visible and whether prompts bubble.",
  },
  {
    label: "Transcript trail",
    value: "Sidechain",
    detail: "Worker messages are recorded separately so parent sessions stay readable.",
  },
];

const CORE_AGENT_CARD = {
  name: "aniclew-runtime",
  status: "draft",
  path: "docs/agent-cards/aniclew-runtime.agent.md",
  preload: ["docs/llms.txt", "aniclew-runtime.agent.md"],
  gate: ["docs:check", "proxy-go tests", "targeted eslint"],
};

const CARD_SECTIONS = [
  {
    label: "Role",
    detail: "Owns runtime-plane decisions that must survive model and provider changes.",
    icon: Bot,
  },
  {
    label: "Invariants",
    detail: "Keeps Runtime, Harness, and Proof concepts separate across UI, API, and docs.",
    icon: ShieldCheck,
  },
  {
    label: "JIT knowledge",
    detail: "Preloads the small docs map first, then pulls README, memory, or source only when needed.",
    icon: BookOpen,
  },
  {
    label: "Verification",
    detail: "Requires command evidence or receipts before claiming backend, web, or docs completion.",
    icon: FileCheck2,
  },
];

function toneClass(tone: string) {
  if (tone === "good") return "border-green-500/20 bg-green-500/10 text-green-300";
  if (tone === "warn") return "border-amber-500/20 bg-amber-500/10 text-amber-300";
  return "border-surface-800 bg-surface-950/30 text-surface-400";
}

export function AgentsSettings() {
  const { settings } = useSettingsStore();
  const harness = useHarnessStatus(settings.apiUrl);
  const [copiedCard, setCopiedCard] = useState(false);
  const backendAgents = Object.values(harness.data?.agentTypes ?? {});
  const readOnlyCount = backendAgents.filter((agent) => agent.readOnly).length;
  const writableCount = Math.max(backendAgents.length - readOnlyCount, 0);
  const registryOnline = harness.status === "success" && Boolean(harness.data);
  const agentTone = registryOnline ? "good" : harness.status === "loading" ? "neutral" : "warn";
  const agentTitle = registryOnline
    ? "Agent harness registry is available"
    : harness.status === "loading"
      ? "Checking agent harness registry"
      : "Connect the runtime before editing agent behavior";
  const agentDescription = registryOnline
    ? "Review the Core Agent Card and role boundaries before adding model-specific worker behavior."
    : harness.error || "The local proxy exposes agent types, loops, and workstreams to this panel.";
  const displayedRoles = backendAgents.length
    ? backendAgents.slice(0, 5).map((agent) => ({
        name: agent.name,
        purpose: agent.description,
        boundary: agent.readOnly
          ? "Read-only agent"
          : `${agent.tools?.length ?? 0} scoped tools`,
        tone: agent.readOnly ? "good" : "warn",
    }))
    : AGENT_ROLES;

  async function copyAgentCardSummary() {
    const summary = [
      `Core agent card: ${CORE_AGENT_CARD.name}`,
      `Path: ${CORE_AGENT_CARD.path}`,
      `Gates: ${CORE_AGENT_CARD.gate.join(", ")}`,
      `Registered agents: ${backendAgents.length || "unknown"}`,
    ].join("\n");

    try {
      await navigator.clipboard.writeText(summary);
      setCopiedCard(true);
      window.setTimeout(() => setCopiedCard(false), 1600);
    } catch {
      setCopiedCard(false);
    }
  }

  return (
    <div>
      <SectionHeader
        eyebrow="Harness"
        title="Agents"
        description="Agent roles sit above model routing. The runtime can change providers while this harness keeps worker behavior stable."
      >
        <button
          onClick={harness.refresh}
          className="flex h-11 w-11 items-center justify-center rounded border border-surface-800 text-surface-400 transition-colors hover:border-surface-700 hover:text-surface-200 active:translate-y-px"
          title="Refresh agent registry"
          type="button"
        >
          <RefreshCw className={cn("h-3.5 w-3.5", harness.status === "loading" && "animate-spin")} />
        </button>
      </SectionHeader>

      <SectionActionStrip
        icon={Bot}
        title={agentTitle}
        description={agentDescription}
        stateLabel={registryOnline ? "ready" : harness.status === "loading" ? "checking" : "offline"}
        stateTone={agentTone}
        metrics={[
          {
            label: "Agents",
            value: backendAgents.length ? `${backendAgents.length}` : "unknown",
            tone: backendAgents.length ? "good" : "neutral",
          },
          {
            label: "Read-only",
            value: `${readOnlyCount}`,
            tone: readOnlyCount > 0 ? "good" : "neutral",
          },
          {
            label: "Writable",
            value: backendAgents.length ? `${writableCount}` : "unknown",
            tone: writableCount > 0 ? "warn" : "good",
          },
        ]}
        actions={[
          {
            label: "Refresh agent registry",
            icon: RefreshCw,
            onClick: harness.refresh,
            disabled: harness.status === "loading",
            tone: registryOnline ? "good" : "warn",
          },
          {
            label: copiedCard ? "Copied agent card" : "Copy agent card summary",
            icon: copiedCard ? Check : Clipboard,
            onClick: copyAgentCardSummary,
            tone: copiedCard ? "good" : "neutral",
          },
          {
            label: "Copy verification gates",
            icon: FileCheck2,
            onClick: copyAgentCardSummary,
            tone: "neutral",
          },
        ]}
      />

      <div className="mb-4 grid gap-3 md:grid-cols-3">
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Registered Agents
          </p>
          <p className="mt-2 text-sm font-semibold text-surface-100">{backendAgents.length || "unknown"}</p>
          <p className="mt-1 text-xs text-surface-500">{harness.error || "Builtin plus project-local definitions"}</p>
        </div>
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Read-only
          </p>
          <p className="mt-2 text-sm font-semibold text-surface-100">{readOnlyCount}</p>
          <p className="mt-1 text-xs text-surface-500">Explorer and review-safe boundaries</p>
        </div>
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-surface-600">
            Active Loops
          </p>
          <p className="mt-2 text-sm font-semibold text-surface-100">{harness.data?.loops.count ?? 0}</p>
          <p className="mt-1 text-xs text-surface-500">
            Max {harness.data?.loops.maxConcurrent ?? 3} concurrent loops
          </p>
        </div>
      </div>

      <div className="grid gap-2 md:grid-cols-5">
        {displayedRoles.map((role) => (
          <div
            key={role.name}
            className={cn("rounded-lg border p-3", toneClass(role.tone))}
          >
            <div className="mb-2 flex items-center gap-2">
              <Bot className="h-3.5 w-3.5" />
              <p className="text-sm font-semibold text-surface-100">{role.name}</p>
            </div>
            <p className="text-xs leading-snug text-surface-400">{role.purpose}</p>
            <p className="mt-2 text-[11px] leading-snug text-surface-500">{role.boundary}</p>
          </div>
        ))}
      </div>

      <SettingRow
        label="Core Agent Card"
        description="Agent cards turn project-specific business rules into a stable harness contract instead of a long prompt."
        stack
        scope="Harness"
        risk="medium"
      >
        <div className="rounded-lg border border-surface-800 bg-surface-950/30">
          <div className="flex flex-col gap-3 border-b border-surface-800 p-4 md:flex-row md:items-start md:justify-between">
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-2">
                <p className="font-mono text-sm font-semibold text-surface-100">{CORE_AGENT_CARD.name}</p>
                <span className="rounded border border-amber-500/20 bg-amber-500/10 px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.12em] text-amber-200">
                  {CORE_AGENT_CARD.status}
                </span>
              </div>
              <p className="mt-1 break-all font-mono text-[11px] text-surface-500">{CORE_AGENT_CARD.path}</p>
            </div>
            <div className="flex flex-wrap gap-1.5">
              {CORE_AGENT_CARD.gate.map((gate) => (
                <span
                  key={gate}
                  className="rounded border border-surface-800 bg-surface-900 px-1.5 py-0.5 font-mono text-[11px] text-surface-400"
                >
                  {gate}
                </span>
              ))}
            </div>
          </div>
          <div className="grid gap-px bg-surface-800 md:grid-cols-4">
            {CARD_SECTIONS.map(({ label, detail, icon: Icon }) => (
              <div key={label} className="bg-surface-950 p-3">
                <div className="mb-2 flex items-center gap-2">
                  <Icon className="h-3.5 w-3.5 text-surface-500" />
                  <p className="text-sm font-medium text-surface-100">{label}</p>
                </div>
                <p className="text-xs leading-relaxed text-surface-500">{detail}</p>
              </div>
            ))}
          </div>
          <div className="flex flex-col gap-2 p-4 md:flex-row md:items-center md:justify-between">
            <div className="flex min-w-0 items-start gap-2">
              <Route className="mt-0.5 h-3.5 w-3.5 flex-shrink-0 text-surface-500" />
              <p className="text-xs leading-relaxed text-surface-500">
                Preload stays small: {CORE_AGENT_CARD.preload.join(" -> ")}. Full docs are loaded just in time.
              </p>
            </div>
            <span className="flex-shrink-0 rounded border border-surface-800 px-2 py-1 font-mono text-[11px] text-surface-500">
              docs/agent-cards
            </span>
          </div>
        </div>
      </SettingRow>

      <div className="mt-5 divide-y divide-surface-800 rounded-lg border border-surface-800 bg-surface-950/30">
        {CONTRACTS.map((item) => (
          <div key={item.label} className="flex items-start justify-between gap-4 p-4">
            <div>
              <p className="text-sm font-medium text-surface-100">{item.label}</p>
              <p className="mt-1 text-xs leading-relaxed text-surface-500">{item.detail}</p>
            </div>
            <span className="rounded border border-surface-800 bg-surface-900 px-2 py-1 font-mono text-xs text-surface-400">
              {item.value}
            </span>
          </div>
        ))}
      </div>

      <SettingRow
        label="Forked worker boundary"
        description="Forked subagents are valuable only when context transfer, prompt-cache locality, and recursion guards remain explicit."
        stack
        scope="Harness"
        risk="medium"
      >
        <div className="grid gap-3 md:grid-cols-3">
          <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
            <Users className="mb-2 h-4 w-4 text-surface-500" />
            <p className="text-sm font-medium text-surface-100">Parent controlled</p>
            <p className="mt-1 text-xs leading-relaxed text-surface-500">
              The parent owns the objective, worker directive, and final merge decision.
            </p>
          </div>
          <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
            <GitBranch className="mb-2 h-4 w-4 text-surface-500" />
            <p className="text-sm font-medium text-surface-100">Worktree aware</p>
            <p className="mt-1 text-xs leading-relaxed text-surface-500">
              Isolated workers must translate inherited paths and re-read files before edits.
            </p>
          </div>
          <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
            <ShieldCheck className="mb-2 h-4 w-4 text-surface-500" />
            <p className="text-sm font-medium text-surface-100">No recursive fork</p>
            <p className="mt-1 text-xs leading-relaxed text-surface-500">
              Fork children execute directly instead of spawning more background workers.
            </p>
          </div>
        </div>
      </SettingRow>
    </div>
  );
}
