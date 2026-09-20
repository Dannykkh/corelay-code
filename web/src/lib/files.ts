import { fetchJSON } from './api';

export type FileReadKind =
  | 'text'
  | 'markdown'
  | 'json'
  | 'code'
  | 'directory'
  | 'binary'
  | 'image'
  | 'too_large'
  | 'unsupported'
  | 'error';

export interface FileTreeEntry {
  name: string;
  path: string;
  isDir: boolean;
  size?: number;
  children?: FileTreeEntry[];
}

export interface FileReadResponse {
  path: string;
  type: FileReadKind | string;
  size: number;
  ext?: string;
  lines: number;
  content?: string;
  truncated?: boolean;
  entries?: FileTreeEntry[];
}

export async function readWorkspaceFile(workDir: string, path: string): Promise<FileReadResponse> {
  const query = new URLSearchParams({ path, workDir });
  return fetchJSON<FileReadResponse>(`/api/file?${query}`);
}

export function fileResponseContent(response: FileReadResponse): string {
  if (response.type !== 'directory') return response.content || '';
  if (!response.entries?.length) return '[Empty directory]';
  return response.entries
    .map((entry) => `${entry.isDir ? '[dir] ' : '      '}${entry.path}`)
    .join('\n');
}

export function isEditableFileType(type: string): boolean {
  return type === 'text' || type === 'markdown' || type === 'json' || type === 'code';
}
