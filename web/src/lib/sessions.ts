import { fetchJSON, HTTPError, postJSON } from './api';

export type SessionLifecycleStatus = 'active' | 'interrupted' | 'closed' | 'recovery-needed';
export type SessionSideEffectState = 'unknown' | 'started' | 'may_have_applied' | 'applied';
export type CompletionContractStatus = 'incomplete' | 'complete' | 'blocked';

export interface DurableRunTerminalMetadata {
  terminalState?: string;
  completionStatus?: CompletionContractStatus;
  completionRevision?: number;
  completionCriteria?: number;
  completionSatisfied?: number;
  completionBlocked?: number;
}

export interface SessionInterruption {
  at: string;
  reason?: string;
  runId?: string;
  toolName?: string;
  toolCallId?: string;
  inputDigest?: string;
  sideEffectState?: SessionSideEffectState;
  summary?: string;
}

export interface SessionLifecycleFields {
  version?: number;
  revision?: number;
  parentSessionId?: string;
  parentRevision?: number;
  lifecycleStatus?: SessionLifecycleStatus;
  lastCommittedRevision?: number;
  reconcileRequired?: boolean;
  interruption?: SessionInterruption;
  lastRunTerminal?: DurableRunTerminalMetadata;
}

export interface SessionSummary {
  id: string;
  title: string;
  preview: string;
  turns: number;
  provider: string;
  model: string;
  updatedAt: string;
  workspace?: string;
  version?: number;
  revision?: number;
  parentSessionId?: string;
  parentRevision?: number;
  lifecycleStatus?: SessionLifecycleStatus;
  lastCommittedRevision?: number;
  reconcileRequired?: boolean;
  lastRunTerminal?: DurableRunTerminalMetadata;
}

export interface SessionMessage {
  role: 'user' | 'assistant' | 'tool';
  content: string;
  toolName?: string;
  toolInput?: Record<string, unknown> | string;
  toolResult?: string;
  isError?: boolean;
  timestamp: string;
}

export interface Session extends SessionLifecycleFields {
  id: string;
  title: string;
  messages: SessionMessage[];
  provider: string;
  model: string;
  createdAt: string;
  updatedAt: string;
  turns: number;
  workspace?: string;
}

export interface NormalizedSessionLifecycle {
  status: SessionLifecycleStatus;
  revision: number;
  lastCommittedRevision: number;
  reconcileRequired: boolean;
  interruption?: SessionInterruption;
  lastRunTerminal?: DurableRunTerminalMetadata;
}

export interface SessionResumeState {
  sessionId: string;
  revision: number;
  parentSessionId?: string;
  parentRevision?: number;
  lifecycleStatus: SessionLifecycleStatus;
  lastCommittedRevision: number;
  reconcileRequired: boolean;
  interruption?: SessionInterruption;
  lastRunTerminal?: DurableRunTerminalMetadata;
}

export interface SessionMutationResult {
  ok: boolean;
  id: string;
  version: number;
  revision: number;
  session?: Session;
}

export interface SessionLifecycleMutationResult {
  ok: boolean;
  revision: number;
  session: Session;
}

export interface SessionDeleteCleanup {
  resultCount: number;
  totalBytes: number;
  cleanupPending: boolean;
}

export interface SessionDeleteResult {
  ok: boolean;
  cleanup: SessionDeleteCleanup;
}

export interface SessionInterruptInput {
  runId: string;
  toolName: string;
  toolCallId?: string;
  inputDigest: string;
  sideEffectState: SessionSideEffectState;
  summary: string;
}

export class SessionConflictError extends Error {
  readonly status = 409;

  constructor(message: string) {
    super(message);
    this.name = 'SessionConflictError';
  }
}

async function surfaceSessionConflict<T>(request: Promise<T>): Promise<T> {
  try {
    return await request;
  } catch (error) {
    if (error instanceof HTTPError && error.status === 409) {
      throw new SessionConflictError(error.message);
    }
    throw error;
  }
}

// Older servers omit lifecycle metadata. Treat those sessions as active while
// preserving a fail-closed signal whenever an interruption is present.
export function sessionLifecycle(
  session: SessionLifecycleFields,
): NormalizedSessionLifecycle {
  const revision = session.revision ?? 0;
  const reconcileRequired = session.reconcileRequired === true ||
    session.lifecycleStatus === 'interrupted' ||
    session.lifecycleStatus === 'recovery-needed';
  return {
    status: session.lifecycleStatus ?? (reconcileRequired ? 'recovery-needed' : 'active'),
    revision,
    lastCommittedRevision: session.lastCommittedRevision ?? revision,
    reconcileRequired,
    interruption: session.interruption,
    lastRunTerminal: session.lastRunTerminal,
  };
}

export async function listSessions(workspace?: string): Promise<SessionSummary[]> {
  const params = workspace ? `?workspace=${encodeURIComponent(workspace)}` : '';
  // Backend returns JSON `null` for an empty workspace; the typed return is
  // an array, so coerce once here for every caller.
  const data = await fetchJSON<SessionSummary[] | null>(`/api/sessions${params}`);
  return data ?? [];
}

export async function getSession(id: string): Promise<Session> {
  return fetchJSON(`/api/sessions/${encodeURIComponent(id)}`);
}

export async function saveSession(
  session: Partial<Session>,
  expectedRevision?: number,
): Promise<SessionMutationResult> {
  const revision = expectedRevision ?? session.revision;
  if (session.id && revision === undefined) {
    throw new Error('expectedRevision is required when updating a session');
  }
  const body = session.id ? { ...session, expectedRevision: revision } : session;
  return surfaceSessionConflict(postJSON('/api/sessions', body));
}

export async function deleteSession(
  id: string,
  expectedRevision: number,
): Promise<SessionDeleteResult> {
  return surfaceSessionConflict(fetchJSON(`/api/sessions/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ expectedRevision }),
  }));
}

export async function renameSession(
  id: string,
  title: string,
  expectedRevision: number,
): Promise<SessionMutationResult> {
  return surfaceSessionConflict(fetchJSON(`/api/sessions/${encodeURIComponent(id)}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ title, expectedRevision }),
  }));
}

export async function forkSession(id: string, expectedRevision: number): Promise<SessionMutationResult> {
  return surfaceSessionConflict(postJSON(`/api/sessions/${encodeURIComponent(id)}/fork`, { expectedRevision }));
}

export async function interruptSession(
  id: string,
  expectedRevision: number,
  interruption: SessionInterruptInput,
): Promise<SessionLifecycleMutationResult> {
  return surfaceSessionConflict(postJSON(`/api/sessions/${encodeURIComponent(id)}/interrupt`, {
    expectedRevision,
    ...interruption,
  }));
}

export async function reconcileSession(id: string, expectedRevision: number): Promise<SessionLifecycleMutationResult> {
  return surfaceSessionConflict(postJSON(`/api/sessions/${encodeURIComponent(id)}/reconcile`, { expectedRevision }));
}

export async function closeSession(id: string, expectedRevision: number): Promise<SessionLifecycleMutationResult> {
  return surfaceSessionConflict(postJSON(`/api/sessions/${encodeURIComponent(id)}/close`, { expectedRevision }));
}

export async function getSessionResumeState(id: string): Promise<SessionResumeState> {
  return fetchJSON(`/api/sessions/${encodeURIComponent(id)}/resume-state`);
}
