import { useEffect, useState } from 'react';
import { fetchJSON, postJSON } from '@/lib/api';
import { Copy, Check, Plus } from 'lucide-react';

interface GatewayUser {
  id: string;
  name: string;
  role: string;
  monthlyBudget: number;
  currentSpend: number;
  token: string;
}

interface AuditEntry {
  time: string;
  userId: string;
  provider: string;
  model: string;
  cost: number;
}

export function GatewaySettings() {
  const [users, setUsers] = useState<GatewayUser[]>([]);
  const [audit, setAudit] = useState<AuditEntry[]>([]);
  const [newUser, setNewUser] = useState({ name: '', role: 'developer', budget: 50 });
  const [copied, setCopied] = useState('');

  const load = async () => {
    const [u, a] = await Promise.all([
      fetchJSON<GatewayUser[]>('/api/gateway/users').catch(() => []),
      fetchJSON<AuditEntry[]>('/api/gateway/audit').catch(() => []),
    ]);
    setUsers(Array.isArray(u) ? u : []);
    setAudit(Array.isArray(a) ? a : []);
  };

  useEffect(() => {
    queueMicrotask(() => {
      void load();
    });
  }, []);

  async function addUser() {
    if (!newUser.name) return;
    await postJSON('/api/gateway/users', newUser);
    setNewUser({ name: '', role: 'developer', budget: 50 });
    void load();
  }

  function copyToken(token: string) {
    navigator.clipboard.writeText(token);
    setCopied(token);
    setTimeout(() => setCopied(''), 2000);
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold text-[var(--color-text)]">Team Gateway & API Quotas</h2>
        <p className="text-xs text-[var(--color-text2)] mt-1">
          Manage team member access tokens, monthly spend limits, and provider routing budgets.
        </p>
      </div>

      {/* Add User Form */}
      <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-4">
        <div className="text-xs font-semibold text-[var(--color-text)] mb-3 flex items-center gap-1.5">
          <Plus className="w-3.5 h-3.5 text-[var(--color-accent)]" />
          <span>Add Team Member</span>
        </div>
        <div className="flex flex-wrap gap-2.5">
          <input
            value={newUser.name}
            onChange={(e) => setNewUser({ ...newUser, name: e.target.value })}
            placeholder="Member name"
            className="flex-1 min-w-[140px] bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg px-3 py-1.5 text-xs text-[var(--color-text)]"
          />
          <select
            value={newUser.role}
            onChange={(e) => setNewUser({ ...newUser, role: e.target.value })}
            className="bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg px-3 py-1.5 text-xs text-[var(--color-text)]"
          >
            <option value="admin">Admin</option>
            <option value="developer">Developer</option>
            <option value="viewer">Viewer</option>
          </select>
          <input
            type="number"
            value={newUser.budget}
            onChange={(e) => setNewUser({ ...newUser, budget: +e.target.value })}
            placeholder="Monthly budget ($)"
            className="w-32 bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg px-3 py-1.5 text-xs text-[var(--color-text)]"
          />
          <button
            type="button"
            onClick={() => void addUser()}
            className="px-3.5 py-1.5 bg-[var(--color-accent)] text-white font-medium rounded-lg text-xs hover:opacity-85"
          >
            Add Member
          </button>
        </div>
      </div>

      {/* Users Table */}
      <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl overflow-hidden">
        <table className="w-full text-left">
          <thead>
            <tr className="bg-[var(--color-surface2)] text-[var(--color-text2)] text-[11px] uppercase border-b border-[var(--color-border)]">
              <th className="px-4 py-2 font-semibold">Name</th>
              <th className="px-4 py-2 font-semibold">Role</th>
              <th className="px-4 py-2 font-semibold">Budget</th>
              <th className="px-4 py-2 font-semibold">Spent</th>
              <th className="px-4 py-2 font-semibold">Token</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-[var(--color-border)] text-xs">
            {users.length === 0 ? (
              <tr>
                <td colSpan={5} className="text-center py-6 text-[var(--color-text2)]">
                  No team members configured yet
                </td>
              </tr>
            ) : (
              users.map((u) => (
                <tr key={u.id} className="hover:bg-[var(--color-surface2)]/40 transition-colors">
                  <td className="px-4 py-2.5 font-medium text-[var(--color-text)]">{u.name}</td>
                  <td className="px-4 py-2.5">
                    <span
                      className={`text-[10px] px-2 py-0.5 rounded-full font-medium ${
                        u.role === 'admin'
                          ? 'bg-[var(--color-warning)]/15 text-[var(--color-warning)]'
                          : u.role === 'developer'
                          ? 'bg-[var(--color-accent)]/15 text-[var(--color-accent)]'
                          : 'bg-[var(--color-surface2)] text-[var(--color-text2)]'
                      }`}
                    >
                      {u.role}
                    </span>
                  </td>
                  <td className="px-4 py-2.5 font-mono text-[var(--color-text2)]">${u.monthlyBudget}</td>
                  <td className="px-4 py-2.5 font-mono text-[var(--color-text2)]">${u.currentSpend.toFixed(2)}</td>
                  <td className="px-4 py-2.5">
                    <button
                      type="button"
                      onClick={() => copyToken(u.token)}
                      className="inline-flex items-center gap-1 font-mono text-[11px] text-[var(--color-accent)] hover:underline"
                    >
                      {copied === u.token ? (
                        <>
                          <Check className="w-3 h-3 text-[var(--color-green)]" />
                          <span>Copied</span>
                        </>
                      ) : (
                        <>
                          <Copy className="w-3 h-3" />
                          <span>{u.token ? `${u.token.slice(0, 10)}…` : 'None'}</span>
                        </>
                      )}
                    </button>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>

      {/* Audit Log */}
      {audit.length > 0 && (
        <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl overflow-hidden">
          <div className="px-4 py-2.5 bg-[var(--color-surface2)] border-b border-[var(--color-border)] text-xs font-semibold text-[var(--color-text)]">
            Gateway Audit Log
          </div>
          <div className="divide-y divide-[var(--color-border)] text-xs font-mono max-h-48 overflow-y-auto">
            {audit.map((a, i) => (
              <div key={i} className="px-4 py-2 flex items-center justify-between text-[var(--color-text2)]">
                <span>{new Date(a.time).toLocaleTimeString()} · user:{a.userId} · {a.provider}/{a.model}</span>
                <span className="text-[var(--color-text)]">${a.cost.toFixed(4)}</span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}
