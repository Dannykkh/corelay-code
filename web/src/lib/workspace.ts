const WORKSPACE_SELECTION_KEY = 'corelay-workspace-selection';

export function loadWorkspaceSelection(): string {
  if (typeof localStorage === 'undefined') return '';
  try {
    return localStorage.getItem(WORKSPACE_SELECTION_KEY) || '';
  } catch {
    return '';
  }
}

export function saveWorkspaceSelection(path: string): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(WORKSPACE_SELECTION_KEY, path);
  } catch {
    // Keep the selected project active for this app session if storage is unavailable.
  }
}

export function sameWorkspacePath(left: string, right: string): boolean {
  const normalize = (path: string) => {
    const normalized = path.replace(/\\/g, '/').replace(/\/+$/, '');
    if (/^[A-Za-z]:$/.test(normalized)) return `${normalized}/`;
    return normalized || '/';
  };
  const a = normalize(left);
  const b = normalize(right);
  const windowsPath = /^[A-Za-z]:\//.test(a) && /^[A-Za-z]:\//.test(b);
  return windowsPath ? a.toLowerCase() === b.toLowerCase() : a === b;
}
