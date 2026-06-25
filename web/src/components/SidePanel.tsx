import { useState, useEffect } from 'react';
import { fetchJSON, postJSON } from '../lib/api';
import { getLang } from '../lib/i18n';
import { listSessions, type SessionSummary } from '../lib/sessions';
import { showToast } from '../lib/toast';

// parentPath returns the parent of a filesystem path, handling Windows drive
// roots correctly: `D:/git` -> `D:/`, NOT `D:` (which Windows treats as the
// drive's current directory, not the drive root — exactly the bug that made
// the Up button jump to a "weird" location).
function parentPath(p: string): string {
  const norm = p.replace(/\\/g, '/').replace(/\/+$/, '');
  const idx = norm.lastIndexOf('/');
  if (idx < 0) return norm; // no separator left — already at top
  let parent = norm.slice(0, idx);
  if (/^[A-Za-z]:$/.test(parent)) parent += '/'; // Windows drive: keep the slash
  if (parent === '') return '/';                 // unix root
  return parent;
}

interface ProjectInfo {
  path: string;
  name: string;
  type: string;
  framework: string;
  fileCount: number;
  active: boolean;
}

function FileTreeNode({ node, depth, onFileClick }: { node: TreeNode; depth: number; onFileClick?: (path: string) => void }) {
  const [open, setOpen] = useState(false);
  const indent = depth * 12;

  if (node.isDir) {
    return (
      <div>
        <button
          onClick={() => setOpen(!open)}
          className="w-full text-left flex items-center gap-1 px-2 py-0.5 text-xs hover:bg-[var(--color-surface2)] transition-colors"
          style={{ paddingLeft: `${indent + 8}px` }}
        >
          <span className="text-[9px] w-3">{open ? '▾' : '▸'}</span>
          <span className="truncate">{node.name}</span>
        </button>
        {open && node.children?.map(child => (
          <FileTreeNode key={child.path} node={child} depth={depth + 1} onFileClick={onFileClick} />
        ))}
      </div>
    );
  }

  const ext = node.name.split('.').pop() || '';
  const extColors: Record<string, string> = {
    go: 'text-blue-400', ts: 'text-blue-300', tsx: 'text-blue-300',
    js: 'text-yellow-300', json: 'text-yellow-200', md: 'text-gray-400',
    py: 'text-green-300', rs: 'text-orange-300', css: 'text-pink-300',
  };

  return (
    <button
      onClick={() => onFileClick?.(node.path)}
      className="w-full text-left flex items-center gap-1 px-2 py-0.5 text-xs hover:bg-[var(--color-surface2)] transition-colors"
      style={{ paddingLeft: `${indent + 20}px` }}
    >
      <span className={`truncate ${extColors[ext] || ''}`}>{node.name}</span>
      {node.size !== undefined && node.size > 0 && (
        <span className="text-[9px] text-[var(--color-text2)] ml-auto shrink-0">
          {node.size > 1024 ? `${(node.size / 1024).toFixed(0)}K` : `${node.size}B`}
        </span>
      )}
    </button>
  );
}

interface Props {
  visible: boolean;
  mode: 'files' | 'chat';
  onFileClick?: (path: string) => void;
  onSessionClick?: (id: string) => void;
  onNewChat?: () => void;
  onProjectSwitch?: (path: string) => void;
}

interface TreeNode {
  name: string;
  path: string;
  isDir: boolean;
  size?: number;
  children?: TreeNode[];
}

interface WorkspaceResponse {
  path?: string;
}

interface BrowseEntry {
  name: string;
  isDir: boolean;
  isProject?: boolean;
}

interface BrowseResponse {
  entries?: BrowseEntry[];
  current?: string;
}

function errorMessage(err: unknown): string | undefined {
  return err instanceof Error ? err.message : undefined;
}

export function SidePanel({ visible, mode, onFileClick, onSessionClick, onNewChat, onProjectSwitch }: Props) {
  const [sessionSearch, setSessionSearch] = useState('');
  const [projects, setProjects] = useState<ProjectInfo[]>([]);
  const [sessions, setSessions] = useState<SessionSummary[]>([]);
  const [showProjectList, setShowProjectList] = useState(false);
  const [showAddProject, setShowAddProject] = useState(false);
  const [browseEntries, setBrowseEntries] = useState<BrowseEntry[]>([]);
  const [browsePath, setBrowsePath] = useState('');
  const [fileTree, setFileTree] = useState<TreeNode[]>([]);
  const ko = getLang() === 'ko';

  const activeProject = projects.find(p => p.active);

  async function loadSessions() {
    const ws = (await fetchJSON<WorkspaceResponse>('/api/workspace').catch(() => null))?.path;
    // Backend returns `null` (not `[]`) for a workspace with no sessions;
    // unwrapped that bypasses the [] default and makes render crash on
    // `sessions.length`. Coerce here defensively in addition to the backend fix.
    listSessions(ws || undefined).then((s) => setSessions(s || [])).catch(() => setSessions([]));
  }

  async function loadFileTree() {
    try {
      const data = await fetchJSON<TreeNode[]>('/api/tree');
      setFileTree(data || []);
    } catch { setFileTree([]); }
  }

  async function loadProjects() {
    try {
      const data = await fetchJSON<ProjectInfo[]>('/api/projects');
      setProjects(data);
    } catch { setProjects([]); }
  }

  async function switchProject(path: string) {
    try {
      await fetchJSON('/api/workspace', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ path }),
      });
      await loadProjects();
      loadSessions();
      loadFileTree();
      setShowProjectList(false);
      onProjectSwitch?.(path);
    } catch (e: unknown) {
      showToast(errorMessage(e) || (ko ? '프로젝트 전환 실패' : 'Failed to switch project'), 'error');
    }
  }

  async function addProject(path: string) {
    try {
      await postJSON('/api/projects', { path });
      await loadProjects();
      loadSessions();
      loadFileTree();
      setShowAddProject(false);
      setShowProjectList(false);
      onProjectSwitch?.(path);
      showToast(ko ? `프로젝트 추가됨: ${path}` : `Project added: ${path}`, 'success');
    } catch (e: unknown) {
      // Most common: 409 "Project already exists" → tell the user instead of
      // closing the menu silently like before.
      showToast(errorMessage(e) || (ko ? '프로젝트 추가 실패' : 'Failed to add project'), 'error');
    }
  }

  async function removeProject(path: string, e: React.MouseEvent) {
    e.stopPropagation();
    await fetchJSON(`/api/projects?path=${encodeURIComponent(path)}`, { method: 'DELETE' });
    await loadProjects();
  }

  async function loadBrowse(path: string) {
    try {
      const data = await fetchJSON<BrowseResponse>(`/api/browse?path=${encodeURIComponent(path)}`);
      setBrowseEntries(data.entries || []);
      setBrowsePath(data.current || path);
    } catch {
      setBrowseEntries([]);
      setBrowsePath(path);
    }
  }

  function openFolderBrowser() {
    setShowAddProject(true);
    // Start browsing from common locations
    const startPath = browsePath || 'D:/git';
    loadBrowse(startPath);
  }

  // Load projects + file tree
  useEffect(() => {
    queueMicrotask(() => {
      void loadProjects();
      void loadSessions();
      void loadFileTree();
    });
  }, []);

  if (!visible) return null;

  const projectTypes: Record<string, string> = {
    go: '🔵', node: '🟢', python: '🐍', rust: '🦀', java: '☕', dotnet: '🟣',
  };

  return (
    <div className="w-64 bg-[var(--color-surface)] border-r border-[var(--color-border)] flex flex-col h-[calc(100vh-24px)] overflow-hidden shrink-0">
      {/* Project Header — clickable dropdown */}
      <div className="border-b border-[var(--color-border)]">
        <button
          onClick={() => { setShowProjectList(!showProjectList); setShowAddProject(false); }}
          className="w-full p-3 text-left hover:bg-[var(--color-surface2)] transition-colors"
        >
          <div className="flex items-center gap-2">
            <span>{activeProject ? (projectTypes[activeProject.type] || '▸') : '▸'}</span>
            <div className="min-w-0 flex-1">
              <div className="text-xs font-semibold truncate">{activeProject?.name || (ko ? '프로젝트 선택' : 'Select Project')}</div>
              <div className="text-[10px] text-[var(--color-text2)] truncate">{activeProject?.path || ''}</div>
            </div>
            <span className="text-[10px] text-[var(--color-text2)]">{showProjectList ? '▴' : '▾'}</span>
          </div>
          {activeProject && (
            <div className="flex gap-1 mt-1.5">
              <span className="text-[9px] bg-[var(--color-accent)]/20 text-[var(--color-accent)] px-1 py-0.5 rounded">{activeProject.type}</span>
              {activeProject.framework && <span className="text-[9px] bg-[var(--color-green)]/20 text-[var(--color-green)] px-1 py-0.5 rounded">{activeProject.framework}</span>}
              <span className="text-[9px] text-[var(--color-text2)]">{activeProject.fileCount} files</span>
            </div>
          )}
        </button>

        {/* Project Dropdown */}
        {showProjectList && !showAddProject && (
          <div className="border-t border-[var(--color-border)] bg-[var(--color-bg)] max-h-60 overflow-y-auto">
            {projects.map(p => (
              <div
                key={p.path}
                onClick={() => switchProject(p.path)}
                className={`flex items-center gap-2 px-3 py-2 cursor-pointer hover:bg-[var(--color-surface2)] transition-colors ${p.active ? 'bg-[var(--color-accent)]/10' : ''}`}
              >
                <span className="text-[10px]">{projectTypes[p.type] || '▸'}</span>
                <div className="min-w-0 flex-1">
                  <div className="text-xs font-medium truncate">{p.name}</div>
                  <div className="text-[9px] text-[var(--color-text2)] truncate">{p.path}</div>
                </div>
                {p.active && <span className="text-[9px] text-[var(--color-accent)]">●</span>}
                <button
                  onClick={(e) => removeProject(p.path, e)}
                  className="text-[10px] text-[var(--color-text2)] hover:text-[var(--color-red)] px-1"
                  title={ko ? '제거' : 'Remove'}
                >
                  ✕
                </button>
              </div>
            ))}
            <button
              onClick={openFolderBrowser}
              className="w-full px-3 py-2 text-xs text-[var(--color-accent)] hover:bg-[var(--color-surface2)] transition-colors text-left flex items-center gap-1.5"
            >
              <span>+</span> {ko ? '프로젝트 추가' : 'Add Project'}
            </button>
          </div>
        )}

        {/* Folder Browser for Adding Project */}
        {showAddProject && (
          <div className="border-t border-[var(--color-border)] bg-[var(--color-bg)] max-h-80 overflow-y-auto">
            <div className="px-2 py-1.5 text-[10px] text-[var(--color-text2)] flex items-center justify-between border-b border-[var(--color-border)]">
              <button
                onClick={() => setShowAddProject(false)}
                className="text-[10px] hover:text-[var(--color-text)]"
              >
                ← {ko ? '뒤로' : 'Back'}
              </button>
              <span className="truncate ml-2">{browsePath}</span>
            </div>
            <div className="px-2 py-1 flex gap-1">
              <button
                onClick={() => loadBrowse(parentPath(browsePath))}
                className="text-[10px] px-1.5 py-0.5 rounded bg-[var(--color-surface2)] hover:bg-[var(--color-border)] transition-colors"
              >
                ↑ {ko ? '상위' : 'Up'}
              </button>
              <button
                onClick={() => addProject(browsePath)}
                className="text-[10px] px-1.5 py-0.5 rounded bg-[var(--color-accent)] text-white hover:opacity-80 transition-colors ml-auto"
              >
                {ko ? '이 폴더 추가' : 'Add This Folder'}
              </button>
            </div>
            <div className="py-1">
              {browseEntries.filter((e) => e.isDir).map((entry) => {
                const childPath = browsePath.replace(/\\/g, '/').replace(/\/+$/, '') + '/' + entry.name;
                return (
                  <div
                    key={entry.name}
                    onClick={() => loadBrowse(childPath)}
                    className="flex items-center gap-1.5 px-3 py-1 text-xs cursor-pointer hover:bg-[var(--color-surface2)] transition-colors group"
                  >
                    <span className="text-[10px]">▸</span>
                    <span className={`truncate flex-1 ${entry.isProject ? 'text-[var(--color-accent)]' : ''}`}>{entry.name}</span>
                    {entry.isProject && (
                      <span className="text-[9px] text-[var(--color-accent)] shrink-0" title={ko ? '감지된 프로젝트' : 'detected project'}>•</span>
                    )}
                    <button
                      onClick={(e) => { e.stopPropagation(); addProject(childPath); }}
                      className="ml-auto text-[9px] bg-[var(--color-accent)] text-white px-1.5 py-0.5 rounded hover:opacity-80 shrink-0 opacity-0 group-hover:opacity-100 transition-opacity"
                    >
                      + {ko ? '추가' : 'Add'}
                    </button>
                  </div>
                );
              })}
            </div>
          </div>
        )}
      </div>

      {mode === 'files' ? (
        /* File Tree */
        <div className="flex-1 overflow-y-auto">
          <div className="px-2 py-1.5 text-[10px] text-[var(--color-text2)] uppercase border-b border-[var(--color-border)]">
            <span>{ko ? '파일' : 'Files'}</span>
          </div>
          <div className="py-1">
            {fileTree.length === 0 ? (
              <div className="px-3 py-4 text-xs text-[var(--color-text2)] text-center">
                {ko ? '프로젝트를 선택하세요' : 'Select a project'}
              </div>
            ) : (
              fileTree.map(node => (
                <FileTreeNode key={node.path} node={node} depth={0} onFileClick={onFileClick} />
              ))
            )}
          </div>
        </div>
      ) : (
        /* Session History */
        <div className="flex-1 overflow-y-auto">
          <div className="px-2 py-1.5 text-[10px] text-[var(--color-text2)] uppercase border-b border-[var(--color-border)] flex items-center justify-between">
            <span>{ko ? '대화 기록' : 'History'}</span>
            <button onClick={onNewChat} className="text-[10px] bg-[var(--color-accent)] text-white px-1.5 py-0.5 rounded hover:opacity-80">
              + {ko ? '새 대화' : 'New'}
            </button>
          </div>
          {sessions.length > 3 && (
            <div className="px-2 py-1">
              <input
                value={sessionSearch}
                onChange={(e) => setSessionSearch(e.target.value)}
                placeholder={ko ? '검색...' : 'Search...'}
                className="w-full bg-[var(--color-bg)] border border-[var(--color-border)] rounded px-2 py-1 text-[11px] text-[var(--color-text)] placeholder:text-[var(--color-text2)]"
              />
            </div>
          )}
          <div className="py-1">
            {sessions.length === 0 ? (
              <div className="px-3 py-4 text-xs text-[var(--color-text2)] text-center">{ko ? '대화 기록 없음' : 'No history'}</div>
            ) : (
              sessions.filter(s => !sessionSearch || s.title.toLowerCase().includes(sessionSearch.toLowerCase()) || s.preview.toLowerCase().includes(sessionSearch.toLowerCase())).map((s) => (
                <div
                  key={s.id}
                  onClick={() => onSessionClick?.(s.id)}
                  className="px-3 py-2 cursor-pointer hover:bg-[var(--color-surface2)] transition-colors group"
                >
                  <div className="flex items-center justify-between">
                    <div className="text-xs font-medium truncate flex-1">{s.title}</div>
                    <button
                      onClick={async (e) => {
                        e.stopPropagation();
                        await fetchJSON(`/api/sessions/${s.id}`, { method: 'DELETE' });
                        loadSessions();
                      }}
                      className="text-[10px] text-[var(--color-text2)] hover:text-[var(--color-red)] opacity-0 group-hover:opacity-100 transition-opacity ml-1 shrink-0"
                      title={ko ? '삭제' : 'Delete'}
                    >
                      ✕
                    </button>
                  </div>
                  <div className="text-[10px] text-[var(--color-text2)] truncate">{s.preview}</div>
                  <div className="text-[9px] text-[var(--color-text2)] mt-0.5">{s.turns} {ko ? '턴' : 'turns'} · {s.model}</div>
                </div>
              ))
            )}
          </div>
        </div>
      )}
    </div>
  );
}
