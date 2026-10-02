// Typed client for the NeelKIT API.
//
// Every network call in the app goes through here. Components never build a
// URL or read a header, so changing the base URL, the auth scheme or the error
// shape is a one-file change.

import type {
  ApiError,
  Conversation,
  Message,
  ModelsResponse,
  ReadyResponse,
  ToolsResponse,
  TranscriptHit,
  Turn,
} from './types';

const KEY_STORAGE = 'neelkit.apiKey';

export const API_URL =
  process.env.NEXT_PUBLIC_API_URL || 'http://127.0.0.1:8420';

/** Thrown for any non-2xx response, carrying the server's own error code. */
export class NeelkitError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code: string,
    readonly requestId?: string,
  ) {
    super(message);
    this.name = 'NeelkitError';
  }
}

/**
 * The API key lives in localStorage, not a cookie.
 *
 * A cookie would be attached automatically by the browser to every request to
 * this origin, which is exactly the CSRF surface a service that can run shell
 * commands must not have. An explicit header cannot be forged by another page.
 */
export function getApiKey(): string {
  if (typeof window === 'undefined') return '';
  try {
    return window.localStorage.getItem(KEY_STORAGE) ?? '';
  } catch {
    // Private browsing, or storage disabled entirely.
    return '';
  }
}

export function setApiKey(key: string): void {
  try {
    if (key) window.localStorage.setItem(KEY_STORAGE, key.trim());
    else window.localStorage.removeItem(KEY_STORAGE);
  } catch {
    /* nothing we can do; the caller will see the next request fail */
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const key = getApiKey();
  const headers = new Headers(init.headers);
  if (key) headers.set('Authorization', `Bearer ${key}`);
  if (init.body && !(init.body instanceof FormData)) {
    headers.set('Content-Type', 'application/json');
  }

  let res: Response;
  try {
    res = await fetch(`${API_URL}${path}`, { ...init, headers });
  } catch (e) {
    // fetch only rejects for network-level failures, which here almost always
    // means the server is not running. Say that, rather than "Failed to fetch".
    throw new NeelkitError(
      `Cannot reach NeelKIT at ${API_URL}. Is neelkitd running?`,
      0,
      'unreachable',
    );
  }

  if (res.status === 204) return undefined as T;

  const text = await res.text();
  if (!res.ok) {
    let code = 'unknown';
    let message = text || res.statusText;
    let requestId: string | undefined;
    try {
      const body = JSON.parse(text) as ApiError;
      if (body.error) {
        code = body.error.code;
        message = body.error.message;
        requestId = body.error.request_id;
      }
    } catch {
      /* not JSON; keep the raw text */
    }
    throw new NeelkitError(message, res.status, code, requestId);
  }

  if (!text) return undefined as T;
  const ct = res.headers.get('content-type') ?? '';
  return (ct.includes('json') ? JSON.parse(text) : text) as T;
}

export const api = {
  /** Liveness only; never consults dependencies. */
  health: () => request<{ status: string }>('/healthz'),

  /** Readiness, with a per-dependency breakdown. */
  ready: () => request<ReadyResponse>('/readyz'),

  models: () => request<ModelsResponse>('/v1/models'),

  tools: () => request<ToolsResponse>('/v1/tools'),

  /** Send a typed turn. `execute: false` plans without touching the machine. */
  turn: (text: string, conversationId?: string, execute = true) =>
    request<Turn>('/v1/assistant/turns', {
      method: 'POST',
      body: JSON.stringify({
        text,
        conversation_id: conversationId || undefined,
        execute,
      }),
    }),

  approve: (callId: string) =>
    request<import('./types').CallResult>('/v1/assistant/approvals', {
      method: 'POST',
      body: JSON.stringify({ call_id: callId, decision: 'approve' }),
    }),

  deny: (callId: string, reason = '') =>
    request<{ call_id: string; status: string }>('/v1/assistant/approvals', {
      method: 'POST',
      body: JSON.stringify({ call_id: callId, decision: 'deny', reason }),
    }),

  conversations: (limit = 50) =>
    request<{ conversations: Conversation[] }>(
      `/v1/conversations?limit=${limit}`,
    ),

  messages: (id: string) =>
    request<{ conversation_id: string; messages: Message[] }>(
      `/v1/conversations/${encodeURIComponent(id)}/messages`,
    ),

  deleteConversation: (id: string) =>
    request<{ deleted: boolean; note: string }>(
      `/v1/conversations/${encodeURIComponent(id)}`,
      { method: 'DELETE' },
    ),

  search: (q: string, limit = 25) =>
    request<{ query: string; results: TranscriptHit[] }>(
      `/v1/transcripts/search?q=${encodeURIComponent(q)}&limit=${limit}`,
    ),

  audit: (hours = 24, limit = 100) =>
    request<{ events: import('./types').ApiError[] }>(
      `/v1/audit?hours=${hours}&limit=${limit}`,
    ),

  /** Exchange the API key for a short-lived single-use WebSocket ticket. */
  ticket: () =>
    request<{ ticket: string; expires_at: string; ttl_seconds: number }>(
      '/v1/realtime/tickets',
      { method: 'POST' },
    ),

  transcribeFile: async (file: File, opts: { words?: boolean } = {}) => {
    const form = new FormData();
    form.append('file', file);
    form.append('confidence', 'true');
    if (opts.words) form.append('timestamps', 'word');
    return request<{
      text: string;
      duration: number;
      confidence?: number;
      words?: import('./types').Word[];
      metrics?: { rtf: number; inference_ms: number };
    }>('/v1/audio/transcriptions', { method: 'POST', body: form });
  },
};
