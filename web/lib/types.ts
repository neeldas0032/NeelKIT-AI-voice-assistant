// Wire types shared with the Go server.
//
// These mirror the JSON the API actually produces. Keeping them in one file
// means a field rename on the server breaks the build here rather than
// producing an `undefined` somewhere in a component at runtime.

export type Risk = 'read' | 'write' | 'execute' | 'network';
export type Decision = 'allow' | 'confirm' | 'deny';

export type CallStatus =
  | 'planned'
  | 'awaiting_approval'
  | 'running'
  | 'succeeded'
  | 'failed'
  | 'denied';

export type TurnStatus =
  | 'executed'
  | 'awaiting_approval'
  | 'denied'
  | 'failed'
  | 'chat';

export interface Word {
  word: string;
  start: number;
  end: number;
  confidence: number;
}

export interface CallResult {
  call_id: string;
  tool: string;
  args: Record<string, unknown>;
  risk: Risk;
  decision: Decision;
  reason: string;
  preview: string;
  status: CallStatus;
  summary?: string;
  stdout?: string;
  data?: unknown;
  error?: string;
  duration_ms: number;
}

export interface Intent {
  name: string;
  /** Which tier resolved this: a rule the user can read, or a model they cannot. */
  source: 'grammar' | 'llm' | 'none';
  confidence: number;
  slots?: Record<string, string>;
  pattern?: string;
  reply?: string;
}

export interface Turn {
  turn_id: string;
  conversation_id: string;
  text: string;
  raw_text: string;
  intent: Intent;
  calls: CallResult[];
  status: TurnStatus;
  message: string;
  latency_ms: number;
}

export interface Conversation {
  id: string;
  title: string;
  pinned: boolean;
  created_at: number;
  updated_at: number;
  message_count?: number;
}

export interface Message {
  id: string;
  conversation_id: string;
  turn_id: string;
  role: 'user' | 'assistant' | 'tool' | 'system';
  content: string;
  source: string;
  created_at: number;
}

export interface TranscriptHit {
  id: string;
  conversation_id?: string;
  text: string;
  confidence: number;
  duration_ms: number;
  created_at: number;
  snippet?: string;
}

export interface ToolDescriptor {
  name: string;
  title: string;
  description: string;
  schema: unknown;
  risk: Risk;
  idempotent: boolean;
  examples?: { utterance: string; args: unknown }[];
}

export interface ToolsResponse {
  tools: ToolDescriptor[];
  workspace: string;
  read_only: boolean;
}

export interface ModelsResponse {
  asr: {
    model: string;
    precision: string;
    device: string;
    workers: number;
    ready: boolean;
  };
  llm: { provider: string; model: string };
}

export interface ReadyResponse {
  ready: boolean;
  checks: Record<string, string>;
}

export interface ApiError {
  error: {
    type: string;
    code: string;
    message: string;
    request_id?: string;
    details?: Record<string, unknown>;
  };
}

/** Every message the realtime socket can send. */
export type ServerEvent =
  | { type: 'session.created'; session_id: string }
  | { type: 'vad.speech_started'; utterance_id: string; at_ms?: number }
  | { type: 'vad.speech_stopped'; utterance_id: string; duration_ms?: number }
  | {
      type: 'transcript.partial';
      utterance_id: string;
      text: string;
      stable: false;
      confidence?: number;
    }
  | {
      type: 'transcript.final';
      utterance_id: string;
      text: string;
      stable: true;
      confidence?: number;
      words?: Word[];
      duration_ms?: number;
      latency_ms?: number;
    }
  | { type: 'assistant.turn'; turn: Turn }
  | { type: 'tool.permission_required'; call: CallResult }
  | { type: 'tool.completed'; call: CallResult }
  | { type: 'stream.lagged'; dropped: number }
  | { type: 'stream.overloaded'; message?: string }
  | { type: 'error'; code?: string; message: string }
  | { type: 'pong' };

export type ClientMessage =
  | {
      type: 'session.configure';
      conversation_id?: string;
      mode?: 'vad' | 'push_to_talk';
      partials?: boolean;
      hangover_ms?: number;
      language?: string;
    }
  | { type: 'session.commit' }
  | { type: 'session.cancel' }
  | { type: 'text.input'; text: string }
  | { type: 'tool.approve'; call_id: string }
  | { type: 'tool.deny'; call_id: string; reason?: string }
  | { type: 'ping' };
