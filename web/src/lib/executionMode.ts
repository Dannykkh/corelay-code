import type { SessionExecutionMode } from './sessions';

// The user's chosen execution mode for the next agent turns. The server only
// accepts a mode from the client (it assigns capabilities and user-selection
// provenance itself), so this is sent as `executionPolicy.mode` on /api/agent.
// null means "no explicit choice": the session's stored policy or the server
// default (workspace) applies.
const STORAGE_KEY = 'corelay.executionMode';
const MODES: readonly SessionExecutionMode[] = ['read-only', 'workspace', 'full'];

export function loadPreferredExecutionMode(): SessionExecutionMode | null {
  try {
    const value = localStorage.getItem(STORAGE_KEY);
    return MODES.includes(value as SessionExecutionMode) ? (value as SessionExecutionMode) : null;
  } catch {
    return null;
  }
}

export function savePreferredExecutionMode(mode: SessionExecutionMode | null): void {
  try {
    if (mode) localStorage.setItem(STORAGE_KEY, mode);
    else localStorage.removeItem(STORAGE_KEY);
  } catch {
    // Without storage the choice still applies to this page load via the caller's state.
  }
}
