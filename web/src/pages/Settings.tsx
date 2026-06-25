import { useState, useEffect } from 'react';
import { fetchJSON, putJSON, type ProviderInfo } from '../lib/api';
import { t } from '../lib/i18n';

interface AppConfig {
  provider: string;
  model: string;
  responseLang?: string;
}

export function SettingsPage() {
  const [providers, setProviders] = useState<ProviderInfo[]>([]);
  const [config, setConfig] = useState<AppConfig | null>(null);
  const [selProvider, setSelProvider] = useState('');
  const [selModel, setSelModel] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [saved, setSaved] = useState(false);
  const [showAdvanced, setShowAdvanced] = useState(false);
  const [responseLang, setResponseLang] = useState('auto');
  // Models actually installed in the local Ollama (via /api/ollama/models), so
  // we offer real choices instead of a hardcoded guess that may not be pulled.
  const [ollamaModels, setOllamaModels] = useState<{ id: string; displayName: string }[]>([]);

  useEffect(() => {
    fetchJSON<ProviderInfo[]>('/api/providers').then(setProviders);
    fetchJSON<AppConfig>('/api/config').then((c) => {
      setConfig(c);
      setSelProvider(c.provider);
      setSelModel(c.model);
      setResponseLang(c.responseLang || 'auto');
    });
    fetchJSON<{ models: { id: string; displayName: string }[] }>('/api/ollama/models')
      .then((r) => setOllamaModels(r.models || []))
      .catch(() => setOllamaModels([]));
  }, []);

  // For Ollama, prefer the live installed list; fall back to the static list
  // only if Ollama is unreachable. Cloud providers keep their curated list.
  const models = selProvider === 'ollama' && ollamaModels.length > 0
    ? ollamaModels
    : (providers.find((p) => p.name === selProvider)?.models || []);
  const isCloud = selProvider && selProvider !== 'ollama';
  // Smallest installed model is the safest fast default for the Quick Start
  // "Local (Ollama)" button (backend already sorts smallest-first).
  const defaultOllamaModel = ollamaModels[0]?.id || 'qwen3:8b';

  async function quickStart(provider: string, model: string) {
    await putJSON('/api/config', { provider, model });
    setConfig((c) => c ? { ...c, provider, model } : c);
    setSelProvider(provider);
    setSelModel(model);
    setSaved(true);
    setTimeout(() => setSaved(false), 2000);
  }

  async function applyWithKey() {
    if (apiKey) {
      await fetchJSON('/api/providers/register', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: selProvider, apiKey }),
      });
    }
    await putJSON('/api/config', { provider: selProvider, model: selModel });
    setConfig((c) => c ? { ...c, provider: selProvider, model: selModel } : c);
    setSaved(true);
    setTimeout(() => setSaved(false), 2000);
  }

  return (
    <div className="p-6 w-full overflow-y-auto h-full">

      {/* Current Status */}
      {config && (
        <div className="mb-6 px-4 py-3 bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl flex items-center gap-3">
          <div className="w-2.5 h-2.5 rounded-full bg-[var(--color-green)]" />
          <div>
            <div className="text-sm font-semibold">{config.provider} / {config.model}</div>
            <div className="text-[10px] text-[var(--color-text2)]">{t('settings.title')}</div>
          </div>
          {saved && <span className="ml-auto text-xs text-[var(--color-green)]">Saved!</span>}
        </div>
      )}

      {/* Quick Start — One Click */}
      <div className="mb-6">
        <h2 className="text-lg font-semibold mb-3">Quick Start</h2>
        <div className="grid grid-cols-3 gap-3">
          <button
            onClick={() => { if (ollamaModels.length > 0) quickStart('ollama', defaultOllamaModel); }}
            className={`p-4 rounded-xl border text-left transition-all hover:border-[var(--color-accent)] ${config?.provider === 'ollama' ? 'border-[var(--color-accent)] bg-[var(--color-accent)]/10' : 'border-[var(--color-border)] bg-[var(--color-surface)]'}`}
          >
            <div className="text-2xl mb-2">🏠</div>
            <div className="text-sm font-semibold">Local (Ollama)</div>
            <div className="text-[10px] text-[var(--color-text2)] mt-1">Free, private, no API key</div>
            <div className="text-[9px] text-[var(--color-accent)] mt-2">
              {ollamaModels.length > 0 ? `${defaultOllamaModel} (installed)` : 'no model installed'}
            </div>
          </button>

          <button
            onClick={() => { setSelProvider('openai'); setSelModel('gpt-5.5-mini'); setShowAdvanced(false); }}
            className={`p-4 rounded-xl border text-left transition-all hover:border-[var(--color-accent)] ${config?.provider === 'openai' ? 'border-[var(--color-accent)] bg-[var(--color-accent)]/10' : 'border-[var(--color-border)] bg-[var(--color-surface)]'}`}
          >
            <div className="text-2xl mb-2">🟢</div>
            <div className="text-sm font-semibold">OpenAI</div>
            <div className="text-[10px] text-[var(--color-text2)] mt-1">API key needed</div>
            <div className="text-[9px] text-[var(--color-accent)] mt-2">GPT-5.5 / o4</div>
          </button>

          <button
            onClick={() => { setSelProvider('anthropic'); setSelModel('claude-sonnet-4-6'); setShowAdvanced(false); }}
            className={`p-4 rounded-xl border text-left transition-all hover:border-[var(--color-accent)] ${config?.provider === 'anthropic' ? 'border-[var(--color-accent)] bg-[var(--color-accent)]/10' : 'border-[var(--color-border)] bg-[var(--color-surface)]'}`}
          >
            <div className="text-2xl mb-2">🟠</div>
            <div className="text-sm font-semibold">Anthropic</div>
            <div className="text-[10px] text-[var(--color-text2)] mt-1">API key needed</div>
            <div className="text-[9px] text-[var(--color-accent)] mt-2">Claude Opus 4.8 / Sonnet 4.6</div>
          </button>
        </div>
      </div>

      {/* API Key + Model Selection (shown when cloud provider selected) */}
      {isCloud && (
        <div className="mb-6 bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-5">
          <h3 className="text-sm font-semibold mb-3">{selProvider} Setup</h3>

          <div className="mb-3">
            <label className="block text-xs text-[var(--color-text2)] mb-1">API Key</label>
            <input
              type="password"
              value={apiKey}
              onChange={(e) => setApiKey(e.target.value)}
              placeholder={`Enter your ${selProvider} API key`}
              className="w-full bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg px-3 py-2.5 text-sm text-[var(--color-text)] font-mono"
            />
          </div>

          <div className="mb-4">
            <label className="block text-xs text-[var(--color-text2)] mb-1">Model</label>
            <select
              value={selModel}
              onChange={(e) => setSelModel(e.target.value)}
              className="w-full bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg px-3 py-2.5 text-sm text-[var(--color-text)]"
            >
              <option value="">Select model...</option>
              {models.map((m) => (
                <option key={m.id} value={m.id}>{m.displayName}</option>
              ))}
            </select>
          </div>

          <button
            onClick={applyWithKey}
            disabled={!selModel}
            className="w-full py-2.5 bg-[var(--color-accent)] text-white rounded-lg text-sm font-medium disabled:opacity-40 hover:opacity-90"
          >
            Start with {selProvider}
          </button>
        </div>
      )}

      {/* Language */}
      <div className="mb-6 bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl p-5">
        <h3 className="text-sm font-semibold mb-3">{t('settings.language')}</h3>
        <div className="flex gap-2 flex-wrap">
          {[
            { id: 'auto', label: '🌐 Auto' },
            { id: 'ko', label: '🇰🇷 한국어' },
            { id: 'en', label: '🇺🇸 English' },
            { id: 'ja', label: '🇯🇵 日本語' },
            { id: 'zh', label: '🇨🇳 中文' },
          ].map((lang) => (
            <button
              key={lang.id}
              onClick={async () => {
                setResponseLang(lang.id);
                await putJSON('/api/config', { responseLang: lang.id });
              }}
              className={`px-4 py-2 rounded-lg text-sm ${
                responseLang === lang.id
                  ? 'bg-[var(--color-accent)] text-white'
                  : 'bg-[var(--color-surface2)] text-[var(--color-text2)] hover:text-[var(--color-text)]'
              }`}
            >
              {lang.label}
            </button>
          ))}
        </div>
      </div>

      {/* Advanced Settings (collapsed) */}
      <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl">
        <button
          onClick={() => setShowAdvanced(!showAdvanced)}
          className="w-full px-5 py-3 text-left flex items-center justify-between text-sm"
        >
          <span className="font-medium text-[var(--color-text2)]">Advanced Settings</span>
          <span className="text-[var(--color-text2)]">{showAdvanced ? '▴' : '▾'}</span>
        </button>

        {showAdvanced && (
          <div className="px-5 pb-5 border-t border-[var(--color-border)] pt-4 space-y-4">
            {/* Provider/Model manual select */}
            <div>
              <label className="block text-xs text-[var(--color-text2)] mb-1">Provider</label>
              <select value={selProvider} onChange={(e) => { setSelProvider(e.target.value); setSelModel(''); }}
                className="w-full bg-[var(--color-bg)] border border-[var(--color-border)] rounded px-3 py-2 text-sm text-[var(--color-text)]">
                {providers.map(p => <option key={p.name} value={p.name}>{p.displayName}</option>)}
              </select>
            </div>
            <div>
              <label className="block text-xs text-[var(--color-text2)] mb-1">Model</label>
              <select value={selModel} onChange={(e) => setSelModel(e.target.value)}
                className="w-full bg-[var(--color-bg)] border border-[var(--color-border)] rounded px-3 py-2 text-sm text-[var(--color-text)]">
                <option value="">Select...</option>
                {models.map(m => <option key={m.id} value={m.id}>{m.displayName} ({m.id})</option>)}
              </select>
            </div>

            {/* CLI Connection */}
            <div>
              <div className="text-xs text-[var(--color-text2)] uppercase mb-2">CLI Connection (optional)</div>
              <div className="text-[10px] text-[var(--color-text2)] mb-2">CLI tools have their own login. These commands route them through AniClew for monitoring.</div>
              {['Claude CLI: ANTHROPIC_BASE_URL=http://localhost:4000 claude',
                'Codex CLI: OPENAI_BASE_URL=http://localhost:4000 codex',
                'Gemini CLI: GEMINI_BASE_URL=http://localhost:4000 gemini',
              ].map(cmd => (
                <div key={cmd} className="text-[10px] font-mono text-[var(--color-text2)] bg-[var(--color-bg)] rounded px-2 py-1 mb-1 cursor-pointer hover:text-[var(--color-text)]"
                  onClick={() => navigator.clipboard.writeText(cmd.split(': ')[1])}>
                  {cmd}
                </div>
              ))}
            </div>

            <button onClick={async () => {
              await putJSON('/api/config', { provider: selProvider, model: selModel });
              setSaved(true); setTimeout(() => setSaved(false), 2000);
            }} className="w-full py-2 bg-[var(--color-accent)] text-white rounded text-sm">
              Apply
            </button>
          </div>
        )}
      </div>
    </div>
  );
}
