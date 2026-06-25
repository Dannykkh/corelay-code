import { useState, useEffect } from 'react';
import { fetchJSON, postJSON } from '../lib/api';

interface MemoryEntry {
  key: string;
  value: string;
  category: string;
  source?: string;
}

interface MemoryState {
  entries?: MemoryEntry[];
  totalSize?: number;
  sessionCount?: number;
}

export function MemoryPage() {
  const [state, setState] = useState<MemoryState | null>(null);
  const [searchQ, setSearchQ] = useState('');
  const [results, setResults] = useState<MemoryEntry[]>([]);
  const [newEntry, setNewEntry] = useState({ key: '', value: '', category: 'project' });

  async function load() {
    const s = await fetchJSON<MemoryState>('/api/memory');
    setState(s);
  }

  async function search() {
    const r = await fetchJSON<MemoryEntry[]>(`/api/memory/search?q=${encodeURIComponent(searchQ)}`);
    setResults(r || []);
  }

  async function add() {
    if (!newEntry.key || !newEntry.value) return;
    await postJSON('/api/memory', { ...newEntry, source: 'user' });
    setNewEntry({ key: '', value: '', category: 'project' });
    load();
  }

  async function dream() {
    await postJSON('/api/memory/dream', {});
    load();
  }

  useEffect(() => { queueMicrotask(() => { void load(); }); }, []);

  return (
    <div className="p-6 w-full">
      <div className="flex items-center justify-between mb-2">
        <h1 className="text-xl font-semibold">Memory</h1>
        <button onClick={dream} className="px-4 py-2 bg-purple-600 text-white rounded-lg text-sm">
          Run Dream Cycle
        </button>
      </div>
      <p className="text-sm text-[var(--color-text2)] mb-6">Project knowledge that persists across sessions. Auto-consolidates after 5 conversations.</p>

      {/* Stats */}
      <div className="grid grid-cols-3 gap-4 mb-6">
        <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4">
          <div className="text-xs text-[var(--color-text2)] uppercase mb-1">Entries</div>
          <div className="text-2xl font-bold">{state?.entries?.length || 0}</div>
        </div>
        <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4">
          <div className="text-xs text-[var(--color-text2)] uppercase mb-1">Size</div>
          <div className="text-2xl font-bold">{((state?.totalSize || 0) / 1024).toFixed(1)} KB</div>
          <div className="text-xs text-[var(--color-text2)]">/ 25 KB max</div>
        </div>
        <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4">
          <div className="text-xs text-[var(--color-text2)] uppercase mb-1">Sessions</div>
          <div className="text-2xl font-bold">{state?.sessionCount || 0}</div>
          <div className="text-xs text-[var(--color-text2)]">Dream triggers at 5</div>
        </div>
      </div>

      {/* Search */}
      <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4 mb-4">
        <div className="flex gap-2">
          <input value={searchQ} onChange={(e) => setSearchQ(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && search()}
            placeholder="Search memory..." className="flex-1 bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg px-3 py-2 text-sm text-[var(--color-text)]" />
          <button onClick={search} className="px-4 py-2 bg-[var(--color-accent)] text-white rounded-lg text-sm">Search</button>
        </div>
        {results.length > 0 && (
          <div className="mt-3 space-y-2">
            {results.map((r, i) => (
              <div key={i} className="bg-[var(--color-bg)] rounded-lg p-3">
                <div className="flex items-center gap-2 mb-1">
                  <span className="text-xs font-mono text-[var(--color-accent)]">{r.key}</span>
                  <span className="text-[10px] bg-[var(--color-surface2)] px-1.5 py-0.5 rounded">{r.category}</span>
                </div>
                <div className="text-sm">{r.value}</div>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Add Entry */}
      <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4 mb-4">
        <div className="text-xs text-[var(--color-text2)] uppercase mb-3">Add Memory</div>
        <div className="flex gap-2 mb-2">
          <input value={newEntry.key} onChange={(e) => setNewEntry({ ...newEntry, key: e.target.value })}
            placeholder="Key" className="w-40 bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg px-3 py-2 text-sm text-[var(--color-text)]" />
          <select value={newEntry.category} onChange={(e) => setNewEntry({ ...newEntry, category: e.target.value })}
            className="bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg px-3 py-2 text-sm text-[var(--color-text)]">
            <option value="project">Project</option>
            <option value="pattern">Pattern</option>
            <option value="gotcha">Gotcha</option>
            <option value="preference">Preference</option>
          </select>
        </div>
        <div className="flex gap-2">
          <input value={newEntry.value} onChange={(e) => setNewEntry({ ...newEntry, value: e.target.value })}
            placeholder="Value" className="flex-1 bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg px-3 py-2 text-sm text-[var(--color-text)]" />
          <button onClick={add} className="px-4 py-2 bg-[var(--color-accent)] text-white rounded-lg text-sm">Add</button>
        </div>
      </div>

      {/* Entries */}
      {(state?.entries?.length || 0) > 0 && (
        <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl overflow-hidden">
          <table className="w-full">
            <thead>
              <tr className="bg-[var(--color-surface2)] text-[var(--color-text2)] text-xs uppercase">
                <th className="text-left px-4 py-2">Key</th>
                <th className="text-left px-4 py-2">Value</th>
                <th className="text-left px-4 py-2">Category</th>
                <th className="text-left px-4 py-2">Source</th>
              </tr>
            </thead>
            <tbody>
              {(state?.entries || []).map((e, i) => (
                <tr key={i} className="border-t border-[var(--color-border)]">
                  <td className="px-4 py-2 text-sm font-mono text-[var(--color-accent)]">{e.key}</td>
                  <td className="px-4 py-2 text-sm max-w-md truncate">{e.value}</td>
                  <td className="px-4 py-2 text-xs">{e.category}</td>
                  <td className="px-4 py-2 text-xs text-[var(--color-text2)]">{e.source}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
