import { useState, useEffect } from 'react';
import { fetchJSON } from '../lib/api';
import { getLang } from '../lib/i18n';

interface BrowseEntry {
  name: string;
  isDir: boolean;
  size: number;
  isProject: boolean;
}

interface ProjectInfo {
  type: string;
  name: string;
  framework: string;
  fileCount: number;
}

interface WorkspaceResponse {
  path: string;
  project: ProjectInfo | null;
  ok?: boolean;
}

interface BrowseResponse {
  current: string;
  parent: string;
  entries?: BrowseEntry[];
}

export function WorkspacePage() {
  const [currentPath, setCurrentPath] = useState('');
  const [parentPath, setParentPath] = useState('');
  const [entries, setEntries] = useState<BrowseEntry[]>([]);
  const [workspace, setWorkspace] = useState('');
  const [project, setProject] = useState<ProjectInfo | null>(null);
  const [pathInput, setPathInput] = useState('');
  const [status, setStatus] = useState('');

  const ko = getLang() === 'ko';

  async function browse(path: string) {
    try {
      const data = await fetchJSON<BrowseResponse>(`/api/browse?path=${encodeURIComponent(path)}`);
      setCurrentPath(data.current);
      setParentPath(data.parent);
      setEntries(data.entries || []);
      setPathInput(data.current);
    } catch (e) {
      setStatus(`Error: ${e}`);
    }
  }

  async function selectWorkspace(path: string) {
    try {
      const data = await fetchJSON<WorkspaceResponse>('/api/workspace', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ path }),
      });
      if (data.ok) {
        setWorkspace(path);
        setProject(data.project);
        setStatus(ko ? '워크스페이스 설정됨!' : 'Workspace set!');
        setTimeout(() => setStatus(''), 2000);
      }
    } catch (e) {
      setStatus(`Error: ${e}`);
    }
  }

  useEffect(() => {
    fetchJSON<WorkspaceResponse>('/api/workspace').then((data) => {
      setWorkspace(data.path);
      setProject(data.project);
      void browse(data.path);
    });
  }, []);

  function goTo(name: string) {
    const newPath = currentPath.replace(/\\/g, '/') + '/' + name;
    browse(newPath);
  }

  function goUp() {
    browse(parentPath);
  }

  function goToInput() {
    browse(pathInput);
  }

  const projectTypes: Record<string, string> = {
    go: '🔵 Go', node: '🟢 Node.js', python: '🐍 Python',
    rust: '🦀 Rust', java: '☕ Java', dotnet: '🟣 .NET', unknown: '📁',
  };

  return (
    <div className="p-6 w-full max-w-4xl mx-auto">
      <h1 className="text-xl font-semibold mb-2">{ko ? '워크스페이스' : 'Workspace'}</h1>
      <p className="text-sm text-[var(--color-text2)] mb-6">
        {ko ? '프로젝트 폴더를 선택하면 채팅과 도구가 해당 폴더에서 동작합니다.' : 'Select a project folder. Chat and tools will operate in this directory.'}
      </p>

      {/* Current Workspace */}
      {workspace && (
        <div className="bg-[var(--color-surface)] border border-[var(--color-accent)]/30 rounded-xl p-4 mb-6">
          <div className="text-xs text-[var(--color-text2)] uppercase mb-2">{ko ? '현재 워크스페이스' : 'Current Workspace'}</div>
          <div className="flex items-center gap-3">
            <span className="text-2xl">{project ? (projectTypes[project.type] || '📁') : '📁'}</span>
            <div>
              <div className="text-base font-semibold text-[var(--color-text)]">{project?.name || workspace.split(/[/\\]/).pop()}</div>
              <div className="text-xs text-[var(--color-text2)]">{workspace}</div>
              {project && (
                <div className="flex gap-2 mt-1">
                  <span className="text-[10px] bg-[var(--color-accent)]/20 text-[var(--color-accent)] px-1.5 py-0.5 rounded">{project.type}</span>
                  {project.framework && <span className="text-[10px] bg-[var(--color-green)]/20 text-[var(--color-green)] px-1.5 py-0.5 rounded">{project.framework}</span>}
                  <span className="text-[10px] text-[var(--color-text2)]">{project.fileCount} files</span>
                </div>
              )}
            </div>
          </div>
        </div>
      )}

      {/* Path Input */}
      <div className="flex gap-2 mb-4">
        <input
          value={pathInput}
          onChange={(e) => setPathInput(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && goToInput()}
          placeholder={ko ? '경로 입력...' : 'Enter path...'}
          className="flex-1 bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg px-3 py-2 text-sm text-[var(--color-text)]"
        />
        <button onClick={goToInput} className="px-4 py-2 bg-[var(--color-accent)] text-white rounded-lg text-sm">
          {ko ? '이동' : 'Go'}
        </button>
      </div>

      {/* Status */}
      {status && (
        <div className="text-sm text-[var(--color-green)] mb-3">{status}</div>
      )}

      {/* Folder Browser */}
      <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl overflow-hidden">
        {/* Header */}
        <div className="px-4 py-2 bg-[var(--color-surface2)] border-b border-[var(--color-border)] flex items-center justify-between">
          <div className="text-xs text-[var(--color-text2)] font-mono truncate">{currentPath}</div>
          <button onClick={() => selectWorkspace(currentPath)} className="px-3 py-1 bg-[var(--color-accent)] text-white rounded text-xs">
            {ko ? '이 폴더 선택' : 'Select This Folder'}
          </button>
        </div>

        {/* Parent */}
        <div
          onClick={goUp}
          className="flex items-center gap-3 px-4 py-2.5 cursor-pointer hover:bg-[var(--color-surface2)] border-b border-[var(--color-border)]"
        >
          <span>⬆️</span>
          <span className="text-sm text-[var(--color-text2)]">..</span>
        </div>

        {/* Entries */}
        {entries.map((entry) => (
          <div
            key={entry.name}
            onClick={() => entry.isDir ? goTo(entry.name) : null}
            className={`flex items-center justify-between px-4 py-2.5 border-b border-[var(--color-border)] last:border-0 ${
              entry.isDir ? 'cursor-pointer hover:bg-[var(--color-surface2)]' : ''
            }`}
          >
            <div className="flex items-center gap-3">
              <span>{entry.isDir ? '📁' : '📄'}</span>
              <span className={`text-sm ${entry.isProject ? 'text-[var(--color-accent)] font-medium' : 'text-[var(--color-text)]'}`}>
                {entry.name}
              </span>
              {entry.isProject && (
                <span className="text-[10px] bg-[var(--color-accent)]/15 text-[var(--color-accent)] px-1.5 py-0.5 rounded-full">
                  {ko ? '프로젝트' : 'project'}
                </span>
              )}
            </div>
            <div className="flex items-center gap-2">
              {!entry.isDir && entry.size > 0 && (
                <span className="text-[10px] text-[var(--color-text2)]">
                  {entry.size < 1024 ? `${entry.size}B` : entry.size < 1048576 ? `${(entry.size/1024).toFixed(1)}KB` : `${(entry.size/1048576).toFixed(1)}MB`}
                </span>
              )}
              {entry.isDir && entry.isProject && (
                <button
                  onClick={(e) => { e.stopPropagation(); selectWorkspace(currentPath.replace(/\\/g, '/') + '/' + entry.name); }}
                  className="px-2 py-0.5 border border-[var(--color-accent)] text-[var(--color-accent)] rounded text-[10px] hover:bg-[var(--color-accent)] hover:text-white transition-colors"
                >
                  {ko ? '선택' : 'Select'}
                </button>
              )}
            </div>
          </div>
        ))}

        {entries.length === 0 && (
          <div className="text-center py-8 text-[var(--color-text2)] text-sm">{ko ? '빈 폴더' : 'Empty folder'}</div>
        )}
      </div>
    </div>
  );
}
