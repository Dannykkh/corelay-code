import { fetchJSON, postJSON } from './api';

export interface VerificationPolicy {
  commands?: string[];
  requiredSignals?: string[];
  maxRepairAttempts?: number;
}

export interface VerificationResult {
  status?: string;
  source?: string;
  summary?: string;
  updatedAt?: string;
}

export interface Goal {
  objective?: string;
  acceptanceCriteria?: string[];
  constraints?: string[];
  definitionOfDone?: string[];
  verificationPolicy?: VerificationPolicy;
}

export interface Workstream {
  id: string;
  title: string;
  workspace: string;
  status: 'active' | 'blocked' | 'completed' | 'archived';
  summary?: string;
  nextAction?: string;
  openQuestions?: string[];
  tags?: string[];
  goal?: Goal;
  lastVerification?: VerificationResult;
  createdAt: string;
  updatedAt: string;
}

export interface TimelineEvent {
  id: string;
  type: string;
  at: string;
  message?: string;
  data?: Record<string, string>;
}

export async function listWorkstreams(): Promise<Workstream[]> {
  const data = await fetchJSON<{ workstreams: Workstream[] }>('/api/workstreams');
  return data.workstreams ?? [];
}

export async function createWorkstream(body: {
  title: string;
  summary?: string;
  nextAction?: string;
  tags?: string[];
  goal?: Goal;
}): Promise<Workstream> {
  const data = await postJSON<{ workstream: Workstream }>('/api/workstreams', body);
  return data.workstream;
}

export async function generateHandoff(id: string): Promise<{ path: string; markdown: string }> {
  return postJSON(`/api/workstreams/${encodeURIComponent(id)}/handoff`, {
    includeReceipts: true,
    includeMemoryIndex: true,
  });
}
