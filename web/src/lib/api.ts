import { streamSSE } from './sse';

const BASE = '';

let bootstrapInFlight: Promise<boolean> | undefined;

function bootstrapBrowser(): Promise<boolean> {
  if (!bootstrapInFlight) {
    bootstrapInFlight = (async () => {
      const issued = await fetch(BASE + '/api/bootstrap/challenge', { method: 'POST', cache: 'no-store' });
      if (!issued.ok) return false;
      const body: unknown = await issued.json();
      if (!body || typeof body !== 'object' || !('challenge' in body) || typeof body.challenge !== 'string') return false;
      const exchanged = await fetch(BASE + '/api/bootstrap', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ challenge: body.challenge }), cache: 'no-store',
      });
      return exchanged.ok;
    })().finally(() => { bootstrapInFlight = undefined; });
  }
  return bootstrapInFlight;
}

type ErrorBody = {
  error?: { message?: unknown };
  message?: unknown;
};

function errorMessageFromBody(body: unknown): string | undefined {
  if (!body || typeof body !== 'object') return undefined;
  const parsed = body as ErrorBody;
  if (typeof parsed.error?.message === 'string') return parsed.error.message;
  if (typeof parsed.message === 'string') return parsed.message;
  return undefined;
}

export class HTTPError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = 'HTTPError';
    this.status = status;
  }
}

export async function fetchJSON<T>(url: string, init?: RequestInit): Promise<T> {
  let res = await fetch(BASE + url, init);
  if (res.status === 401 && !url.startsWith('/api/bootstrap') && url.startsWith('/api/')) {
    if (await bootstrapBrowser()) res = await fetch(BASE + url, init);
  }
  if (!res.ok) {
    // Surface server errors as exceptions so callers can show a toast instead
    // of silently treating the error body as a successful result. The server's
    // writeError shape is `{ error: { message }, type }`; we also tolerate
    // plain `{ message }` and bare text.
    let msg = `HTTP ${res.status}`;
    try {
      const body: unknown = await res.json();
      msg = errorMessageFromBody(body) || msg;
    } catch {
      try {
        const text = await res.text();
        if (text) msg = text;
      } catch { /* keep msg */ }
    }
    throw new HTTPError(res.status, msg);
  }
  return res.json();
}

export async function postJSON<T>(url: string, body: unknown): Promise<T> {
  return fetchJSON(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
}

export async function putJSON<T>(url: string, body: unknown): Promise<T> {
  return fetchJSON(url, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
}

export type ApprovalDecision = 'allow_once' | 'deny';

export interface ApprovalResolution {
  id: string;
  decision: ApprovalDecision;
  reason?: string;
  resolvedAt: string;
}

export async function resolveApproval(
  approvalID: string,
  sessionID: string,
  decision: ApprovalDecision,
  signal?: AbortSignal,
): Promise<ApprovalResolution> {
  if (!approvalID.trim() || !sessionID.trim()) {
    throw new Error('Approval and runtime session IDs are required');
  }
  return fetchJSON(`/api/approvals/${encodeURIComponent(approvalID)}/resolve`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sessionId: sessionID, decision }),
    signal,
  });
}

// SSE streaming for chat
export async function* streamChat(messages: unknown[], model?: string): AsyncGenerator<string> {
  const res = await fetch(BASE + '/v1/messages', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      model: model || 'default',
      messages,
      max_tokens: 8192,
      stream: true,
    }),
  });

  for await (const frame of streamSSE(res)) {
    try {
      const event = JSON.parse(frame.data);
      if (event.type === 'content_block_delta' && event.delta?.type === 'text_delta') {
        yield event.delta.text;
      }
      if (event.type === 'message_stop') return;
    } catch { /* skip */ }
  }
}

// Types
export interface ProviderInfo {
  name: string;
  displayName: string;
  models: { id: string; displayName: string }[];
}

export interface RouteRule {
  role: string;
  provider: string;
  model: string;
  fallback?: { provider: string; model: string };
}

export interface UsageEntry {
  provider: string;
  model: string;
  requests: number;
  tokens: number;
}
