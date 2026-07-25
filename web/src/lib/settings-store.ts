import { useSyncExternalStore } from 'react';
import { DEFAULT_API_URL, DEFAULT_MODEL } from './constants';

export type PermissionMode = 'default' | 'acceptEdits' | 'plan' | 'bypassPermissions';

export interface PermissionSettings {
  mode: PermissionMode;
  allow: string[];
  deny: string[];
  ask: string[];
  /** Tool name to auto-approval flag, e.g. { Read: true, Bash: false }. */
  autoApprove: Record<string, boolean>;
}

/** Anthropic Messages content block, narrowed to what the UI reads. */
export interface ContentBlock {
  type: string;
  text?: string;
}

export interface ConversationMessage {
  role: 'user' | 'assistant' | string;
  content: string | ContentBlock[];
}

export interface Conversation {
  id: string;
  title: string;
  messages: ConversationMessage[];
  createdAt: number;
  updatedAt: number;
  model?: string;
  tags?: string[];
  isPinned?: boolean;
}

export interface McpServerConfig {
  name: string;
  command?: string;
  args?: string[];
  url?: string;
  enabled: boolean;
}

export interface Settings {
  apiKey: string;
  apiUrl: string;
  model: string;
  systemPrompt: string;
  temperature: number;
  maxTokens: number;
  streamingEnabled: boolean;
  telemetryEnabled: boolean;
  permissions: PermissionSettings;
  mcpServers: McpServerConfig[];
}

export const DEFAULT_SETTINGS: Settings = {
  apiKey: '',
  apiUrl: DEFAULT_API_URL,
  model: DEFAULT_MODEL,
  systemPrompt: '',
  temperature: 1,
  maxTokens: 8192,
  streamingEnabled: true,
  telemetryEnabled: false,
  permissions: { mode: 'default', allow: [], deny: [], ask: [], autoApprove: {} },
  mcpServers: [],
};

/** Which settings each reset scope clears. An unknown scope resets everything. */
const RESET_SCOPES: Record<string, (keyof Settings)[]> = {
  api: ['apiKey', 'apiUrl'],
  model: ['model', 'temperature', 'maxTokens', 'systemPrompt'],
  routing: ['model'],
  permissions: ['permissions'],
  integrations: ['mcpServers'],
  privacy: ['telemetryEnabled'],
};

const STORAGE_KEY = 'aniclew.settings';

function load(): Settings {
  if (typeof localStorage === 'undefined') return DEFAULT_SETTINGS;
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return DEFAULT_SETTINGS;
    const parsed = JSON.parse(raw) as Partial<Settings>;
    // Merge rather than replace: a settings key added in a later version must
    // not come back undefined for users with an older payload in storage.
    return {
      ...DEFAULT_SETTINGS,
      ...parsed,
      permissions: { ...DEFAULT_SETTINGS.permissions, ...(parsed.permissions ?? {}) },
      mcpServers: parsed.mcpServers ?? DEFAULT_SETTINGS.mcpServers,
    };
  } catch {
    return DEFAULT_SETTINGS;
  }
}

function persist(next: Settings) {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(next));
  } catch {
    // Quota or private-mode failure is not worth breaking the UI over.
  }
}

let current: Settings = load();
const listeners = new Set<() => void>();

function subscribe(onChange: () => void) {
  listeners.add(onChange);
  return () => {
    listeners.delete(onChange);
  };
}

function getSnapshot(): Settings {
  return current;
}

function commit(next: Settings) {
  current = next;
  persist(next);
  listeners.forEach((listener) => listener());
}

export function updateSettings(patch: Partial<Settings>) {
  commit({ ...current, ...patch });
}

export function resetSettings(scope?: string) {
  const keys = scope ? RESET_SCOPES[scope] : undefined;
  if (!keys) {
    commit({ ...DEFAULT_SETTINGS });
    return;
  }

  const next = { ...current };
  for (const key of keys) {
    // Index through a mutable alias: each key is its own value type, and a
    // per-key assignment cannot be expressed generically without a cast.
    (next as Record<string, unknown>)[key] = DEFAULT_SETTINGS[key];
  }
  commit(next);
}

export interface SettingsStore {
  settings: Settings;
  updateSettings: (patch: Partial<Settings>) => void;
  resetSettings: (scope?: string) => void;
  conversations: Conversation[];
  deleteConversation: (id: string) => void;
}

/**
 * Minimal external store for the settings surface. Replaces the Next.js app's
 * store for the ported components; only the fields those components read are
 * modelled here.
 */
/** Stable empty list so the snapshot identity does not change between renders. */
const NO_CONVERSATIONS: Conversation[] = [];

function deleteConversation(_id: string) {
  // Intentionally inert until the sessions layer is wired in — see below.
}

export function useSettingsStore(): SettingsStore {
  const settings = useSyncExternalStore(subscribe, getSnapshot, getSnapshot);
  // Conversation history lives in the chat session layer (lib/sessions), which
  // the settings surface only reads counts from and offers deletion for. Wire
  // both here once that layer exposes a subscribable list; until then counters
  // read zero rather than showing a wrong number, and deletion is a no-op
  // rather than silently claiming to have removed something.
  return {
    settings,
    updateSettings,
    resetSettings,
    conversations: NO_CONVERSATIONS,
    deleteConversation,
  };
}
