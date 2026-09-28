import { useEffect, useState, useMemo } from 'react';
import {
  ShieldAlert,
  Check,
  X,
  Play,
  Plus,
  Trash2,
  ChevronDown,
  ChevronRight,
  CircleCheck,
  CircleX,
  LoaderCircle,
  Clock,
  Layers,
  MinusCircle,
  Sliders,
  Terminal,
} from 'lucide-react';
import { fetchJSON, HTTPError, resolveApproval, type ApprovalDecision, type ProviderInfo } from '../lib/api';
import { listWorkstreams, type Workstream } from '../lib/workstreams';
import { streamSSE } from '../lib/sse';
import { WorkspaceModeSwitch, type WorkspaceMode } from '../components/WorkspaceModeSwitch';
import { loadPreferredExecutionMode } from '../lib/executionMode';

type TeamTaskDraft = {
  /** Stable React key; the editable `id` must not be used as a key. */
  uid: string;
  id: string;
  name: string;
  description: string;
  kind: string;
  role: string;
  provider: string;
  model: string;
  readOnly: boolean;
  files: string;
  dependsOn: string;
  modelSlots: string;
  toolSlots: string;
  webFetchSlots: string;
  testSlots: string;
};

type TeamCapacityDraft = {
  modelSlots: string;
  toolSlots: string;
  webFetchSlots: string;
  testSlots: string;
  maxParallelTasks: string;
};

type ActiveTeamApproval = {
  id: string;
  sessionId: string;
  toolName: string;
  redactedInput: string;
  dangerLevel?: string;
  expiresAt?: string;
  state: 'pending' | 'resolving';
  error?: string;
};

type TaskStatus = 'pending' | 'running' | 'completed' | 'failed' | 'not-run';

const taskKinds = ['research', 'propose', 'blueprint', 'implement', 'review', 'verify'];
const taskRoles = ['researcher', 'planner', 'architect', 'implementer', 'reviewer', 'verifier'];

const fieldClass = 'bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg px-2.5 py-1.5 text-xs text-[var(--color-text)] focus:outline-none focus:border-[var(--color-accent)]';
const labelClass = 'block text-[10px] uppercase font-semibold text-[var(--color-text2)] mb-1';

let draftUidCounter = 0;

function newTaskDraft(index: number): TeamTaskDraft {
  draftUidCounter += 1;
  return {
    uid: `draft-${draftUidCounter}`,
    id: `task-${index}`,
    name: '',
    description: '',
    kind: 'implement',
    role: 'implementer',
    provider: '',
    model: '',
    readOnly: false,
    files: '**',
    dependsOn: '',
    modelSlots: '',
    toolSlots: '',
    webFetchSlots: '',
    testSlots: '',
  };
}

function splitList(value: string): string[] {
  return value.split(',').map((item) => item.trim()).filter(Boolean);
}

function positiveInt(value: string): number | undefined {
  const parsed = Number.parseInt(value, 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : undefined;
}

function stringifyEventData(data: unknown): string {
  if (typeof data === 'string') return data;
  if (data == null) return '';
  return JSON.stringify(data);
}

async function readErrorMessage(res: Response): Promise<string> {
  try {
    const body = await res.json();
    return body?.error?.message || body?.message || `HTTP ${res.status}`;
  } catch {
    try {
      const text = await res.text();
      return text || `HTTP ${res.status}`;
    } catch {
      return `HTTP ${res.status}`;
    }
  }
}

export function TeamPage({ selectedWorkspace, onApprovalPendingChange, onWorkspaceModeChange, workspaceModeAttention }: {
  selectedWorkspace?: string;
  onApprovalPendingChange?: (pending: boolean) => void;
  onWorkspaceModeChange?: (mode: WorkspaceMode) => void;
  workspaceModeAttention?: Partial<Record<WorkspaceMode, boolean>>;
}) {
  const [providers, setProviders] = useState<ProviderInfo[]>([]);
  const [workstreams, setWorkstreams] = useState<Workstream[]>([]);
  const [teamName, setTeamName] = useState('web-team');
  const [objective, setObjective] = useState('');
  const [verifyCommand, setVerifyCommand] = useState('');
  const [selectedWorkstreamId, setSelectedWorkstreamId] = useState('');
  const [showAdvancedCapacity, setShowAdvancedCapacity] = useState(false);
  const [capacity, setCapacity] = useState<TeamCapacityDraft>({
    modelSlots: '1',
    toolSlots: '',
    webFetchSlots: '',
    testSlots: '',
    maxParallelTasks: '2',
  });
  const [teamTasks, setTeamTasks] = useState<TeamTaskDraft[]>([newTaskDraft(1)]);
  const [teamRunning, setTeamRunning] = useState(false);
  const [teamLog, setTeamLog] = useState<string[]>([]);
  const [taskStatuses, setTaskStatuses] = useState<Record<string, TaskStatus>>({});
  const [activeApprovals, setActiveApprovals] = useState<ActiveTeamApproval[]>([]);
  const [notRunReason, setNotRunReason] = useState('');

  useEffect(() => {
    onApprovalPendingChange?.(activeApprovals.length > 0);
  }, [activeApprovals.length, onApprovalPendingChange]);

  // The broker denies an unanswered request at expiresAt; drop the card then so
  // it cannot be "retried" against a request that no longer exists.
  useEffect(() => {
    const deadlines = activeApprovals
      .map((a) => (a.expiresAt ? Date.parse(a.expiresAt) : Number.NaN))
      .filter((t) => Number.isFinite(t));
    if (deadlines.length === 0) return;
    const delay = Math.max(0, Math.min(...deadlines) - Date.now());
    const timer = window.setTimeout(() => {
      const now = Date.now();
      const expired = activeApprovals.filter((a) => a.expiresAt && Date.parse(a.expiresAt) <= now);
      if (expired.length === 0) return;
      const expiredIds = new Set(expired.map((a) => a.id));
      setTeamLog((log) => [...log, ...expired.map((a) => `[Approval Expired] ${a.toolName}: nothing was allowed`)]);
      setActiveApprovals((prev) => prev.filter((a) => !expiredIds.has(a.id)));
    }, delay);
    return () => window.clearTimeout(timer);
  }, [activeApprovals]);

  async function handleResolveApproval(approvalId: string, decision: ApprovalDecision) {
    const current = activeApprovals.find((a) => a.id === approvalId);
    if (!current || current.state === 'resolving') return;
    setActiveApprovals((prev) => prev.map((a) => (a.id === approvalId ? { ...a, state: 'resolving', error: undefined } : a)));
    try {
      await resolveApproval(current.id, current.sessionId, decision);
      setTeamLog((prev) => [...prev, `[Approval] ${decision === 'allow_once' ? 'Allowed' : 'Denied'} ${current.toolName}`]);
      setActiveApprovals((prev) => prev.filter((a) => a.id !== approvalId));
    } catch (err) {
      const noLongerActionable = err instanceof HTTPError && (err.status === 404 || err.status === 409);
      if (noLongerActionable) {
        setTeamLog((prev) => [...prev, `[Approval] ${current.toolName}: request is no longer actionable; nothing was allowed`]);
        setActiveApprovals((prev) => prev.filter((a) => a.id !== approvalId));
        return;
      }
      const message = err instanceof Error ? err.message : String(err);
      setTeamLog((prev) => [...prev, `[Approval Error] ${message}`]);
      setActiveApprovals((prev) => prev.map((a) => (
        a.id === approvalId ? { ...a, state: 'pending', error: `Could not record the decision (${message}). Nothing was allowed; retry or deny.` } : a
      )));
    }
  }

  useEffect(() => {
    void (async () => {
      const [p, w] = await Promise.all([
        fetchJSON<ProviderInfo[]>('/api/providers').catch(() => []),
        listWorkstreams().catch(() => []),
      ]);
      setProviders(Array.isArray(p) ? p : []);
      setWorkstreams(Array.isArray(w) ? w : []);
    })();
  }, []);

  function nextUniqueTaskId(existing: TeamTaskDraft[]): string {
    const existingIds = new Set(existing.map((t) => t.id));
    let n = existing.length + 1;
    while (existingIds.has(`task-${n}`)) {
      n++;
    }
    return `task-${n}`;
  }

  function addTaskRow() {
    setTeamTasks((prev) => {
      const id = nextUniqueTaskId(prev);
      return [...prev, { ...newTaskDraft(prev.length + 1), id }];
    });
  }

  function removeTaskRow(index: number) {
    setTeamTasks((prev) => prev.filter((_, i) => i !== index));
  }

  function updateTask<K extends keyof TeamTaskDraft>(index: number, key: K, value: TeamTaskDraft[K]) {
    setTeamTasks((prev) => prev.map((task, i) => (i === index ? { ...task, [key]: value } : task)));
  }

  function updateTaskProvider(index: number, provider: string) {
    setTeamTasks((prev) => prev.map((task, i) => (i === index ? { ...task, provider, model: '' } : task)));
  }

  function updateCapacity<K extends keyof TeamCapacityDraft>(key: K, value: TeamCapacityDraft[K]) {
    setCapacity((prev) => ({ ...prev, [key]: value }));
  }

  function providerModels(provider: string) {
    return providers.find((p) => p.name === provider)?.models || [];
  }

  async function runTeam() {
    const incomplete = teamTasks.filter((task) => !task.name.trim() || !task.description.trim());
    if (incomplete.length > 0) {
      setTeamLog((prev) => [...prev, `Error: every task needs a name and description (${incomplete.map((t) => t.id || '?').join(', ')}).`]);
      return;
    }
    const validTasks = teamTasks;
    const taskIds = new Set<string>();
    for (const t of validTasks) {
      const id = t.id.trim();
      if (!id) {
        setTeamLog((prev) => [...prev, 'Error: All tasks must have a non-empty ID.']);
        return;
      }
      // Progress is read from server messages like "Worker w: <id> — completed"
      // and "Wave 1/2: 2 tasks (<id>, <id>)", so IDs must not contain separators.
      if (!/^[A-Za-z0-9._-]+$/.test(id)) {
        setTeamLog((prev) => [...prev, `Error: Task ID '${id}' may only use letters, digits, '.', '_' and '-'.`]);
        return;
      }
      if (taskIds.has(id)) {
        setTeamLog((prev) => [...prev, `Error: Duplicate task ID '${id}'. IDs must be unique.`]);
        return;
      }
      taskIds.add(id);
    }

    setTeamRunning(true);
    setTeamLog([]);
    setActiveApprovals([]);
    setNotRunReason('');
    let failedTaskId: string | null = null;
    let runError = false;

    const initialStatuses: Record<string, TaskStatus> = {};
    for (const t of validTasks) {
      initialStatuses[t.id.trim()] = 'pending';
    }
    setTaskStatuses(initialStatuses);

    const tasks = validTasks.map((task) => {
      const files = splitList(task.files);
      return {
        id: task.id.trim(),
        name: task.name.trim(),
        description: task.description.trim(),
        kind: task.kind,
        role: task.role,
        provider: task.provider.trim(),
        model: task.model.trim(),
        readOnly: Boolean(task.readOnly),
        resources: {
          modelSlots: positiveInt(task.modelSlots),
          toolSlots: positiveInt(task.toolSlots),
          webFetchSlots: positiveInt(task.webFetchSlots),
          testSlots: positiveInt(task.testSlots),
        },
        files: files.length > 0 ? files : ['**'],
        dependsOn: splitList(task.dependsOn),
      };
    });

    const preferredMode = loadPreferredExecutionMode();
    try {
      const res = await fetch('/api/team', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: teamName.trim() || 'web-team',
          objective: objective.trim() || teamName.trim() || 'web-team',
          verifyCommand: verifyCommand.trim(),
          workstreamId: selectedWorkstreamId || undefined,
          // Same project and execution-mode choice as the chat workspace; the
          // server validates both (registered workspace, known mode).
          workDir: selectedWorkspace || undefined,
          executionPolicy: preferredMode ? { mode: preferredMode } : undefined,
          capacity: {
            modelSlots: positiveInt(capacity.modelSlots),
            toolSlots: positiveInt(capacity.toolSlots),
            webFetchSlots: positiveInt(capacity.webFetchSlots),
            testSlots: positiveInt(capacity.testSlots),
            maxParallelTasks: positiveInt(capacity.maxParallelTasks),
            fileScopeLock: true,
          },
          tasks,
        }),
      });

      if (!res.ok) {
        throw new Error(await readErrorMessage(res));
      }
      if (!res.body) {
        throw new Error('Empty team stream');
      }

      for await (const frame of streamSSE(res)) {
        try {
          const event = JSON.parse(frame.data) as { type?: string; data?: unknown };
          if (event.type === 'status' || event.type === 'text' || event.type === 'error') {
            const msg = stringifyEventData(event.data);
            if (msg) {
              setTeamLog((prev) => [...prev, msg]);
              if (event.type === 'error') runError = true;
              // Server formats (internal/agent/team.go):
              //   "Worker worker-x: task-1 — completed (3 tools, 1.2s)"
              //   "Wave 1/2: 2 tasks (task-1, task-2)"
              //   "Wave 1 batch 1: 1/2 tasks (task-1)" (only when capacity splits a wave)
              const workerMatch = msg.match(/Worker\s+[^:]+:\s*([^\s—]+)\s*—\s*(completed|failed)/i);
              if (workerMatch) {
                const taskId = workerMatch[1];
                const status = workerMatch[2].toLowerCase() as TaskStatus;
                if (status === 'failed' && !failedTaskId) failedTaskId = taskId;
                setTaskStatuses((prev) => ({ ...prev, [taskId]: status }));
              }
              const waveMatch = msg.match(/^Wave\s+\d+(?:\/\d+|\s+batch\s+\d+):[^(]*\(([^)]+)\)/i);
              if (waveMatch) {
                const ids = waveMatch[1].split(',').map((s) => s.trim()).filter(Boolean);
                setTaskStatuses((prev) => {
                  const next = { ...prev };
                  for (const id of ids) {
                    if (next[id] !== 'completed' && next[id] !== 'failed') {
                      next[id] = 'running';
                    }
                  }
                  return next;
                });
              }
            }
          } else if (event.type === 'session') {
            const data = event.data as { traceId?: string };
            if (data.traceId) setTeamLog((prev) => [...prev, `Trace: ${data.traceId}`]);
          } else if (event.type === 'workstream') {
            const data = event.data as { title?: string; id?: string };
            const label = data.title || data.id;
            if (label) setTeamLog((prev) => [...prev, `Workstream: ${label}`]);
          } else if (event.type === 'approval_required') {
            const data = event.data as Record<string, unknown>;
            if (typeof data?.id === 'string' && typeof data?.sessionId === 'string') {
              const rawInput = data.redactedInput;
              const redactedInput = typeof rawInput === 'string' ? rawInput : JSON.stringify(rawInput ?? {}, null, 2);
              const newApproval: ActiveTeamApproval = {
                id: data.id,
                sessionId: data.sessionId,
                toolName: String(data.toolName || 'Unknown'),
                redactedInput,
                dangerLevel: typeof data.dangerLevel === 'string' ? data.dangerLevel : undefined,
                expiresAt: typeof data.expiresAt === 'string' ? data.expiresAt : undefined,
                state: 'pending',
              };
              setActiveApprovals((prev) => {
                const filtered = prev.filter((a) => a.id !== newApproval.id);
                return [...filtered, newApproval];
              });
              setTeamLog((prev) => [...prev, `[Approval Required] ${String(data.toolName || 'tool')}`]);
            }
          }
        } catch {
          setTeamLog((prev) => [...prev, frame.data]);
        }
      }
    } catch (err) {
      runError = true;
      setTeamLog((prev) => [...prev, `Error: ${err instanceof Error ? err.message : String(err)}`]);
    } finally {
      setTeamRunning(false);
      setActiveApprovals([]);
      // A failed task aborts the remaining waves; say so instead of leaving
      // those tasks looking "pending" forever.
      if (failedTaskId || runError) {
        setNotRunReason(failedTaskId ? `${failedTaskId} failed` : 'the run ended with an error');
        setTaskStatuses((prev) => {
          const next = { ...prev };
          for (const [id, status] of Object.entries(next)) {
            if (status === 'pending' || status === 'running') next[id] = 'not-run';
          }
          return next;
        });
      }
    }
  }

  // Calculate dependency graph waves for visualization
  const taskWaves = useMemo(() => {
    const waves: TeamTaskDraft[][] = [];
    const resolvedIds = new Set<string>();
    let remaining = [...teamTasks];
    let iteration = 0;

    while (remaining.length > 0 && iteration < 10) {
      iteration++;
      const currentWave: TeamTaskDraft[] = [];
      const nextRemaining: TeamTaskDraft[] = [];

      for (const t of remaining) {
        const deps = splitList(t.dependsOn);
        if (deps.length === 0 || deps.every((d) => resolvedIds.has(d))) {
          currentWave.push(t);
        } else {
          nextRemaining.push(t);
        }
      }

      if (currentWave.length === 0) {
        waves.push(remaining);
        break;
      }

      waves.push(currentWave);
      for (const t of currentWave) {
        resolvedIds.add(t.id);
      }
      remaining = nextRemaining;
    }

    return waves;
  }, [teamTasks]);

  return (
    <div className="p-6 w-full max-w-6xl mx-auto overflow-y-auto h-full space-y-6">
      {/* Top Header & Team Objective */}
      <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-5 space-y-4">
        <div className="flex flex-wrap items-center justify-between gap-4">
          <div>
            {onWorkspaceModeChange && (
              <div className="mb-2">
                <WorkspaceModeSwitch mode="team" onChange={onWorkspaceModeChange} attention={workspaceModeAttention} />
              </div>
            )}
            <h1 className="text-lg font-semibold text-[var(--color-text)] flex items-center gap-2">
              <Layers aria-hidden="true" className="w-5 h-5 text-[var(--color-accent)]" />
              <span>Team Plan Editor & Wave Execution</span>
            </h1>
            <p className="text-xs text-[var(--color-text2)] mt-0.5">
              Tasks run in dependency waves with file-scope locks. Tool calls that need approval appear below the plan.
            </p>
          </div>

          <button
            type="button"
            onClick={runTeam}
            disabled={teamRunning || teamTasks.length === 0}
            className="flex items-center gap-1.5 px-4 py-2 bg-[var(--color-accent)] hover:opacity-90 disabled:opacity-50 text-[var(--color-bg)] rounded-lg text-xs font-semibold shadow-sm transition-all"
          >
            {teamRunning ? (
              <>
                <LoaderCircle aria-hidden="true" className="w-4 h-4 animate-spin" />
                <span>Executing Plan…</span>
              </>
            ) : (
              <>
                <Play className="w-4 h-4 fill-current" />
                <span>Run Team Plan</span>
              </>
            )}
          </button>
        </div>

        <div className="grid grid-cols-1 md:grid-cols-4 gap-3">
          <label>
            <span className={labelClass}>Team Name</span>
            <input
              value={teamName}
              onChange={(e) => setTeamName(e.target.value)}
              className={`${fieldClass} w-full`}
            />
          </label>

          <label>
            <span className={labelClass}>Workstream</span>
            <select
              value={selectedWorkstreamId}
              onChange={(e) => setSelectedWorkstreamId(e.target.value)}
              className={`${fieldClass} w-full`}
            >
              <option value="">No workstream</option>
              {workstreams.map((ws) => (
                <option key={ws.id} value={ws.id}>
                  {ws.title}
                </option>
              ))}
            </select>
          </label>

          <label className="md:col-span-2">
            <span className={labelClass}>Team Objective</span>
            <input
              value={objective}
              onChange={(e) => setObjective(e.target.value)}
              placeholder="e.g. Implement user authentication with OAuth2"
              className={`${fieldClass} w-full`}
            />
          </label>

          <label className="md:col-span-4">
            <span className={labelClass}>Verification Command (Verification Gate)</span>
            <input
              value={verifyCommand}
              onChange={(e) => setVerifyCommand(e.target.value)}
              placeholder="e.g. go test ./... -count=1 or npm test"
              className={`${fieldClass} w-full font-mono`}
            />
          </label>
        </div>
      </div>

      {/* Plan Visual Sequence (Wave Dependency Graph) */}
      <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4">
        <div className="flex items-center justify-between mb-3">
          <div className="text-xs font-semibold text-[var(--color-text)] flex items-center gap-1.5">
            <Layers className="w-3.5 h-3.5 text-[var(--color-accent)]" />
            <span>Execution Waves & Dependencies</span>
          </div>
          <span className="text-[11px] text-[var(--color-text2)]">
            {taskWaves.length} wave{taskWaves.length === 1 ? '' : 's'} calculated
          </span>
        </div>

        <div className="flex flex-wrap items-center gap-3">
          {taskWaves.map((wave, wIdx) => (
            <div key={wIdx} className="flex items-center gap-2">
              <div className="p-2.5 rounded-lg border border-[var(--color-border)] bg-[var(--color-surface2)]/50 min-w-[150px] space-y-1.5">
                <div className="text-[10px] font-semibold text-[var(--color-accent)] uppercase tracking-wide">
                  Wave {wIdx + 1}
                </div>
                <div className="space-y-1">
                  {wave.map((t) => {
                    const status = taskStatuses[t.id.trim()] || 'pending';
                    return (
                      <div
                        key={t.uid}
                        title={status === 'not-run' && notRunReason ? `Not run: ${notRunReason}` : status}
                        className="px-2 py-1 rounded bg-[var(--color-bg)] border border-[var(--color-border)] text-xs flex items-center justify-between gap-2"
                      >
                        <span className="font-medium truncate">{t.name || t.id}</span>
                        {status === 'running' ? (
                          <LoaderCircle aria-label="running" className="w-3 h-3 text-[var(--color-accent)] animate-spin shrink-0" />
                        ) : status === 'completed' ? (
                          <CircleCheck aria-label="completed" className="w-3 h-3 text-[var(--color-green)] shrink-0" />
                        ) : status === 'failed' ? (
                          <CircleX aria-label="failed" className="w-3 h-3 text-[var(--color-red)] shrink-0" />
                        ) : status === 'not-run' ? (
                          <MinusCircle aria-label="not run" className="w-3 h-3 text-[var(--color-text2)] shrink-0" />
                        ) : (
                          <Clock aria-label="pending" className="w-3 h-3 text-[var(--color-text2)] shrink-0" />
                        )}
                      </div>
                    );
                  })}
                </div>
              </div>

              {wIdx < taskWaves.length - 1 && (
                <span className="text-[var(--color-text2)] font-mono">→</span>
              )}
            </div>
          ))}
        </div>
      </div>

      {/* Task Plan Editor Cards */}
      <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4 space-y-4">
        <div className="flex items-center justify-between">
          <div className="text-xs font-semibold text-[var(--color-text)]">
            Task Specifications ({teamTasks.length})
          </div>
          <button
            type="button"
            onClick={addTaskRow}
            disabled={teamRunning}
            className="flex items-center gap-1 text-xs px-2.5 py-1 bg-[var(--color-surface2)] hover:bg-[var(--color-border)] text-[var(--color-text)] rounded font-medium transition-colors disabled:opacity-40"
          >
            <Plus aria-hidden="true" className="w-3.5 h-3.5" />
            Add Task
          </button>
        </div>

        {/* The running plan is fixed; edits apply to the next run. */}
        <fieldset disabled={teamRunning} className="space-y-3 min-w-0 border-0 p-0 m-0">
          {teamTasks.map((task, i) => {
            const models = providerModels(task.provider);
            const status = taskStatuses[task.id.trim()] || 'pending';
            return (
              <div
                key={task.uid}
                className="p-3.5 rounded-lg border border-[var(--color-border)] bg-[var(--color-bg)] space-y-3"
              >
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <div className="flex flex-wrap items-center gap-2">
                    <label className="flex items-center gap-1">
                      <span className="text-[10px] text-[var(--color-text2)] uppercase font-semibold">ID:</span>
                      <input
                        value={task.id}
                        onChange={(e) => updateTask(i, 'id', e.target.value)}
                        placeholder="task-1"
                        className={`${fieldClass} font-mono w-24 py-0.5 text-xs`}
                      />
                    </label>

                    <label className="flex items-center gap-1.5 cursor-pointer text-xs text-[var(--color-text)]">
                      <input
                        type="checkbox"
                        checked={Boolean(task.readOnly)}
                        onChange={(e) => updateTask(i, 'readOnly', e.target.checked)}
                        className="rounded border-[var(--color-border)] text-[var(--color-accent)] focus:ring-0"
                      />
                      <span>Read-only</span>
                    </label>

                    <span className="text-xs text-[var(--color-text2)]">Role:</span>
                    <select
                      value={task.role}
                      onChange={(e) => updateTask(i, 'role', e.target.value)}
                      className={fieldClass}
                    >
                      {taskRoles.map((r) => (
                        <option key={r} value={r}>
                          {r}
                        </option>
                      ))}
                    </select>

                    <span className="text-xs text-[var(--color-text2)]">Kind:</span>
                    <select
                      value={task.kind}
                      onChange={(e) => updateTask(i, 'kind', e.target.value)}
                      className={fieldClass}
                    >
                      {taskKinds.map((k) => (
                        <option key={k} value={k}>
                          {k}
                        </option>
                      ))}
                    </select>

                    <span
                      className={`text-[10px] px-2 py-0.5 rounded font-medium ${
                        status === 'running'
                          ? 'bg-[var(--color-accent)]/15 text-[var(--color-accent)]'
                          : status === 'completed'
                          ? 'bg-[var(--color-green)]/15 text-[var(--color-green)]'
                          : status === 'failed'
                          ? 'bg-[var(--color-red)]/15 text-[var(--color-red)]'
                          : 'bg-[var(--color-surface2)] text-[var(--color-text2)]'
                      }`}
                    >
                      {status === 'not-run' ? `not run${notRunReason ? `: ${notRunReason}` : ''}` : status}
                    </span>
                  </div>

                  <button
                    type="button"
                    onClick={() => removeTaskRow(i)}
                    disabled={teamTasks.length <= 1}
                    className="text-xs text-[var(--color-red)] hover:opacity-80 disabled:opacity-30 p-1"
                    title="Remove Task"
                  >
                    <Trash2 className="w-3.5 h-3.5" />
                  </button>
                </div>

                <div className="grid grid-cols-1 md:grid-cols-3 gap-3">
                  <label>
                    <span className={labelClass}>Task Name</span>
                    <input
                      value={task.name}
                      onChange={(e) => updateTask(i, 'name', e.target.value)}
                      placeholder="e.g. Design auth endpoints"
                      className={`${fieldClass} w-full`}
                    />
                  </label>

                  <label className="md:col-span-2">
                    <span className={labelClass}>Description</span>
                    <input
                      value={task.description}
                      onChange={(e) => updateTask(i, 'description', e.target.value)}
                      placeholder="Detailed instructions for this worker"
                      className={`${fieldClass} w-full`}
                    />
                  </label>
                </div>

                <div className="grid grid-cols-1 sm:grid-cols-4 gap-3 pt-1">
                  <label>
                    <span className={labelClass}>Provider</span>
                    <select
                      value={task.provider}
                      onChange={(e) => updateTaskProvider(i, e.target.value)}
                      className={`${fieldClass} w-full`}
                    >
                      <option value="">Default provider</option>
                      {providers.map((p) => (
                        <option key={p.name} value={p.name}>
                          {p.name}
                        </option>
                      ))}
                    </select>
                  </label>

                  <label>
                    <span className={labelClass}>Model</span>
                    <input
                      list={`team-models-${i}`}
                      value={task.model}
                      onChange={(e) => updateTask(i, 'model', e.target.value)}
                      placeholder="Default or custom model"
                      className={`${fieldClass} w-full`}
                    />
                    <datalist id={`team-models-${i}`}>
                      {models.map((m) => (
                        <option key={m.id} value={m.id}>
                          {m.displayName || m.id}
                        </option>
                      ))}
                    </datalist>
                  </label>

                  <label>
                    <span className={labelClass}>File Scope</span>
                    <input
                      value={task.files}
                      onChange={(e) => updateTask(i, 'files', e.target.value)}
                      placeholder="internal/**, src/**"
                      className={`${fieldClass} w-full font-mono`}
                    />
                  </label>

                  <label>
                    <span className={labelClass}>Depends On</span>
                    <input
                      value={task.dependsOn}
                      onChange={(e) => updateTask(i, 'dependsOn', e.target.value)}
                      placeholder="e.g. task-1"
                      className={`${fieldClass} w-full font-mono`}
                    />
                  </label>
                </div>

                {/* Per-task Resource Slots */}
                <div className="flex flex-wrap items-center gap-3 pt-2 border-t border-[var(--color-border)]/50 text-xs">
                  <span className="text-[10px] uppercase font-semibold text-[var(--color-text2)]">Resources:</span>
                  <label className="flex items-center gap-1">
                    <span className="text-[10px] text-[var(--color-text2)]">Model slots:</span>
                    <input
                      type="number"
                      min="1"
                      value={task.modelSlots}
                      onChange={(e) => updateTask(i, 'modelSlots', e.target.value)}
                      placeholder="1"
                      className={`${fieldClass} w-16 py-0.5`}
                    />
                  </label>
                  <label className="flex items-center gap-1">
                    <span className="text-[10px] text-[var(--color-text2)]">Tool slots:</span>
                    <input
                      type="number"
                      min="1"
                      value={task.toolSlots}
                      onChange={(e) => updateTask(i, 'toolSlots', e.target.value)}
                      placeholder="1"
                      className={`${fieldClass} w-16 py-0.5`}
                    />
                  </label>
                  <label className="flex items-center gap-1">
                    <span className="text-[10px] text-[var(--color-text2)]">Web slots:</span>
                    <input
                      type="number"
                      min="0"
                      value={task.webFetchSlots}
                      onChange={(e) => updateTask(i, 'webFetchSlots', e.target.value)}
                      placeholder="0"
                      className={`${fieldClass} w-16 py-0.5`}
                    />
                  </label>
                  <label className="flex items-center gap-1">
                    <span className="text-[10px] text-[var(--color-text2)]">Test slots:</span>
                    <input
                      type="number"
                      min="0"
                      value={task.testSlots}
                      onChange={(e) => updateTask(i, 'testSlots', e.target.value)}
                      placeholder="0"
                      className={`${fieldClass} w-16 py-0.5`}
                    />
                  </label>
                </div>
              </div>
            );
          })}
        </fieldset>
      </div>

      {/* Collapsible Advanced Slot & Capacity Limits */}
      <div className="border border-[var(--color-border)] rounded-xl bg-[var(--color-surface)] overflow-hidden">
        <button
          type="button"
          onClick={() => setShowAdvancedCapacity(!showAdvancedCapacity)}
          aria-expanded={showAdvancedCapacity}
          className="w-full px-4 py-3 flex items-center justify-between text-xs font-semibold text-[var(--color-text)] hover:bg-[var(--color-surface2)]/50 transition-colors"
        >
          <div className="flex items-center gap-2">
            <Sliders className="w-3.5 h-3.5 text-[var(--color-accent)]" />
            <span>Capacity & Resource Slot Limits (Advanced)</span>
          </div>
          {showAdvancedCapacity ? <ChevronDown className="w-4 h-4" /> : <ChevronRight className="w-4 h-4" />}
        </button>

        {showAdvancedCapacity && (
          <div className="p-4 border-t border-[var(--color-border)] grid grid-cols-2 sm:grid-cols-5 gap-3 bg-[var(--color-bg)]">
            <label>
              <span className={labelClass}>Model slots</span>
              <input
                type="number"
                min="1"
                value={capacity.modelSlots}
                onChange={(e) => updateCapacity('modelSlots', e.target.value)}
                className={`${fieldClass} w-full`}
              />
            </label>
            <label>
              <span className={labelClass}>Tool slots</span>
              <input
                type="number"
                min="1"
                value={capacity.toolSlots}
                onChange={(e) => updateCapacity('toolSlots', e.target.value)}
                placeholder="default"
                className={`${fieldClass} w-full`}
              />
            </label>
            <label>
              <span className={labelClass}>Web fetch slots</span>
              <input
                type="number"
                min="1"
                value={capacity.webFetchSlots}
                onChange={(e) => updateCapacity('webFetchSlots', e.target.value)}
                placeholder="default"
                className={`${fieldClass} w-full`}
              />
            </label>
            <label>
              <span className={labelClass}>Test slots</span>
              <input
                type="number"
                min="1"
                value={capacity.testSlots}
                onChange={(e) => updateCapacity('testSlots', e.target.value)}
                placeholder="default"
                className={`${fieldClass} w-full`}
              />
            </label>
            <label>
              <span className={labelClass}>Max parallel</span>
              <input
                type="number"
                min="1"
                value={capacity.maxParallelTasks}
                onChange={(e) => updateCapacity('maxParallelTasks', e.target.value)}
                className={`${fieldClass} w-full`}
              />
            </label>
          </div>
        )}
      </div>

      {/* Operator Approval Cards (Embedded) */}
      {activeApprovals.length > 0 && (
        <ul aria-label="Pending approvals" className="space-y-3">
          {activeApprovals.map((approval) => (
            <li
              key={approval.id}
              role="alert"
              aria-busy={approval.state === 'resolving'}
              className="p-4 bg-[var(--color-surface2)] border border-[var(--color-warning)]/40 rounded-xl space-y-2.5"
            >
              <div className="flex items-center justify-between gap-3">
                <div className="flex items-center gap-2">
                  <ShieldAlert aria-hidden="true" className="w-5 h-5 text-[var(--color-warning)]" />
                  <span className="text-xs font-semibold text-[var(--color-text)]">
                    Permission required: <span className="font-mono">{approval.toolName}</span>
                  </span>
                  {approval.dangerLevel && (
                    <span className="text-[10px] px-1.5 py-0.5 rounded border border-[var(--color-warning)]/50 text-[var(--color-warning)] font-mono">
                      {approval.dangerLevel}
                    </span>
                  )}
                </div>

                <div className="flex items-center gap-2">
                  <button
                    type="button"
                    onClick={() => void handleResolveApproval(approval.id, 'allow_once')}
                    disabled={approval.state === 'resolving'}
                    className="flex items-center gap-1 text-xs px-3 py-1 border border-[var(--color-green)] text-[var(--color-text)] rounded font-medium hover:bg-[var(--color-green)]/10 disabled:opacity-50"
                  >
                    <Check aria-hidden="true" className="w-3.5 h-3.5 text-[var(--color-green)]" />
                    Allow once
                  </button>
                  <button
                    type="button"
                    onClick={() => void handleResolveApproval(approval.id, 'deny')}
                    disabled={approval.state === 'resolving'}
                    className="flex items-center gap-1 text-xs px-3 py-1 border border-[var(--color-red)] text-[var(--color-text)] rounded font-medium hover:bg-[var(--color-red)]/10 disabled:opacity-50"
                  >
                    <X aria-hidden="true" className="w-3.5 h-3.5 text-[var(--color-red)]" />
                    Deny
                  </button>
                </div>
              </div>

              {approval.error && (
                <div className="text-[11px] text-[var(--color-red)] bg-[var(--color-red)]/10 p-2 rounded">{approval.error}</div>
              )}

              <pre className="text-xs font-mono bg-[var(--color-bg)] p-3 rounded-lg border border-[var(--color-border)] text-[var(--color-text)] max-h-36 overflow-y-auto whitespace-pre-wrap break-all">
                {approval.redactedInput}
              </pre>

              {approval.expiresAt && (
                <div className="text-[11px] text-[var(--color-text2)]">
                  Expires at {new Date(approval.expiresAt).toLocaleTimeString()}
                </div>
              )}
            </li>
          ))}
        </ul>
      )}

      {/* Execution Terminal Log */}
      {teamLog.length > 0 && (
        <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl overflow-hidden">
          <div className="px-4 py-2 bg-[var(--color-surface2)] border-b border-[var(--color-border)] text-xs font-semibold text-[var(--color-text)] flex items-center gap-1.5">
            <Terminal className="w-3.5 h-3.5 text-[var(--color-accent)]" />
            <span>Execution Log</span>
          </div>
          <div className="p-3 bg-[var(--color-bg)] font-mono text-xs max-h-64 overflow-y-auto space-y-1">
            {teamLog.map((log, i) => (
              <div key={i} className="text-[var(--color-text2)] whitespace-pre-wrap leading-relaxed">
                {log}
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}
