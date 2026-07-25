export interface ModelOption {
  id: string;
  label: string;
  description: string;
}

/**
 * Fallback model list for pickers rendered before /api/runtime answers.
 * The live list comes from RuntimeProviderInfo.models; this only covers the
 * first paint and the offline case.
 */
export const MODELS: ModelOption[] = [
  { id: 'claude-opus-4-6', label: 'Claude Opus 4.6', description: 'Most capable' },
  { id: 'claude-sonnet-4-6', label: 'Claude Sonnet 4.6', description: 'Balanced' },
  { id: 'claude-haiku-4-5-20251001', label: 'Claude Haiku 4.5', description: 'Fastest' },
  { id: 'gpt-5.5', label: 'GPT-5.5', description: 'Codex-compatible group' },
  { id: 'qwen3:14b', label: 'Qwen3 14B', description: 'Local via Ollama' },
];

export const DEFAULT_MODEL = 'claude-sonnet-4-6';

export const DEFAULT_API_URL = 'http://localhost:4000';
