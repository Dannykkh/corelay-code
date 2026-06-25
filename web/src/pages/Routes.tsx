import { useState, useEffect } from 'react';
import { fetchJSON, putJSON, type ProviderInfo, type RouteRule } from '../lib/api';
import { t, getLang } from '../lib/i18n';

const tierColor: Record<string, string> = {
  ollama: 'bg-green-500/15 text-green-400',
  openai: 'bg-blue-500/15 text-blue-400',
  anthropic: 'bg-orange-500/15 text-orange-400',
  gemini: 'bg-yellow-500/15 text-yellow-400',
  groq: 'bg-purple-500/15 text-purple-400',
};

// Role descriptions: Korean / English
const roleDesc: Record<string, { ko: string; en: string; icon: string }> = {
  'bash-only':       { ko: '터미널 명령만 실행', en: 'Terminal commands only', icon: '⌨️' },
  'file-read':       { ko: '파일 읽기 (조회만)', en: 'Read files (view only)', icon: '📖' },
  'file-edit':       { ko: '파일 하나 수정', en: 'Edit a single file', icon: '✏️' },
  'multi-file-edit':  { ko: '여러 파일 동시 수정', en: 'Edit multiple files at once', icon: '📝' },
  'agent-spawn':     { ko: '하위 에이전트 생성', en: 'Spawn sub-agent', icon: '🤖' },
  'explain':         { ko: '코드 설명 / 질문 답변', en: 'Explain code / Answer questions', icon: '💡' },
  'generate':        { ko: '새 코드 작성', en: 'Generate new code', icon: '🆕' },
  'refactor':        { ko: '코드 리팩토링 / 구조 개선', en: 'Refactor / improve structure', icon: '♻️' },
  'debug':           { ko: '버그 수정 / 에러 해결', en: 'Fix bugs / resolve errors', icon: '🐛' },
  'review':          { ko: '코드 리뷰 / 검토', en: 'Code review / audit', icon: '🔍' },
  'test':            { ko: '테스트 코드 작성', en: 'Write test code', icon: '🧪' },
  'commit':          { ko: '커밋 메시지 작성', en: 'Write commit message', icon: '📦' },
  'short-context':   { ko: '짧은 대화 (2K 토큰 이하)', en: 'Short conversation (<2K tokens)', icon: '💬' },
  'medium-context':  { ko: '보통 길이 대화', en: 'Medium conversation', icon: '📄' },
  'long-context':    { ko: '긴 대화 (50K 토큰 이상)', en: 'Long conversation (>50K tokens)', icon: '📚' },
  'default':         { ko: '기본값 (분류 안 될 때)', en: 'Default (when no match)', icon: '⚡' },
};

export function RoutesPage() {
  const [rules, setRules] = useState<RouteRule[]>([]);
  const [providers, setProviders] = useState<ProviderInfo[]>([]);
  const [editing, setEditing] = useState<string | null>(null);
  const [editProvider, setEditProvider] = useState('');
  const [editModel, setEditModel] = useState('');

  async function load() {
    const data = await fetchJSON<{ rules: RouteRule[] }>('/api/routes');
    setRules(data.rules || []);
  }

  async function save(role: string) {
    await putJSON('/api/routes', { role, provider: editProvider, model: editModel });
    setEditing(null);
    load();
  }

  useEffect(() => {
    queueMicrotask(() => { void load(); });
    fetchJSON<ProviderInfo[]>('/api/providers').then(setProviders);
  }, []);

  return (
    <div className="p-6 w-full">
      <h1 className="text-xl font-semibold mb-2">{t('routes.title')}</h1>
      <p className="text-sm text-[var(--color-text2)] mb-6">{t('routes.desc')}</p>

      <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl overflow-hidden">
        <table className="w-full">
          <thead>
            <tr className="bg-[var(--color-surface2)] text-[var(--color-text2)] text-xs uppercase">
              <th className="text-left px-4 py-3 w-80">{t('routes.role')}</th>
              <th className="text-left px-4 py-3">{t('routes.provider')}</th>
              <th className="text-left px-4 py-3">{t('routes.model')}</th>
              <th className="text-left px-4 py-3">{t('routes.fallback')}</th>
              <th className="px-4 py-3 w-16"></th>
            </tr>
          </thead>
          <tbody>
            {rules.map((r) => {
              const desc = roleDesc[r.role] || { ko: '', en: '', icon: '❓' };

              if (editing === r.role) {
                return (
                  <tr key={r.role} className="border-t border-[var(--color-border)]">
                    <td className="px-4 py-2">
                      <div className="flex items-center gap-2">
                        <span>{desc.icon}</span>
                        <span className="text-xs font-mono bg-[var(--color-surface2)] px-2 py-1 rounded">{r.role}</span>
                      </div>
                    </td>
                    <td className="px-4 py-2">
                      <select value={editProvider} onChange={(e) => setEditProvider(e.target.value)}
                        className="bg-[var(--color-bg)] border border-[var(--color-border)] rounded px-2 py-1 text-xs text-[var(--color-text)]">
                        {providers.map((p) => <option key={p.name} value={p.name}>{p.name}</option>)}
                      </select>
                    </td>
                    <td className="px-4 py-2">
                      <input value={editModel} onChange={(e) => setEditModel(e.target.value)}
                        className="bg-[var(--color-bg)] border border-[var(--color-border)] rounded px-2 py-1 text-xs text-[var(--color-text)] w-48" />
                    </td>
                    <td className="px-4 py-2 text-xs text-[var(--color-text2)]">—</td>
                    <td className="px-4 py-2 flex gap-1">
                      <button onClick={() => save(r.role)} className="px-2 py-1 bg-[var(--color-accent)] text-white rounded text-xs">Save</button>
                      <button onClick={() => setEditing(null)} className="px-2 py-1 text-[var(--color-text2)] text-xs">X</button>
                    </td>
                  </tr>
                );
              }

              return (
                <tr key={r.role} className="border-t border-[var(--color-border)] hover:bg-[var(--color-accent)]/5">
                  <td className="px-4 py-3">
                    <div className="flex items-start gap-2.5">
                      <span className="text-base mt-0.5">{desc.icon}</span>
                      <div>
                        <span className={`text-xs font-semibold px-2 py-0.5 rounded-full ${tierColor[r.provider] || 'bg-gray-500/15 text-gray-400'}`}>
                          {r.role}
                        </span>
                        <div className="text-xs text-[var(--color-text)] mt-1">{getLang() === 'ko' ? desc.ko : desc.en}</div>
                        {getLang() === 'ko' && <div className="text-[10px] text-[var(--color-text2)]">{desc.en}</div>}
                      </div>
                    </div>
                  </td>
                  <td className="px-4 py-3 text-sm">{r.provider}</td>
                  <td className="px-4 py-3 text-sm font-mono text-xs">{r.model}</td>
                  <td className="px-4 py-3 text-xs text-[var(--color-text2)]">
                    {r.fallback ? `${r.fallback.provider}/${r.fallback.model}` : '—'}
                  </td>
                  <td className="px-4 py-3">
                    <button onClick={() => { setEditing(r.role); setEditProvider(r.provider); setEditModel(r.model); }}
                      className="px-2 py-1 border border-[var(--color-border)] rounded text-xs hover:border-[var(--color-accent)] hover:text-[var(--color-accent)]">
                      Edit
                    </button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}
