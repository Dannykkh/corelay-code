import { useEffect, useState } from 'react';
import { fetchJSON, postJSON, type ProviderInfo } from '../lib/api';
import { listWorkstreams, type Workstream } from '../lib/workstreams';

type GatewayUser = {
  id: string;
  name: string;
  role: string;
  monthlyBudget: number;
  currentSpend: number;
  token: string;
};

type AuditEntry = {
  time: string;
  userId: string;
  provider: string;
  model: string;
  cost: number;
};

type TeamTaskDraft = {
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

type TeamSSEEvent = {
  type?: string;
  data?: unknown;
};

const taskKinds = ['research', 'propose', 'blueprint', 'implement', 'review', 'verify'];
const taskRoles = ['researcher', 'planner', 'architect', 'implementer', 'reviewer', 'verifier'];

const fieldClass = 'bg-[var(--color-bg)] border border-[var(--color-border)] rounded px-2 py-1.5 text-xs text-[var(--color-text)]';
const labelClass = 'block text-[10px] uppercase text-[var(--color-text2)] mb-1';

function newTaskDraft(index: number): TeamTaskDraft {
  return {
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

export function TeamPage() {
  const [users, setUsers] = useState<GatewayUser[]>([]);
  const [audit, setAudit] = useState<AuditEntry[]>([]);
  const [providers, setProviders] = useState<ProviderInfo[]>([]);
  const [workstreams, setWorkstreams] = useState<Workstream[]>([]);
  const [newUser, setNewUser] = useState({ name: '', role: 'developer', budget: 50 });
  const [copied, setCopied] = useState('');
  const [teamName, setTeamName] = useState('web-team');
  const [objective, setObjective] = useState('');
  const [verifyCommand, setVerifyCommand] = useState('');
  const [selectedWorkstreamId, setSelectedWorkstreamId] = useState('');
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

  useEffect(() => { load(); }, []);

  async function load() {
    const [u, a, p, w] = await Promise.all([
      fetchJSON<GatewayUser[]>('/api/gateway/users').catch(() => []),
      fetchJSON<AuditEntry[]>('/api/gateway/audit').catch(() => []),
      fetchJSON<ProviderInfo[]>('/api/providers').catch(() => []),
      listWorkstreams().catch(() => []),
    ]);
    setUsers(Array.isArray(u) ? u : []);
    setAudit(Array.isArray(a) ? a : []);
    setProviders(Array.isArray(p) ? p : []);
    setWorkstreams(Array.isArray(w) ? w : []);
  }

  async function addUser() {
    if (!newUser.name) return;
    await postJSON('/api/gateway/users', newUser);
    setNewUser({ name: '', role: 'developer', budget: 50 });
    load();
  }

  function copyToken(token: string) {
    navigator.clipboard.writeText(token);
    setCopied(token);
    setTimeout(() => setCopied(''), 2000);
  }

  function addTaskRow() {
    setTeamTasks((prev) => [...prev, newTaskDraft(prev.length + 1)]);
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
    setTeamRunning(true);
    setTeamLog([]);

    const tasks = teamTasks.filter((task) => task.name.trim() && task.description.trim()).map((task) => {
      const files = splitList(task.files);
      return {
        id: task.id.trim(),
        name: task.name.trim(),
        description: task.description.trim(),
        kind: task.kind,
        role: task.role,
        provider: task.provider.trim(),
        model: task.model.trim(),
        readOnly: task.readOnly,
        files: task.readOnly ? files : (files.length > 0 ? files : ['**']),
        dependsOn: splitList(task.dependsOn),
        resources: {
          modelSlots: positiveInt(task.modelSlots),
          toolSlots: positiveInt(task.toolSlots),
          webFetchSlots: positiveInt(task.webFetchSlots),
          testSlots: positiveInt(task.testSlots),
        },
      };
    });

    if (tasks.length === 0) {
      setTeamLog(['Error: at least one task needs a name and description']);
      setTeamRunning(false);
      return;
    }

    try {
      const res = await fetch('/api/team', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: teamName.trim() || 'web-team',
          objective: objective.trim() || teamName.trim() || 'web-team',
          verifyCommand: verifyCommand.trim(),
          workstreamId: selectedWorkstreamId || undefined,
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

      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buffer = '';

      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        const lines = buffer.split('\n');
        buffer = lines.pop() || '';
        for (const line of lines) {
          if (!line.startsWith('data: ')) continue;
          try {
            const event = JSON.parse(line.slice(6)) as TeamSSEEvent;
            if (event.type === 'status' || event.type === 'text' || event.type === 'error') {
              const msg = stringifyEventData(event.data);
              if (msg) setTeamLog((prev) => [...prev, msg]);
            } else if (event.type === 'session') {
              const data = event.data as { traceId?: string };
              if (data.traceId) setTeamLog((prev) => [...prev, `Trace: ${data.traceId}`]);
            } else if (event.type === 'workstream') {
              const data = event.data as { title?: string; id?: string };
              const label = data.title || data.id;
              if (label) setTeamLog((prev) => [...prev, `Workstream: ${label}`]);
            }
          } catch {
            setTeamLog((prev) => [...prev, line.slice(6)]);
          }
        }
      }
    } catch (err) {
      setTeamLog((prev) => [...prev, `Error: ${err instanceof Error ? err.message : String(err)}`]);
    } finally {
      setTeamRunning(false);
    }
  }

  return (
    <div className="p-6 w-full overflow-y-auto h-full">
      <h1 className="text-xl font-semibold mb-6">Team</h1>

      <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4 mb-6">
        <div className="text-xs text-[var(--color-text2)] uppercase mb-3">Agent Team - Wave Execution</div>

        <div className="grid grid-cols-4 gap-3 mb-4">
          <label>
            <span className={labelClass}>Name</span>
            <input value={teamName} onChange={(e) => setTeamName(e.target.value)}
              className={`${fieldClass} w-full`} />
          </label>
          <label>
            <span className={labelClass}>Workstream</span>
            <select value={selectedWorkstreamId} onChange={(e) => setSelectedWorkstreamId(e.target.value)}
              className={`${fieldClass} w-full`}>
              <option value="">No workstream</option>
              {workstreams.map((ws) => (
                <option key={ws.id} value={ws.id}>{ws.title}</option>
              ))}
            </select>
          </label>
          <label className="col-span-2">
            <span className={labelClass}>Objective</span>
            <input value={objective} onChange={(e) => setObjective(e.target.value)}
              placeholder="Team objective" className={`${fieldClass} w-full`} />
          </label>
          <label className="col-span-4">
            <span className={labelClass}>Verify command</span>
            <input value={verifyCommand} onChange={(e) => setVerifyCommand(e.target.value)}
              placeholder="go test ./... -count=1" className={`${fieldClass} w-full font-mono`} />
          </label>
        </div>

        <div className="grid grid-cols-5 gap-2 mb-4">
          <label>
            <span className={labelClass}>Model slots</span>
            <input type="number" min="1" value={capacity.modelSlots}
              onChange={(e) => updateCapacity('modelSlots', e.target.value)}
              className={`${fieldClass} w-full`} />
          </label>
          <label>
            <span className={labelClass}>Tool slots</span>
            <input type="number" min="1" value={capacity.toolSlots}
              onChange={(e) => updateCapacity('toolSlots', e.target.value)}
              placeholder="default" className={`${fieldClass} w-full`} />
          </label>
          <label>
            <span className={labelClass}>Web slots</span>
            <input type="number" min="1" value={capacity.webFetchSlots}
              onChange={(e) => updateCapacity('webFetchSlots', e.target.value)}
              placeholder="default" className={`${fieldClass} w-full`} />
          </label>
          <label>
            <span className={labelClass}>Test slots</span>
            <input type="number" min="1" value={capacity.testSlots}
              onChange={(e) => updateCapacity('testSlots', e.target.value)}
              placeholder="default" className={`${fieldClass} w-full`} />
          </label>
          <label>
            <span className={labelClass}>Max parallel</span>
            <input type="number" min="1" value={capacity.maxParallelTasks}
              onChange={(e) => updateCapacity('maxParallelTasks', e.target.value)}
              className={`${fieldClass} w-full`} />
          </label>
        </div>

        <div className="overflow-x-auto border border-[var(--color-border)] rounded">
          <div className="min-w-[1180px] divide-y divide-[var(--color-border)]">
            {teamTasks.map((task, i) => {
              const models = providerModels(task.provider);
              return (
                <div key={task.id} className="p-3">
                  <div className="grid grid-cols-[96px_160px_1fr_120px_120px_120px_160px] gap-2 items-end">
                    <label>
                      <span className={labelClass}>ID</span>
                      <input value={task.id} onChange={(e) => updateTask(i, 'id', e.target.value)}
                        className={`${fieldClass} w-full font-mono`} />
                    </label>
                    <label>
                      <span className={labelClass}>Name</span>
                      <input value={task.name} onChange={(e) => updateTask(i, 'name', e.target.value)}
                        placeholder="Task name" className={`${fieldClass} w-full`} />
                    </label>
                    <label>
                      <span className={labelClass}>Description</span>
                      <input value={task.description} onChange={(e) => updateTask(i, 'description', e.target.value)}
                        placeholder="What this worker should do" className={`${fieldClass} w-full`} />
                    </label>
                    <label>
                      <span className={labelClass}>Kind</span>
                      <select value={task.kind} onChange={(e) => updateTask(i, 'kind', e.target.value)}
                        className={`${fieldClass} w-full`}>
                        {taskKinds.map((kind) => <option key={kind} value={kind}>{kind}</option>)}
                      </select>
                    </label>
                    <label>
                      <span className={labelClass}>Role</span>
                      <select value={task.role} onChange={(e) => updateTask(i, 'role', e.target.value)}
                        className={`${fieldClass} w-full`}>
                        {taskRoles.map((role) => <option key={role} value={role}>{role}</option>)}
                      </select>
                    </label>
                    <label>
                      <span className={labelClass}>Provider</span>
                      <select value={task.provider} onChange={(e) => updateTaskProvider(i, e.target.value)}
                        className={`${fieldClass} w-full`}>
                        <option value="">default</option>
                        {providers.map((provider) => <option key={provider.name} value={provider.name}>{provider.displayName}</option>)}
                      </select>
                    </label>
                    <label>
                      <span className={labelClass}>Model</span>
                      {models.length > 0 ? (
                        <select value={task.model} onChange={(e) => updateTask(i, 'model', e.target.value)}
                          className={`${fieldClass} w-full`}>
                          <option value="">default</option>
                          {models.map((model) => <option key={model.id} value={model.id}>{model.displayName}</option>)}
                        </select>
                      ) : (
                        <input value={task.model} onChange={(e) => updateTask(i, 'model', e.target.value)}
                          placeholder="default" className={`${fieldClass} w-full`} />
                      )}
                    </label>
                  </div>

                  <div className="grid grid-cols-[1fr_180px_78px_78px_78px_78px_82px] gap-2 mt-2 items-end">
                    <label>
                      <span className={labelClass}>Files</span>
                      <input value={task.files} onChange={(e) => updateTask(i, 'files', e.target.value)}
                        placeholder="**" className={`${fieldClass} w-full font-mono`} />
                    </label>
                    <label>
                      <span className={labelClass}>Depends on</span>
                      <input value={task.dependsOn} onChange={(e) => updateTask(i, 'dependsOn', e.target.value)}
                        placeholder="task-1, task-2" className={`${fieldClass} w-full font-mono`} />
                    </label>
                    <label>
                      <span className={labelClass}>Model</span>
                      <input type="number" min="1" value={task.modelSlots}
                        onChange={(e) => updateTask(i, 'modelSlots', e.target.value)}
                        placeholder="1" className={`${fieldClass} w-full`} />
                    </label>
                    <label>
                      <span className={labelClass}>Tool</span>
                      <input type="number" min="1" value={task.toolSlots}
                        onChange={(e) => updateTask(i, 'toolSlots', e.target.value)}
                        placeholder="0" className={`${fieldClass} w-full`} />
                    </label>
                    <label>
                      <span className={labelClass}>Web</span>
                      <input type="number" min="1" value={task.webFetchSlots}
                        onChange={(e) => updateTask(i, 'webFetchSlots', e.target.value)}
                        placeholder="0" className={`${fieldClass} w-full`} />
                    </label>
                    <label>
                      <span className={labelClass}>Test</span>
                      <input type="number" min="1" value={task.testSlots}
                        onChange={(e) => updateTask(i, 'testSlots', e.target.value)}
                        placeholder="0" className={`${fieldClass} w-full`} />
                    </label>
                    <div className="flex items-center justify-end gap-2 pb-1.5">
                      <label className="flex items-center gap-1 text-xs text-[var(--color-text2)]">
                        <input type="checkbox" checked={task.readOnly}
                          onChange={(e) => updateTask(i, 'readOnly', e.target.checked)}
                          className="accent-[var(--color-accent)]" />
                        Read
                      </label>
                      {teamTasks.length > 1 && (
                        <button onClick={() => removeTaskRow(i)}
                          className="text-xs px-2 py-1 bg-[var(--color-surface2)] text-[var(--color-text2)] rounded hover:text-[var(--color-text)]">
                          Remove
                        </button>
                      )}
                    </div>
                  </div>
                </div>
              );
            })}
          </div>
        </div>

        <div className="flex gap-2 mt-3">
          <button onClick={addTaskRow} className="text-xs px-3 py-1.5 bg-[var(--color-surface2)] text-[var(--color-text)] rounded hover:bg-[var(--color-border)]">
            Add Task
          </button>
          <button onClick={runTeam} disabled={teamRunning}
            className="text-xs px-4 py-1.5 bg-[var(--color-accent)] text-white rounded hover:opacity-80 disabled:opacity-40">
            {teamRunning ? 'Running...' : 'Execute Team'}
          </button>
        </div>

        {teamLog.length > 0 && (
          <div className="mt-3 bg-[var(--color-bg)] rounded p-3 font-mono text-xs max-h-56 overflow-y-auto">
            {teamLog.map((log, i) => (
              <div key={i} className="py-0.5 text-[var(--color-text2)] whitespace-pre-wrap">{log}</div>
            ))}
          </div>
        )}
      </div>

      <h2 className="text-lg font-semibold mb-4">Team Gateway</h2>

      <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4 mb-6">
        <div className="text-xs text-[var(--color-text2)] uppercase mb-3">Add Team Member</div>
        <div className="flex gap-2">
          <input value={newUser.name} onChange={(e) => setNewUser({ ...newUser, name: e.target.value })}
            placeholder="Name" className="w-40 bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg px-3 py-2 text-sm text-[var(--color-text)]" />
          <select value={newUser.role} onChange={(e) => setNewUser({ ...newUser, role: e.target.value })}
            className="bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg px-3 py-2 text-sm text-[var(--color-text)]">
            <option value="admin">Admin</option>
            <option value="developer">Developer</option>
            <option value="viewer">Viewer</option>
          </select>
          <input type="number" value={newUser.budget} onChange={(e) => setNewUser({ ...newUser, budget: +e.target.value })}
            placeholder="Monthly budget ($)" className="w-32 bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg px-3 py-2 text-sm text-[var(--color-text)]" />
          <button onClick={addUser} className="px-4 py-2 bg-[var(--color-accent)] text-white rounded-lg text-sm">Add</button>
        </div>
      </div>

      <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl overflow-hidden mb-6">
        <table className="w-full">
          <thead>
            <tr className="bg-[var(--color-surface2)] text-[var(--color-text2)] text-xs uppercase">
              <th className="text-left px-4 py-2">Name</th>
              <th className="text-left px-4 py-2">Role</th>
              <th className="text-left px-4 py-2">Budget</th>
              <th className="text-left px-4 py-2">Spent</th>
              <th className="text-left px-4 py-2">Token</th>
            </tr>
          </thead>
          <tbody>
            {users.length === 0 ? (
              <tr><td colSpan={5} className="text-center py-6 text-[var(--color-text2)] text-sm">No users yet</td></tr>
            ) : (
              users.map((u) => (
                <tr key={u.id} className="border-t border-[var(--color-border)]">
                  <td className="px-4 py-2.5 text-sm font-medium">{u.name}</td>
                  <td className="px-4 py-2.5">
                    <span className={`text-xs px-2 py-0.5 rounded-full ${
                      u.role === 'admin' ? 'bg-orange-500/15 text-orange-400' :
                      u.role === 'developer' ? 'bg-blue-500/15 text-blue-400' : 'bg-gray-500/15 text-gray-400'
                    }`}>{u.role}</span>
                  </td>
                  <td className="px-4 py-2.5 text-sm">${u.monthlyBudget}</td>
                  <td className="px-4 py-2.5 text-sm">
                    <span className={u.currentSpend > u.monthlyBudget * 0.8 ? 'text-[var(--color-red)]' : ''}>
                      ${u.currentSpend.toFixed(2)}
                    </span>
                  </td>
                  <td className="px-4 py-2.5">
                    <button onClick={() => copyToken(u.token)}
                      className="text-xs font-mono bg-[var(--color-bg)] px-2 py-1 rounded border border-[var(--color-border)] hover:border-[var(--color-accent)]">
                      {copied === u.token ? 'Copied!' : u.token.slice(0, 12) + '...'}
                    </button>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>

      <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4">
        <div className="text-xs text-[var(--color-text2)] uppercase mb-3">Audit Log</div>
        <div className="max-h-60 overflow-y-auto font-mono text-xs space-y-1">
          {audit.length === 0 ? (
            <div className="text-[var(--color-text2)]">No audit entries yet</div>
          ) : (
            audit.map((a, i) => (
              <div key={i} className="flex gap-3 py-1 border-b border-[var(--color-border)] last:border-0">
                <span className="text-[var(--color-text2)] w-20">{new Date(a.time).toLocaleTimeString()}</span>
                <span className="text-[var(--color-accent)]">{a.userId}</span>
                <span>{a.provider}/{a.model}</span>
                <span className="text-[var(--color-green)]">${a.cost.toFixed(4)}</span>
              </div>
            ))
          )}
        </div>
      </div>
    </div>
  );
}
