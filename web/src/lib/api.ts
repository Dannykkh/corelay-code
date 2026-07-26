const BASE = '';

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

export async function fetchJSON<T>(url: string, init?: RequestInit): Promise<T> {
  const res = await fetch(BASE + url, init);
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
    throw new Error(msg);
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

  const reader = res.body!.getReader();
  const decoder = new TextDecoder();
  let buffer = '';

  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });

    const lines = buffer.split('\n');
    buffer = lines.pop() || '';

    for (const line of lines) {
      const trimmed = line.trim();
      if (!trimmed || trimmed.startsWith('event:')) continue;
      if (trimmed.startsWith('data: ')) {
        try {
          const event = JSON.parse(trimmed.slice(6));
          if (event.type === 'content_block_delta' && event.delta?.type === 'text_delta') {
            yield event.delta.text;
          }
          if (event.type === 'message_stop') return;
        } catch { /* skip */ }
      }
    }
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
