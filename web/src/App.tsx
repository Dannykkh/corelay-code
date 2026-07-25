import { useState, useEffect, useCallback } from 'react';
import { ActivityBar } from './components/ActivityBar';
import { SidePanel } from './components/SidePanel';
import { Toast } from './components/Toast';
import { ChatPage } from './pages/Chat';
import { RoutesPage } from './pages/Routes';
import { CostsPage } from './pages/Costs';
import { KairosPage } from './pages/Kairos';
import { SettingsPage } from './pages/RuntimeSettings';
import { MemoryPage } from './pages/Memory';
import { TeamPage } from './pages/Team';
import { fetchJSON, putJSON } from './lib/api';
import { useToast } from './lib/toast';
import './lib/i18n';

interface ProjectInfo {
  path: string;
  name: string;
  type: string;
  framework: string;
  fileCount: number;
  active: boolean;
}

interface AppConfig {
  provider: string;
  model: string;
  routerEnabled?: boolean;
  workDir?: string;
}

interface FileResponse {
  content?: string;
}

function App() {
  const [page, setPage] = useState('chat');
  const [theme, setTheme] = useState<'dark' | 'light'>(() =>
    (localStorage.getItem('aniclew-theme') as 'dark' | 'light') || 'dark'
  );
  const [status, setStatus] = useState<AppConfig | null>(null);
  const [loadSessionId, setLoadSessionId] = useState<string | null>(null);
  const [projects, setProjects] = useState<ProjectInfo[]>([]);
  const [showProjectPicker, setShowProjectPicker] = useState(false);
  const [showModelPicker, setShowModelPicker] = useState(false);
  const [viewingFile, setViewingFile] = useState<{ path: string; content: string } | null>(null);
  const [editMode, setEditMode] = useState(false);
  const [editContent, setEditContent] = useState('');

  const activeProject = projects.find(p => p.active);
  const { toast, dismissToast } = useToast();

  const loadProjects = useCallback(async () => {
    try {
      const data = await fetchJSON<ProjectInfo[]>('/api/projects');
      setProjects(data);
    } catch { setProjects([]); }
  }, []);

  useEffect(() => {
    const load = async () => {
      try {
        const data = await fetchJSON<AppConfig>('/api/config');
        setStatus(data);
      } catch { setStatus(null); }
    };
    load();
    queueMicrotask(() => { void loadProjects(); });
    const interval = setInterval(load, 15000);
    return () => clearInterval(interval);
  }, [loadProjects]);

  async function switchProject(path: string) {
    await putJSON('/api/workspace', { path });
    await loadProjects();
    setShowProjectPicker(false);
  }

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme);
    localStorage.setItem('aniclew-theme', theme);
  }, [theme]);

  // Side panel visible for chat and files mode
  const showSidePanel = page === 'chat' || page === 'files';
  const sidePanelMode = page === 'files' ? 'files' : 'chat';


  return (
    <div className="flex h-screen w-full">
      {/* Toast notifications */}
      {toast && <Toast message={toast.message} type={toast.type} onDismiss={dismissToast} />}

      {/* Activity Bar (always visible) */}
      <ActivityBar
        active={page}
        onNavigate={setPage}
        onThemeToggle={() => setTheme(theme === 'dark' ? 'light' : 'dark')}
        theme={theme}
      />

      {/* Side Panel (for chat + files) */}
      {showSidePanel && (
        <SidePanel
          visible={true}
          mode={sidePanelMode as 'files' | 'chat'}
          onNewChat={() => setLoadSessionId('__new__')}
          onSessionClick={(id) => setLoadSessionId(id)}
          onProjectSwitch={() => loadProjects()}
          onFileClick={async (path) => {
            try {
              const data = await fetchJSON<FileResponse | string>(`/api/file?path=${encodeURIComponent(path)}`);
              setViewingFile({ path, content: typeof data === 'string' ? data : data.content || '' });
            } catch { setViewingFile({ path, content: 'Failed to load file' }); }
          }}
        />
      )}

      {/* Main Content */}
      <main className="flex-1 min-w-0 h-[calc(100vh-24px)] overflow-hidden flex">
        {page === 'chat' && <ChatPage loadSessionId={loadSessionId} onSessionLoaded={() => setLoadSessionId(null)} />}
        {page === 'files' && (
          viewingFile ? (
            <div className="flex flex-col h-full w-full">
              <div className="px-4 py-1.5 border-b border-[var(--color-border)] bg-[var(--color-surface)] flex items-center justify-between">
                <div className="flex items-center gap-2">
                  <div className="text-xs font-mono text-[var(--color-accent)]">{viewingFile.path}</div>
                  {editMode && <span className="text-[9px] bg-yellow-500/20 text-yellow-400 px-1.5 py-0.5 rounded">EDITING</span>}
                </div>
                <div className="flex items-center gap-2">
                  {editMode ? (
                    <>
                      <button
                        onClick={async () => {
                          await fetch('/api/file/write', {
                            method: 'POST',
                            headers: { 'Content-Type': 'application/json' },
                            body: JSON.stringify({ path: viewingFile.path, content: editContent }),
                          });
                          setViewingFile({ ...viewingFile, content: editContent });
                          setEditMode(false);
                        }}
                        className="text-xs px-2 py-0.5 bg-[var(--color-green)] text-white rounded hover:opacity-80"
                      >Save</button>
                      <button onClick={() => { setEditMode(false); setEditContent(''); }} className="text-xs text-[var(--color-text2)] hover:text-[var(--color-text)]">Cancel</button>
                    </>
                  ) : (
                    <button
                      onClick={() => { setEditMode(true); setEditContent(viewingFile.content); }}
                      className="text-xs px-2 py-0.5 bg-[var(--color-surface2)] text-[var(--color-text)] rounded hover:bg-[var(--color-border)]"
                    >Edit</button>
                  )}
                  <button onClick={() => { setViewingFile(null); setEditMode(false); }} className="text-xs text-[var(--color-text2)] hover:text-[var(--color-text)]">Close</button>
                </div>
              </div>
              {editMode ? (
                <textarea
                  value={editContent}
                  onChange={(e) => setEditContent(e.target.value)}
                  className="flex-1 p-4 text-xs font-mono text-[var(--color-text)] bg-[var(--color-bg)] leading-relaxed resize-none focus:outline-none"
                  spellCheck={false}
                />
              ) : (
                <pre className="flex-1 overflow-auto p-4 text-xs font-mono text-[var(--color-text)] bg-[var(--color-bg)] leading-relaxed whitespace-pre-wrap">{viewingFile.content}</pre>
              )}
            </div>
          ) : (
            <ChatPage loadSessionId={loadSessionId} onSessionLoaded={() => setLoadSessionId(null)} />
          )
        )}
        {page === 'routes' && <RoutesPage />}
        {page === 'costs' && <CostsPage />}
        {page === 'kairos' && <KairosPage />}
        {page === 'settings' && (
          <div className="overflow-y-auto h-full w-full">
            <SettingsPage />
          </div>
        )}
        {page === 'memory' && (
          <div className="overflow-y-auto h-full w-full">
            <MemoryPage />
          </div>
        )}
        {page === 'team' && (
          <div className="overflow-y-auto h-full w-full">
            <TeamPage />
          </div>
        )}
      </main>

      {/* Status Bar */}
      <div className="fixed bottom-0 left-12 right-0 h-6 bg-[var(--color-surface)] border-t border-[var(--color-border)] flex items-center px-3 text-[10px] text-[var(--color-text2)] gap-4 z-50">
        <div className="relative">
          <button
            onClick={() => setShowModelPicker(!showModelPicker)}
            className="flex items-center gap-1.5 px-1.5 py-0.5 rounded hover:bg-[var(--color-surface2)] transition-colors"
          >
            <div className={`w-1.5 h-1.5 rounded-full ${status ? 'bg-[var(--color-green)]' : 'bg-[var(--color-red)]'}`} />
            {status ? `${status.provider} / ${status.model}` : 'Offline'}
            <span className="text-[8px]">▾</span>
          </button>
          {showModelPicker && (
            <div className="absolute bottom-6 left-0 bg-[var(--color-surface)] border border-[var(--color-border)] rounded-lg shadow-lg min-w-56 max-h-60 overflow-y-auto z-50">
              {[
                { provider: 'ollama', models: ['qwen3:8b', 'qwen3:14b', 'qwen3:32b'] },
                { provider: 'openai', models: ['gpt-4o-mini', 'gpt-4o', 'o4-mini'] },
                { provider: 'anthropic', models: ['claude-sonnet-4-6-20250217', 'claude-opus-4-6-20250205'] },
                { provider: 'gemini', models: ['gemini-2.5-flash', 'gemini-2.5-pro'] },
              ].map(group => (
                <div key={group.provider}>
                  <div className="px-3 py-1 text-[9px] text-[var(--color-text2)] uppercase">{group.provider}</div>
                  {group.models.map(m => (
                    <button
                      key={m}
                      onClick={async () => {
                        await putJSON('/api/config', { provider: group.provider, model: m });
                        const cfg = await fetchJSON<AppConfig>('/api/config');
                        setStatus(cfg);
                        setShowModelPicker(false);
                      }}
                      className={`w-full text-left px-3 py-1 text-[11px] hover:bg-[var(--color-surface2)] ${status?.model === m ? 'text-[var(--color-accent)]' : ''}`}
                    >
                      {m}
                    </button>
                  ))}
                </div>
              ))}
            </div>
          )}
        </div>
        {status?.routerEnabled && <span>Router ON</span>}

        {/* Project Picker in Status Bar */}
        <div className="relative">
          <button
            onClick={() => setShowProjectPicker(!showProjectPicker)}
            className="flex items-center gap-1 px-1.5 py-0.5 rounded hover:bg-[var(--color-surface2)] transition-colors"
          >
            <span>{activeProject ? `${activeProject.name}` : 'No Project'}</span>
            <span>{showProjectPicker ? '▴' : '▾'}</span>
          </button>
          {showProjectPicker && projects.length > 0 && (
            <div className="absolute bottom-6 left-0 bg-[var(--color-surface)] border border-[var(--color-border)] rounded-lg shadow-lg min-w-48 max-h-60 overflow-y-auto z-50">
              {projects.map(p => (
                <button
                  key={p.path}
                  onClick={() => switchProject(p.path)}
                  className={`w-full text-left px-3 py-1.5 text-[11px] hover:bg-[var(--color-surface2)] transition-colors flex items-center gap-2 ${p.active ? 'text-[var(--color-accent)]' : ''}`}
                >
                  <span>{p.active ? '●' : '○'}</span>
                  <span className="truncate">{p.name}</span>
                  <span className="text-[9px] text-[var(--color-text2)] ml-auto">{p.type}</span>
                </button>
              ))}
            </div>
          )}
        </div>

        <div className="ml-auto">AniClew v1.0</div>
      </div>
    </div>
  );
}

export default App;
