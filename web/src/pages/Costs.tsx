import { useState, useEffect } from 'react';
import { fetchJSON, type CostEntry } from '../lib/api';

type RunTrace = {
  id: string;
  kind: string;
  startedAt: string;
  durationMs?: number;
  provider: string;
  model: string;
  workstreamId?: string;
  status: 'running' | 'ok' | 'failed' | string;
  error?: string;
  spans?: { name: string; status: string; durationMs?: number }[];
  metadata?: Record<string, string>;
};

type RegressionCase = {
  id: string;
  traceId: string;
  createdAt: string;
  name: string;
  kind: string;
  provider: string;
  model: string;
  replayable: boolean;
  replayHint?: string;
  inputs?: Record<string, string>;
  failure: { status: string; error?: string; failedSpans?: string[] };
};

type RegressionRun = {
  id: string;
  caseId: string;
  traceId: string;
  runTraceId?: string;
  startedAt: string;
  durationMs?: number;
  kind: string;
  provider: string;
  model: string;
  status: 'running' | 'passed' | 'failed' | 'unsupported' | string;
  error?: string;
  checks?: { name: string; target: string; expected: string; observed: string; passed: boolean }[];
};

type ProviderMetrics = {
  requests: number;
  avgLatencyMs: number;
  cost: number;
  errors: number;
};

type Metrics = {
  totalRequests: number;
  avgLatencyMs: number;
  p95LatencyMs: number;
  errorRate: number;
  byProvider?: Record<string, ProviderMetrics>;
};

type RequestTrace = {
  timestamp: string;
  status: string;
  provider: string;
  model: string;
  latencyMs: number;
  outputTokens: number;
  source: string;
};

type FeedbackStats = {
  total: number;
  thumbsUp: number;
  thumbsDown: number;
  score: number;
  byModel?: Record<string, { up: number; down: number; score: number }>;
};

type ABResult = {
  modelA: string;
  modelB: string;
  responseA?: string;
  responseB?: string;
  latencyA_ms: number;
  latencyB_ms: number;
  tokensA: number;
  tokensB: number;
  winner?: string;
};

type SSEEvent = {
  type?: string;
  data?: unknown;
};

const replayableRegressionKinds = new Set(['chronos', 'team']);

function canReplayRegression(c: RegressionCase): boolean {
  return c.replayable && replayableRegressionKinds.has(c.kind);
}

function regressionCaseLabel(c: RegressionCase): string {
  return c.inputs?.task || c.inputs?.objective || c.inputs?.teamName || c.name;
}

function regressionReplayTitle(c: RegressionCase): string {
  if (canReplayRegression(c)) {
    return `Replay ${c.kind} regression`;
  }
  return c.replayHint || 'Replay unsupported';
}

export function CostsPage() {
  const [costs, setCosts] = useState<{ total: number; breakdown: CostEntry[] }>({ total: 0, breakdown: [] });
  const [metrics, setMetrics] = useState<Metrics | null>(null);
  const [traces, setTraces] = useState<RequestTrace[]>([]);
  const [runTraces, setRunTraces] = useState<RunTrace[]>([]);
  const [regressionCases, setRegressionCases] = useState<RegressionCase[]>([]);
  const [regressionRuns, setRegressionRuns] = useState<RegressionRun[]>([]);
  const [feedbackStats, setFeedbackStats] = useState<FeedbackStats | null>(null);
  const [abPrompt, setAbPrompt] = useState('');
  const [abRunning, setAbRunning] = useState(false);
  const [abResults, setAbResults] = useState<ABResult[]>([]);
  const [runActionId, setRunActionId] = useState<string | null>(null);
  const [actionMessage, setActionMessage] = useState<string | null>(null);

  useEffect(() => {
    load();
    const interval = setInterval(load, 5000);
    return () => clearInterval(interval);
  }, []);

  async function load() {
    const [c, m, t, rt, rc, rr, f] = await Promise.all([
      fetchJSON<{ total: number; breakdown: CostEntry[] }>('/api/costs').catch(() => ({ total: 0, breakdown: [] })),
      fetchJSON<Metrics>('/api/metrics?window=60').catch(() => null),
      fetchJSON<RequestTrace[]>('/api/traces?limit=20').catch(() => []),
      fetchJSON<RunTrace[]>('/api/run-traces?limit=40').catch(() => []),
      fetchJSON<RegressionCase[]>('/api/regressions?limit=40').catch(() => []),
      fetchJSON<RegressionRun[]>('/api/regression-runs?limit=40').catch(() => []),
      fetchJSON<FeedbackStats>('/api/feedback').catch(() => null),
    ]);
    setCosts(c);
    setMetrics(m);
    setTraces(Array.isArray(t) ? t : []);
    setRunTraces(Array.isArray(rt) ? rt : []);
    setRegressionCases(Array.isArray(rc) ? rc : []);
    setRegressionRuns(Array.isArray(rr) ? rr : []);
    setFeedbackStats(f);
  }

  async function createRegression(traceId: string) {
    setRunActionId(traceId);
    setActionMessage(null);
    try {
      const c = await fetchJSON<RegressionCase>(`/api/run-traces/${encodeURIComponent(traceId)}/regression`, {
        method: 'POST',
      });
      setActionMessage(`Regression case ready: ${c.id}`);
      await load();
    } catch (err) {
      setActionMessage(err instanceof Error ? err.message : String(err));
    } finally {
      setRunActionId(null);
    }
  }

  async function replayRegression(caseId: string) {
    setRunActionId(caseId);
    setActionMessage('Replay started');
    try {
      const res = await fetch(`/api/regressions/${encodeURIComponent(caseId)}/run`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({}),
      });
      if (!res.ok) {
        const msg = await readError(res);
        throw new Error(msg);
      }
      const result = await readRegressionReplay(res);
      setActionMessage(result ? `Replay ${result.status}: ${result.id}` : 'Replay completed');
      await load();
    } catch (err) {
      setActionMessage(err instanceof Error ? err.message : String(err));
    } finally {
      setRunActionId(null);
    }
  }

  const maxCost = Math.max(...costs.breakdown.map((b) => b.cost), 0.001);
  const failedRuns = runTraces.filter((t) => t.status === 'failed').slice().reverse().slice(0, 6);
  const recentAgenticRuns = runTraces.slice().reverse().slice(0, 8);
  const latestRegressionRuns = regressionRuns.slice().reverse().slice(0, 8);
  const casesByTrace = new Map(regressionCases.map((c) => [c.traceId, c]));
  const lastRunByCase = new Map<string, RegressionRun>();
  for (const run of regressionRuns) {
    lastRunByCase.set(run.caseId, run);
  }

  return (
    <div className="p-6 w-full overflow-y-auto h-full">
      <h1 className="text-xl font-semibold mb-1">Activity</h1>
      <p className="text-sm text-[var(--color-text2)] mb-6">How your AI is being used — requests, speed, costs, and quality</p>

      {/* Metrics Cards */}
      <div className="grid grid-cols-5 gap-3 mb-6">
        <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4">
          <div className="text-[10px] text-[var(--color-text2)] uppercase mb-1">Total Cost</div>
          <div className="text-xl font-bold text-[var(--color-green)]">${costs.total.toFixed(4)}</div>
        </div>
        <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4">
          <div className="text-[10px] text-[var(--color-text2)] uppercase mb-1">Requests (1h)</div>
          <div className="text-xl font-bold text-[var(--color-accent)]">{metrics?.totalRequests || 0}</div>
        </div>
        <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4">
          <div className="text-[10px] text-[var(--color-text2)] uppercase mb-1">Avg Latency</div>
          <div className="text-xl font-bold">{metrics?.avgLatencyMs || 0}<span className="text-xs font-normal">ms</span></div>
        </div>
        <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4">
          <div className="text-[10px] text-[var(--color-text2)] uppercase mb-1">P95 Latency</div>
          <div className="text-xl font-bold">{metrics?.p95LatencyMs || 0}<span className="text-xs font-normal">ms</span></div>
        </div>
        <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4">
          <div className="text-[10px] text-[var(--color-text2)] uppercase mb-1">Error Rate</div>
          <div className={`text-xl font-bold ${(metrics?.errorRate || 0) > 0.1 ? 'text-[var(--color-red)]' : 'text-[var(--color-green)]'}`}>
            {((metrics?.errorRate || 0) * 100).toFixed(1)}%
          </div>
        </div>
      </div>

      {/* Provider Breakdown + Feedback */}
      <div className="grid grid-cols-2 gap-4 mb-6">
        {/* Provider Metrics */}
        <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4">
          <div className="text-xs text-[var(--color-text2)] uppercase mb-3">Provider Breakdown (1h)</div>
          {metrics?.byProvider && Object.keys(metrics.byProvider).length > 0 ? (
            <div className="space-y-2">
              {Object.entries(metrics.byProvider).map(([name, pm]) => (
                <div key={name} className="flex items-center justify-between text-sm">
                  <span className="font-medium">{name}</span>
                  <div className="flex gap-4 text-xs text-[var(--color-text2)]">
                    <span>{pm.requests} reqs</span>
                    <span>{pm.avgLatencyMs}ms</span>
                    <span>${pm.cost.toFixed(4)}</span>
                    {pm.errors > 0 && <span className="text-[var(--color-red)]">{pm.errors} err</span>}
                  </div>
                </div>
              ))}
            </div>
          ) : (
            <div className="text-xs text-[var(--color-text2)]">No data yet</div>
          )}
        </div>

        {/* Feedback / Evals */}
        <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4">
          <div className="text-xs text-[var(--color-text2)] uppercase mb-3">Response Quality (Evals)</div>
          {feedbackStats && feedbackStats.total > 0 ? (
            <div>
              <div className="flex items-center gap-4 mb-3">
                <div className="text-3xl font-bold">{(feedbackStats.score * 100).toFixed(0)}%</div>
                <div className="text-xs text-[var(--color-text2)]">
                  <div>{feedbackStats.thumbsUp} positive / {feedbackStats.thumbsDown} negative</div>
                  <div>{feedbackStats.total} total ratings</div>
                </div>
              </div>
              {Object.entries(feedbackStats.byModel || {}).map(([model, ms]) => (
                <div key={model} className="flex items-center justify-between text-xs py-1">
                  <span className="font-mono">{model}</span>
                  <div className="flex items-center gap-2">
                    <div className="w-24 h-2 bg-[var(--color-bg)] rounded overflow-hidden">
                      <div className="h-full bg-[var(--color-green)] rounded" style={{ width: `${ms.score * 100}%` }} />
                    </div>
                    <span>{(ms.score * 100).toFixed(0)}%</span>
                  </div>
                </div>
              ))}
            </div>
          ) : (
            <div className="text-xs text-[var(--color-text2)]">
              No feedback yet. Rate responses in chat to start tracking quality.
            </div>
          )}
        </div>
      </div>

      {/* Cost Table */}
      <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl overflow-hidden mb-6">
        <table className="w-full">
          <thead>
            <tr className="bg-[var(--color-surface2)] text-[var(--color-text2)] text-xs uppercase">
              <th className="text-left px-4 py-3">Provider / Model</th>
              <th className="text-left px-4 py-3">Requests</th>
              <th className="text-left px-4 py-3">Tokens</th>
              <th className="text-left px-4 py-3">Cost</th>
              <th className="text-left px-4 py-3">Share</th>
            </tr>
          </thead>
          <tbody>
            {costs.breakdown.length === 0 ? (
              <tr><td colSpan={5} className="text-center py-6 text-[var(--color-text2)] text-sm">No requests yet</td></tr>
            ) : (
              costs.breakdown.map((b) => (
                <tr key={`${b.provider}/${b.model}`} className="border-t border-[var(--color-border)]">
                  <td className="px-4 py-2.5 text-sm">{b.provider}/{b.model}</td>
                  <td className="px-4 py-2.5 text-sm">{b.requests}</td>
                  <td className="px-4 py-2.5 text-sm">{b.tokens.toLocaleString()}</td>
                  <td className="px-4 py-2.5 text-sm">${b.cost.toFixed(4)}</td>
                  <td className="px-4 py-2.5 w-32">
                    <div className="h-4 bg-[var(--color-bg)] rounded overflow-hidden">
                      <div className="h-full bg-[var(--color-accent)] rounded" style={{ width: `${(b.cost / maxCost) * 100}%` }} />
                    </div>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>

      {/* Agentic run loop */}
      <div className="mb-6">
        <div className="flex items-center justify-between mb-3">
          <div>
            <div className="text-xs text-[var(--color-text2)] uppercase">Agentic Runs</div>
            <div className="text-sm text-[var(--color-text)]">Trace failures, promote them to regressions, and replay fixed cases.</div>
          </div>
          <div className="flex items-center gap-2 text-xs">
            {actionMessage && <span className="text-[var(--color-text2)] max-w-96 truncate">{actionMessage}</span>}
            <button
              onClick={load}
              className="px-3 py-1.5 rounded border border-[var(--color-border)] text-[var(--color-text)] hover:border-[var(--color-accent)] active:scale-[0.98]"
            >
              Refresh
            </button>
          </div>
        </div>

        <div className="grid grid-cols-4 gap-3 mb-3">
          <MetricTile label="Run traces" value={runTraces.length} tone="accent" />
          <MetricTile label="Failed runs" value={failedRuns.length} tone={failedRuns.length > 0 ? 'red' : 'green'} />
          <MetricTile label="Regression cases" value={regressionCases.length} tone="accent" />
          <MetricTile label="Replay passes" value={regressionRuns.filter((r) => r.status === 'passed').length} tone="green" />
        </div>

        <div className="grid grid-cols-2 gap-4 mb-4">
          <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl overflow-hidden">
            <div className="px-4 py-3 bg-[var(--color-surface2)] border-b border-[var(--color-border)] flex items-center justify-between">
              <div className="text-xs text-[var(--color-text2)] uppercase">Failed Run Traces</div>
              <div className="text-[10px] text-[var(--color-text2)]">{failedRuns.length} visible</div>
            </div>
            <div className="divide-y divide-[var(--color-border)]">
              {failedRuns.length === 0 ? (
                <div className="px-4 py-6 text-xs text-[var(--color-text2)]">No failed agentic runs recorded.</div>
              ) : failedRuns.map((run) => {
                const c = casesByTrace.get(run.id);
                return (
                  <div key={run.id} className="px-4 py-3 text-xs">
                    <div className="flex items-center gap-2 min-w-0">
                      <StatusPill status={run.status} />
                      <span className="font-mono text-[var(--color-accent)] truncate">{shortID(run.id)}</span>
                      <span className="text-[var(--color-text2)]">{run.kind}</span>
                      <span className="text-[var(--color-text2)] ml-auto">{formatTime(run.startedAt)}</span>
                    </div>
                    <div className="mt-1 text-[var(--color-text2)] truncate">
                      {run.error || run.spans?.find((s) => s.status === 'failed')?.name || 'failed without detail'}
                    </div>
                    <div className="mt-2 flex items-center gap-2">
                      {c ? (
                        <span className="text-[var(--color-green)]">Case {shortID(c.id)}</span>
                      ) : (
                        <button
                          onClick={() => createRegression(run.id)}
                          disabled={runActionId === run.id}
                          className="px-2.5 py-1 rounded bg-[var(--color-surface2)] text-[var(--color-text)] border border-[var(--color-border)] hover:border-[var(--color-accent)] disabled:opacity-40 active:scale-[0.98]"
                        >
                          {runActionId === run.id ? 'Creating' : 'Create Case'}
                        </button>
                      )}
                      <span className="text-[var(--color-text2)] truncate">{run.provider}/{run.model}</span>
                    </div>
                  </div>
                );
              })}
            </div>
          </div>

          <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl overflow-hidden">
            <div className="px-4 py-3 bg-[var(--color-surface2)] border-b border-[var(--color-border)] flex items-center justify-between">
              <div className="text-xs text-[var(--color-text2)] uppercase">Regression Cases</div>
              <div className="text-[10px] text-[var(--color-text2)]">{regressionCases.length} total</div>
            </div>
            <div className="divide-y divide-[var(--color-border)] max-h-80 overflow-y-auto">
              {regressionCases.length === 0 ? (
                <div className="px-4 py-6 text-xs text-[var(--color-text2)]">Create a case from a failed run trace.</div>
              ) : regressionCases.slice().reverse().slice(0, 8).map((c) => {
                const last = lastRunByCase.get(c.id);
                const canReplay = canReplayRegression(c);
                return (
                  <div key={c.id} className="px-4 py-3 text-xs">
                    <div className="flex items-center gap-2 min-w-0">
                      <span className="font-mono text-[var(--color-accent)] truncate">{shortID(c.id)}</span>
                      <span className="text-[var(--color-text2)]">{c.kind}</span>
                      <span className={c.replayable ? 'text-[var(--color-green)]' : 'text-[var(--color-yellow)]'}>
                        {c.replayable ? 'replayable' : 'checks only'}
                      </span>
                      <span className="text-[var(--color-text2)] ml-auto">{formatTime(c.createdAt)}</span>
                    </div>
                    <div className="mt-1 text-[var(--color-text)] truncate">{regressionCaseLabel(c)}</div>
                    <div className="mt-2 flex items-center gap-2">
                      <button
                        onClick={() => replayRegression(c.id)}
                        disabled={!canReplay || runActionId === c.id}
                        title={regressionReplayTitle(c)}
                        className="px-2.5 py-1 rounded bg-[var(--color-accent)] text-white disabled:bg-[var(--color-surface2)] disabled:text-[var(--color-text2)] disabled:opacity-60 active:scale-[0.98]"
                      >
                        {runActionId === c.id ? 'Running' : 'Replay'}
                      </button>
                      {last ? <StatusPill status={last.status} /> : <span className="text-[var(--color-text2)]">not replayed</span>}
                      <span className="text-[var(--color-text2)] truncate">{c.failure.error || c.failure.failedSpans?.join(', ')}</span>
                    </div>
                  </div>
                );
              })}
            </div>
          </div>
        </div>

        <div className="bg-[var(--color-bg)] border border-[var(--color-border)] rounded-xl p-4 font-mono text-xs max-h-64 overflow-auto">
          <div className="grid grid-cols-[5rem_5rem_7rem_1fr_6rem_7rem] gap-3 text-[var(--color-text2)] uppercase font-sans text-[10px] mb-2">
            <span>Time</span><span>Status</span><span>Kind</span><span>ID / Task</span><span>Duration</span><span>Run Trace</span>
          </div>
          {latestRegressionRuns.length === 0 && recentAgenticRuns.length === 0 ? (
            <div className="text-[var(--color-text2)]">No agentic run activity yet</div>
          ) : (
            <>
              {latestRegressionRuns.map((run) => (
                <div key={run.id} className="grid grid-cols-[5rem_5rem_7rem_1fr_6rem_7rem] gap-3 py-1 items-center border-t border-[var(--color-border)]/60">
                  <span className="text-[var(--color-text2)]">{formatTime(run.startedAt)}</span>
                  <StatusPill status={run.status} />
                  <span>{run.kind}</span>
                  <span className="truncate text-[var(--color-accent)]">{shortID(run.caseId)}</span>
                  <span className="text-[var(--color-text2)]">{formatDuration(run.durationMs)}</span>
                  <span className="text-[var(--color-text2)] truncate">{run.runTraceId ? shortID(run.runTraceId) : '-'}</span>
                </div>
              ))}
              {recentAgenticRuns.map((run) => (
                <div key={run.id} className="grid grid-cols-[5rem_5rem_7rem_1fr_6rem_7rem] gap-3 py-1 items-center border-t border-[var(--color-border)]/60">
                  <span className="text-[var(--color-text2)]">{formatTime(run.startedAt)}</span>
                  <StatusPill status={run.status} />
                  <span>{run.kind}</span>
                  <span className="truncate text-[var(--color-accent)]">{shortID(run.id)}</span>
                  <span className="text-[var(--color-text2)]">{formatDuration(run.durationMs)}</span>
                  <span className="text-[var(--color-text2)] truncate">{run.metadata?.source || 'live'}</span>
                </div>
              ))}
            </>
          )}
        </div>
      </div>

      {/* Request Trace Log */}
      <div className="bg-[var(--color-bg)] border border-[var(--color-border)] rounded-xl p-4 font-mono text-xs max-h-60 overflow-y-auto">
        <div className="text-[var(--color-text2)] uppercase mb-2 font-sans text-xs">Recent Requests</div>
        {traces.length === 0 ? (
          <div className="text-[var(--color-text2)]">No traces yet</div>
        ) : (
          traces.slice().reverse().map((t, i) => (
            <div key={i} className="flex gap-3 py-0.5 items-center">
              <span className="text-[var(--color-text2)] w-16 shrink-0">
                {new Date(t.timestamp).toLocaleTimeString()}
              </span>
              <span className={`w-12 shrink-0 ${t.status === 'ok' ? 'text-[var(--color-green)]' : 'text-[var(--color-red)]'}`}>
                {t.status}
              </span>
              <span className="text-[var(--color-accent)] w-20 shrink-0 truncate">{t.provider}</span>
              <span className="w-28 shrink-0 truncate">{t.model}</span>
              <span className="text-[var(--color-text2)] w-16 shrink-0">{t.latencyMs}ms</span>
              <span className="text-[var(--color-text2)] w-16 shrink-0">{t.outputTokens}tok</span>
              <span className="text-[var(--color-text2)] w-10 shrink-0">{t.source}</span>
            </div>
          ))
        )}
      </div>

      {/* A/B Model Test */}
      <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4 mt-6">
        <div className="text-xs text-[var(--color-text2)] uppercase mb-3">A/B Model Comparison</div>
        <div className="flex gap-2 mb-3">
          <input
            value={abPrompt}
            onChange={(e) => setAbPrompt(e.target.value)}
            placeholder="Enter a prompt to compare across models..."
            className="flex-1 bg-[var(--color-bg)] border border-[var(--color-border)] rounded px-3 py-2 text-sm text-[var(--color-text)]"
          />
          <button
            onClick={async () => {
              if (!abPrompt) return;
              setAbRunning(true);
              try {
                const result = await fetchJSON<ABResult>('/api/ab-test', {
                  method: 'POST',
                  headers: { 'Content-Type': 'application/json' },
                  body: JSON.stringify({ prompt: abPrompt }),
                });
                setAbResults(prev => [result, ...prev].slice(0, 10));
              } catch (err) {
                setActionMessage(err instanceof Error ? err.message : String(err));
              }
              setAbRunning(false);
            }}
            disabled={abRunning}
            className="px-4 py-2 bg-[var(--color-accent)] text-white rounded text-sm disabled:opacity-40"
          >
            {abRunning ? 'Running...' : 'Compare'}
          </button>
        </div>
        {abResults.map((r, i) => (
          <div key={i} className="bg-[var(--color-bg)] rounded-lg p-3 mb-2 text-xs">
            <div className="flex gap-4 mb-2">
              <span className="font-medium">{r.modelA}</span>
              <span className="text-[var(--color-text2)]">vs</span>
              <span className="font-medium">{r.modelB}</span>
              <span className="ml-auto text-[var(--color-accent)]">Winner: {r.winner || 'tie'}</span>
            </div>
            <div className="grid grid-cols-2 gap-2">
              <div className="text-[var(--color-text2)]">
                <div className="text-[9px] uppercase mb-1">Model A ({r.latencyA_ms}ms, {r.tokensA}tok)</div>
                <div className="truncate">{r.responseA?.slice(0, 100)}</div>
              </div>
              <div className="text-[var(--color-text2)]">
                <div className="text-[9px] uppercase mb-1">Model B ({r.latencyB_ms}ms, {r.tokensB}tok)</div>
                <div className="truncate">{r.responseB?.slice(0, 100)}</div>
              </div>
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

function MetricTile({ label, value, tone }: { label: string; value: number; tone: 'accent' | 'green' | 'red' }) {
  const toneClass = tone === 'green' ? 'text-[var(--color-green)]' : tone === 'red' ? 'text-[var(--color-red)]' : 'text-[var(--color-accent)]';
  return (
    <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-3">
      <div className="text-[10px] text-[var(--color-text2)] uppercase mb-1">{label}</div>
      <div className={`text-lg font-bold tabular-nums ${toneClass}`}>{value}</div>
    </div>
  );
}

function StatusPill({ status }: { status: string }) {
  const cls = status === 'ok' || status === 'passed'
    ? 'text-[var(--color-green)] bg-green-500/10'
    : status === 'failed'
      ? 'text-[var(--color-red)] bg-red-500/10'
      : status === 'unsupported'
        ? 'text-[var(--color-yellow)] bg-yellow-500/10'
        : 'text-[var(--color-text2)] bg-[var(--color-surface2)]';
  return <span className={`px-1.5 py-0.5 rounded text-[10px] uppercase font-sans ${cls}`}>{status}</span>;
}

function shortID(id: string) {
  if (id.length <= 18) return id;
  return `${id.slice(0, 8)}...${id.slice(-6)}`;
}

function formatTime(value?: string) {
  if (!value) return '-';
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return '-';
  return d.toLocaleTimeString();
}

function formatDuration(ms?: number) {
  if (!ms) return '-';
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

async function readError(res: Response) {
  try {
    const body = await res.json();
    return body?.error?.message || body?.message || `HTTP ${res.status}`;
  } catch {
    return `HTTP ${res.status}`;
  }
}

async function readRegressionReplay(res: Response): Promise<RegressionRun | null> {
  const contentType = res.headers.get('content-type') || '';
  if (!contentType.includes('text/event-stream')) {
    return res.json() as Promise<RegressionRun>;
  }
  const reader = res.body?.getReader();
  if (!reader) return null;
  const decoder = new TextDecoder();
  let buffer = '';
  let result: RegressionRun | null = null;
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    const lines = buffer.split('\n');
    buffer = lines.pop() || '';
    for (const line of lines) {
      const trimmed = line.trim();
      if (!trimmed.startsWith('data: ')) continue;
      try {
        const event = JSON.parse(trimmed.slice(6)) as SSEEvent;
        if (event.type === 'regression_result') {
          result = event.data as RegressionRun;
        }
      } catch {
        // Ignore malformed stream fragments.
      }
    }
  }
  return result;
}
