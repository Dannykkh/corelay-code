
import { Check, Clipboard, Database, RefreshCw, Server, Shield, Wrench } from "lucide-react";
import { useMemo, useState } from "react";
import { useSettingsStore } from "@/lib/settings-store";
import { useHarnessStatus, type SkillInfo } from "@/hooks/useHarnessStatus";
import { SectionActionStrip, SectionHeader, SettingRow } from "./SettingRow";
import { cn } from "@/lib/utils";

const SKILL_LANES = [
  {
    label: "Bundled",
    description: "Core workflows shipped with the app and available without workspace setup.",
    count: "stable",
  },
  {
    label: "Project",
    description: "Repository-local instructions that encode team workflow and domain memory.",
    count: "scoped",
  },
  {
    label: "Plugin",
    description: "Admin-approved extension workflows with explicit trust boundaries.",
    count: "trusted",
  },
];

const EMPTY_SKILLS: SkillInfo[] = [];

export function SkillsSettings() {
  const { settings } = useSettingsStore();
  const harness = useHarnessStatus(settings.apiUrl);
  const [copiedSkills, setCopiedSkills] = useState(false);
  const skills = harness.data?.skills ?? EMPTY_SKILLS;
  const registryOnline = harness.status === "success" && Boolean(harness.data);
  const sourceCounts = useMemo(() => {
    return skills.reduce<Record<string, number>>((counts, skill) => {
      counts[skill.source] = (counts[skill.source] ?? 0) + 1;
      return counts;
    }, {});
  }, [skills]);
  const lanes = SKILL_LANES.map((lane) => {
    const source = lane.label.toLowerCase();
    return {
      ...lane,
      count:
        lane.label === "Bundled"
          ? String(skills.length)
          : String(sourceCounts[source] ?? sourceCounts[lane.label.toLowerCase()] ?? 0),
    };
  });
  const sourceCount = Object.keys(sourceCounts).length;
  const skillTone = registryOnline ? "good" : harness.status === "loading" ? "neutral" : "warn";
  const skillTitle = registryOnline
    ? "Skill registry is loaded"
    : harness.status === "loading"
      ? "Checking skill registry"
      : "Connect the runtime before changing skill policy";
  const skillDescription = registryOnline
    ? "Skills are reusable workflow contracts. Keep preload narrow and load deeper references only when a task needs them."
    : harness.error || "The runtime exposes bundled, project, and plugin skill metadata to this panel.";

  async function copySkillSummary() {
    const lines = [
      `Skills: ${skills.length}`,
      `Sources: ${Object.entries(sourceCounts).map(([source, count]) => `${source}:${count}`).join(", ") || "none"}`,
      "Policy: preload only named, allowed skill references",
    ];

    try {
      await navigator.clipboard.writeText(lines.join("\n"));
      setCopiedSkills(true);
      window.setTimeout(() => setCopiedSkills(false), 1600);
    } catch {
      setCopiedSkills(false);
    }
  }

  return (
    <div>
      <SectionHeader
        eyebrow="Harness"
        title="Skills"
        description="Skills are reusable workflow capital. Keep them visible as a harness layer instead of hiding them behind generic integrations."
      >
        <button
          onClick={harness.refresh}
          className="flex h-11 w-11 items-center justify-center rounded border border-surface-800 text-surface-400 transition-colors hover:border-surface-700 hover:text-surface-200 active:translate-y-px"
          title="Refresh skills"
          type="button"
        >
          <RefreshCw className={cn("h-3.5 w-3.5", harness.status === "loading" && "animate-spin")} />
        </button>
      </SectionHeader>

      <SectionActionStrip
        icon={Wrench}
        title={skillTitle}
        description={skillDescription}
        stateLabel={registryOnline ? "ready" : harness.status === "loading" ? "checking" : "offline"}
        stateTone={skillTone}
        metrics={[
          {
            label: "Skills",
            value: `${skills.length}`,
            tone: skills.length > 0 ? "good" : "neutral",
          },
          {
            label: "Sources",
            value: `${sourceCount}`,
            tone: sourceCount > 0 ? "good" : "neutral",
          },
          {
            label: "Preload",
            value: "named",
            tone: "good",
          },
        ]}
        actions={[
          {
            label: "Refresh skills",
            icon: RefreshCw,
            onClick: harness.refresh,
            disabled: harness.status === "loading",
            tone: registryOnline ? "good" : "warn",
          },
          {
            label: copiedSkills ? "Copied skills" : "Copy skill summary",
            icon: copiedSkills ? Check : Clipboard,
            onClick: copySkillSummary,
            disabled: skills.length === 0,
            tone: copiedSkills ? "good" : "neutral",
          },
          {
            label: "Copy preload policy",
            icon: Shield,
            onClick: copySkillSummary,
            tone: "neutral",
          },
        ]}
      />

      <div className="divide-y divide-surface-800 rounded-lg border border-surface-800 bg-surface-950/30">
        {lanes.map((lane) => (
          <div key={lane.label} className="flex items-start justify-between gap-4 p-4">
            <div className="flex gap-3">
              <Wrench className="mt-0.5 h-4 w-4 text-surface-500" />
              <div>
                <p className="text-sm font-medium text-surface-100">{lane.label}</p>
                <p className="mt-1 text-xs leading-relaxed text-surface-500">
                  {lane.description}
                </p>
              </div>
            </div>
            <span className="rounded border border-surface-800 bg-surface-900 px-2 py-1 font-mono text-xs text-surface-400">
              {lane.count}
            </span>
          </div>
        ))}
      </div>

      <div className="mt-4 flex flex-wrap gap-2">
        {Object.entries(sourceCounts).map(([source, count]) => (
          <span
            key={source}
            className="rounded border border-surface-800 bg-surface-900 px-2 py-1 font-mono text-xs text-surface-400"
          >
            {source}: {count}
          </span>
        ))}
        {skills.length === 0 && (
          <span className="text-xs text-surface-500">{harness.error || "No skills loaded."}</span>
        )}
      </div>

      <SettingRow
        label="Skill preload policy"
        description="Agents may preload skills only when the agent definition names them and the source is allowed by policy."
        stack
        scope="Harness"
        risk="medium"
      >
        <div className="grid gap-3 md:grid-cols-3">
          <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
            <Server className="mb-2 h-4 w-4 text-surface-500" />
            <p className="text-sm font-medium text-surface-100">MCP additive</p>
            <p className="mt-1 text-xs leading-relaxed text-surface-500">
              Agent-specific MCP tools merge with the parent tool pool.
            </p>
          </div>
          <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
            <Shield className="mb-2 h-4 w-4 text-surface-500" />
            <p className="text-sm font-medium text-surface-100">Trust gated</p>
            <p className="mt-1 text-xs leading-relaxed text-surface-500">
              Plugin-only policies block user-controlled hooks and MCP where required.
            </p>
          </div>
          <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
            <Database className="mb-2 h-4 w-4 text-surface-500" />
            <p className="text-sm font-medium text-surface-100">Memory aware</p>
            <p className="mt-1 text-xs leading-relaxed text-surface-500">
              Skills should point to compact references, not bulk-load unrelated context.
            </p>
          </div>
        </div>
      </SettingRow>
    </div>
  );
}
