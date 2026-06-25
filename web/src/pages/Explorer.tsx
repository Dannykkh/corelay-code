import { useState, useEffect } from 'react';
import { fetchJSON } from '../lib/api';

interface ConfigResponse {
  workDir?: string;
}

interface ProjectInfo {
  name: string;
  type: string;
  framework?: string;
  fileCount: number;
  fileTree?: string;
}

type AgentEvent = {
  type?: string;
  data?: unknown;
};

type ToolResultData = {
  result?: string;
};

function toolResultData(data: unknown): ToolResultData {
  return data && typeof data === 'object' ? data as ToolResultData : {};
}

export function ExplorerPage() {
  const [workDir, setWorkDir] = useState('');
  const [fileContent, setFileContent] = useState('');
  const [selectedFile, setSelectedFile] = useState('');
  const [project, setProject] = useState<ProjectInfo | null>(null);

  useEffect(() => {
    fetchJSON<ConfigResponse>('/api/config').then((c) => {
      const dir = c.workDir || '.';
      setWorkDir(dir);
      fetchJSON<ProjectInfo>(`/api/project?workDir=${encodeURIComponent(dir)}`).then(setProject);
    });
  }, []);

  async function readFile(path: string) {
    setSelectedFile(path);
    try {
      const resp = await fetch('/api/agent', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: [{ role: 'user', content: `Read the file ${path} and show its contents` }],
          workDir: workDir,
        }),
      });
      const reader = resp.body!.getReader();
      const decoder = new TextDecoder();
      let content = '';
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        const text = decoder.decode(value);
        const lines = text.split('\n');
        for (const line of lines) {
          if (line.startsWith('data: ')) {
            try {
              const event = JSON.parse(line.slice(6)) as AgentEvent;
              if (event.type === 'text') content += typeof event.data === 'string' ? event.data : '';
              if (event.type === 'tool_result') content = toolResultData(event.data).result || '';
            } catch { /* skip */ }
          }
        }
      }
      setFileContent(content);
    } catch (e) {
      setFileContent(`Error: ${e}`);
    }
  }

  return (
    <div className="flex h-full w-full">
      {/* Left: Project Info + File List */}
      <div className="w-72 border-r border-[var(--color-border)] bg-[var(--color-surface)] flex flex-col overflow-hidden">
        {/* Project Header */}
        {project && (
          <div className="p-3 border-b border-[var(--color-border)]">
            <div className="text-xs text-[var(--color-text2)] uppercase">Project</div>
            <div className="text-sm font-semibold mt-1">{project.name}</div>
            <div className="flex gap-2 mt-1">
              <span className="text-[10px] bg-[var(--color-accent)]/20 text-[var(--color-accent)] px-1.5 py-0.5 rounded">{project.type}</span>
              {project.framework && (
                <span className="text-[10px] bg-[var(--color-green)]/20 text-[var(--color-green)] px-1.5 py-0.5 rounded">{project.framework}</span>
              )}
              <span className="text-[10px] text-[var(--color-text2)]">{project.fileCount} files</span>
            </div>
          </div>
        )}

        {/* File Tree */}
        <div className="flex-1 overflow-y-auto p-2">
          <div className="text-xs text-[var(--color-text2)] uppercase px-2 mb-2">Files</div>
          {project?.fileTree ? (
            <pre className="text-xs text-[var(--color-text)] font-mono leading-relaxed px-2 whitespace-pre-wrap">
              {project.fileTree.split('\n').map((line: string, i: number) => {
                const isDir = line.trimEnd().endsWith('/');
                const trimmed = line.replace(/^\s+/, '');
                const indent = line.length - trimmed.length;
                return (
                  <div
                    key={i}
                    className={`py-0.5 cursor-pointer hover:bg-[var(--color-surface2)] rounded px-1 ${
                      selectedFile === trimmed.replace('/', '') ? 'bg-[var(--color-accent)]/10 text-[var(--color-accent)]' : ''
                    }`}
                    style={{ paddingLeft: indent * 8 + 4 }}
                    onClick={() => !isDir && readFile(trimmed.replace('/', ''))}
                  >
                    <span className="mr-1">{isDir ? '📁' : '📄'}</span>
                    {trimmed}
                  </div>
                );
              })}
            </pre>
          ) : (
            <div className="text-xs text-[var(--color-text2)] px-2">Loading...</div>
          )}
        </div>
      </div>

      {/* Right: File Viewer */}
      <div className="flex-1 flex flex-col">
        <div className="px-4 py-2 border-b border-[var(--color-border)] bg-[var(--color-surface)] flex items-center justify-between">
          <div className="text-sm font-medium">
            {selectedFile ? `📄 ${selectedFile}` : 'Select a file'}
          </div>
        </div>
        <div className="flex-1 overflow-auto p-4 bg-[var(--color-bg)]">
          {fileContent ? (
            <pre className="text-xs font-mono text-[var(--color-text)] whitespace-pre-wrap leading-relaxed">
              {fileContent}
            </pre>
          ) : (
            <div className="flex items-center justify-center h-full text-[var(--color-text2)] text-sm">
              Click a file to view its contents
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
