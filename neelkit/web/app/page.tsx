'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { ApprovalCard } from '@/components/ApprovalCard';
import { TelemetryRail, type AsrSample, type SessionStats } from '@/components/TelemetryRail';
import { VoiceDock } from '@/components/VoiceDock';
import { IconCheck, IconCopy, IconMic, IconTerminal } from '@/components/icons';
import {
  Chip,
  ErrorNote,
  Inset,
  Micro,
  Output,
  RiskBadge,
  clockTime,
  riskColor,
} from '@/components/ui';
import { api, getApiKey } from '@/lib/api';
import { RealtimeSession, SPECTRUM_BANDS, type ConnectionState } from '@/lib/session';
import type { CallResult, ServerEvent, Turn, Word } from '@/lib/types';

type Entry =
  | {
      kind: 'user';
      id: string;
      at: number;
      text: string;
      confidence?: number;
      words?: Word[];
      asrMs?: number;
      audioMs?: number;
      typed?: boolean;
    }
  | { kind: 'assistant'; id: string; at: number; turn: Turn }
  | { kind: 'approval'; id: string; call: CallResult }
  | { kind: 'result'; id: string; at: number; call: CallResult }
  | { kind: 'note'; id: string; at: number; text: string };

export default function SessionPage() {
  const [state, setState] = useState<ConnectionState>('idle');
  const [detail, setDetail] = useState<string>();
  const [entries, setEntries] = useState<Entry[]>([]);
  const [partial, setPartial] = useState('');
  const [speaking, setSpeaking] = useState(false);
  const [spectrum, setSpectrum] = useState(new Float32Array(SPECTRUM_BANDS));
  const [mode, setMode] = useState<'vad' | 'push_to_talk'>('vad');
  const [busyCall, setBusyCall] = useState<string | null>(null);
  const [hasKey, setHasKey] = useState(true);
  const [models, setModels] = useState<Awaited<ReturnType<typeof api.models>> | null>(null);

  const [lastAsr, setLastAsr] = useState<AsrSample | null>(null);
  const [lastTurn, setLastTurn] = useState<Turn | null>(null);
  const [calls, setCalls] = useState<CallResult[]>([]);
  const [tiers, setTiers] = useState({ grammar: 0, llm: 0 });

  const sessionRef = useRef<RealtimeSession | null>(null);
  const logRef = useRef<HTMLDivElement>(null);
  const bottomRef = useRef<HTMLDivElement>(null);

  useEffect(() => setHasKey(Boolean(getApiKey())), []);
  useEffect(() => {
    void api.models().then(setModels).catch(() => setModels(null));
  }, []);

  const push = useCallback((e: Entry) => setEntries((prev) => [...prev, e]), []);

  // Follow the log only when the reader is already at the bottom. Yanking the
  // view down while they are reading an earlier tool result is worse than a
  // missed scroll.
  useEffect(() => {
    const box = logRef.current;
    if (!box) return;
    const nearBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 180;
    if (nearBottom)
      bottomRef.current?.scrollIntoView({ behavior: 'smooth', block: 'end' });
  }, [entries, partial]);

  const handleEvent = useCallback(
    (e: ServerEvent) => {
      switch (e.type) {
        case 'vad.speech_started':
          setSpeaking(true);
          break;
        case 'vad.speech_stopped':
          setSpeaking(false);
          break;
        case 'transcript.partial':
          setPartial(e.text);
          break;
        case 'transcript.final':
          setPartial('');
          if (e.latency_ms && e.duration_ms) {
            setLastAsr({
              latencyMs: e.latency_ms,
              durationMs: e.duration_ms,
              confidence: e.confidence ?? 0,
            });
          }
          push({
            kind: 'user',
            id: e.utterance_id || crypto.randomUUID(),
            at: Date.now(),
            text: e.text,
            confidence: e.confidence,
            words: e.words,
            asrMs: e.latency_ms,
            audioMs: e.duration_ms,
          });
          break;
        case 'assistant.turn':
          setLastTurn(e.turn);
          setTiers((t) =>
            e.turn.intent.source === 'grammar'
              ? { ...t, grammar: t.grammar + 1 }
              : e.turn.intent.source === 'llm'
                ? { ...t, llm: t.llm + 1 }
                : t,
          );
          push({ kind: 'assistant', id: e.turn.turn_id, at: Date.now(), turn: e.turn });
          for (const c of e.turn.calls ?? []) {
            if (c.status !== 'awaiting_approval') {
              setCalls((prev) => [...prev, c]);
              push({ kind: 'result', id: c.call_id, at: Date.now(), call: c });
            }
          }
          break;
        case 'tool.permission_required':
          push({ kind: 'approval', id: e.call.call_id, call: e.call });
          break;
        case 'tool.completed':
          setBusyCall(null);
          setCalls((prev) => [...prev, e.call]);
          setEntries((prev) =>
            prev
              .filter((x) => !(x.kind === 'approval' && x.id === e.call.call_id))
              .concat({
                kind: 'result',
                id: e.call.call_id,
                at: Date.now(),
                call: e.call,
              }),
          );
          break;
        case 'stream.lagged':
          push({
            kind: 'note',
            id: crypto.randomUUID(),
            at: Date.now(),
            text: `${e.dropped} live previews skipped to keep up — the final transcripts are complete`,
          });
          break;
        case 'stream.overloaded':
          push({
            kind: 'note',
            id: crypto.randomUUID(),
            at: Date.now(),
            text: e.message ?? 'recogniser busy',
          });
          break;
        case 'error':
          push({ kind: 'note', id: crypto.randomUUID(), at: Date.now(), text: e.message });
          break;
      }
    },
    [push],
  );

  const connect = useCallback(async () => {
    setDetail(undefined);
    const session = new RealtimeSession({
      mode,
      onEvent: handleEvent,
      onState: (s, d) => {
        setState(s);
        if (d) setDetail(d);
        if (s !== 'listening') setSpeaking(false);
      },
      // A new Float32Array per tick: React bails out of a state update when the
      // reference is unchanged, and the session reuses its buffer.
      onSpectrum: (b) => setSpectrum(new Float32Array(b)),
    });
    sessionRef.current = session;
    try {
      await session.connect();
    } catch (err) {
      setState('error');
      setDetail(err instanceof Error ? err.message : String(err));
    }
  }, [handleEvent, mode]);

  const startListening = useCallback(async () => {
    try {
      await sessionRef.current?.startlistening();
    } catch (err) {
      setDetail(
        err instanceof Error ? err.message : 'The microphone could not start.',
      );
    }
  }, []);

  const stopListening = useCallback(() => sessionRef.current?.stopListening(), []);

  useEffect(() => () => sessionRef.current?.close(), []);

  const approve = useCallback((callId: string) => {
    setBusyCall(callId);
    const s = sessionRef.current;
    if (s?.connected) {
      s.approve(callId);
      return;
    }
    // No socket open (the command was typed): fall back to REST. Both routes
    // reach the same endpoint on the server.
    void api
      .approve(callId)
      .then((call) => {
        setBusyCall(null);
        setCalls((prev) => [...prev, call]);
        setEntries((prev) =>
          prev
            .filter((x) => !(x.kind === 'approval' && x.id === callId))
            .concat({ kind: 'result', id: callId, at: Date.now(), call }),
        );
      })
      .catch((err) => {
        setBusyCall(null);
        setDetail(err instanceof Error ? err.message : String(err));
      });
  }, []);

  const deny = useCallback((callId: string) => {
    const s = sessionRef.current;
    if (s?.connected) s.deny(callId);
    else void api.deny(callId).catch(() => undefined);
    setEntries((prev) =>
      prev.filter((x) => !(x.kind === 'approval' && x.id === callId)),
    );
  }, []);

  const submitTyped = useCallback(
    async (text: string) => {
      push({ kind: 'user', id: crypto.randomUUID(), at: Date.now(), text, typed: true });

      const s = sessionRef.current;
      if (s?.connected) {
        s.sendText(text);
        return;
      }
      try {
        const turn = await api.turn(text);
        setLastTurn(turn);
        setTiers((t) =>
          turn.intent.source === 'grammar'
            ? { ...t, grammar: t.grammar + 1 }
            : turn.intent.source === 'llm'
              ? { ...t, llm: t.llm + 1 }
              : t,
        );
        push({ kind: 'assistant', id: turn.turn_id, at: Date.now(), turn });
        for (const c of turn.calls ?? []) {
          if (c.status === 'awaiting_approval') {
            push({ kind: 'approval', id: c.call_id, call: c });
          } else {
            setCalls((prev) => [...prev, c]);
            push({ kind: 'result', id: c.call_id, at: Date.now(), call: c });
          }
        }
      } catch (err) {
        setDetail(err instanceof Error ? err.message : String(err));
      }
    },
    [push],
  );

  const stats: SessionStats = useMemo(
    () => ({
      lastTurn,
      lastAsr,
      calls,
      grammarTurns: tiers.grammar,
      llmTurns: tiers.llm,
    }),
    [lastTurn, lastAsr, calls, tiers],
  );

  const turnNumber = useRef(0);
  turnNumber.current = 0;

  return (
    /* The workstation fills the viewport: the transcript scrolls inside its own
       panel and the dock stays pinned, so the control that opens the microphone
       is never scrolled off the screen mid-sentence. Below lg the page scrolls
       normally, because a 100dvh layout on a phone fights the URL bar. */
    <div className="mx-auto grid w-full max-w-[1600px] gap-3 pt-12 lg:h-[calc(100dvh-1.25rem)] lg:pt-0 xl:grid-cols-[minmax(0,1fr)_300px]">
      <div className="flex min-h-0 min-w-0 flex-col gap-3">
        {!hasKey && (
          <ErrorNote error="No API key stored. Paste the key neelkitd printed on first run into Setup." />
        )}
        {detail && <ErrorNote error={detail} onRetry={connect} />}

        <StatusStrip
          state={state}
          mode={mode}
          onMode={setMode}
          turns={tiers.grammar + tiers.llm}
        />

        <div
          ref={logRef}
          className="glass flex min-h-[46vh] flex-1 flex-col gap-3 overflow-y-auto p-3 sm:p-4 lg:min-h-0"
        >
          {entries.length === 0 && !partial ? (
            <Welcome onPick={submitTyped} />
          ) : (
            entries.map((e) => {
              switch (e.kind) {
                case 'user':
                  turnNumber.current += 1;
                  return (
                    <UserBlock key={e.id} entry={e} n={turnNumber.current} />
                  );
                case 'assistant':
                  return <AssistantBlock key={e.id} at={e.at} turn={e.turn} />;
                case 'approval':
                  return (
                    <ApprovalCard
                      key={e.id}
                      call={e.call}
                      busy={busyCall === e.id}
                      onApprove={() => approve(e.id)}
                      onDeny={() => deny(e.id)}
                    />
                  );
                case 'result':
                  return <ToolBlock key={e.id + e.at} call={e.call} />;
                case 'note':
                  return (
                    <div
                      key={e.id}
                      className="flex items-center gap-2 px-1 text-[12px]"
                      style={{ color: 'var(--warn)' }}
                    >
                      <span className="pip" aria-hidden />
                      {e.text}
                    </div>
                  );
              }
            })
          )}
          <div ref={bottomRef} />
        </div>

        <VoiceDock
          state={state}
          speaking={speaking}
          partial={partial}
          spectrum={spectrum}
          onConnect={() => void connect()}
          onListen={() => void startListening()}
          onStop={stopListening}
          onSubmit={(t) => void submitTyped(t)}
        />
      </div>

      {/* The rail is supporting detail, not the story. It leaves at 1280px
          rather than squeezing the transcript, which is the thing people
          actually read. */}
      <aside className="hidden min-h-0 overflow-y-auto xl:block">
        <TelemetryRail stats={stats} models={models} live={state === 'listening'} />
      </aside>
    </div>
  );
}

/* ── header strip ───────────────────────────────────────────────────── */

function StatusStrip({
  state,
  mode,
  onMode,
  turns,
}: {
  state: ConnectionState;
  mode: 'vad' | 'push_to_talk';
  onMode: (m: 'vad' | 'push_to_talk') => void;
  turns: number;
}) {
  const bound = state === 'connected' || state === 'listening';
  return (
    <div className="glass flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-2.5">
      <span className="flex items-center gap-2">
        <span
          className={bound ? 'pip breathe' : 'pip'}
          style={{ color: bound ? 'var(--live)' : 'var(--text-3)' }}
          aria-hidden
        />
        <span
          className="font-mono text-[10px] font-semibold uppercase tracking-[0.12em]"
          style={{ color: bound ? 'var(--live)' : 'var(--text-3)' }}
        >
          {state === 'listening'
            ? 'capturing'
            : state === 'connected'
              ? 'session bound'
              : state}
        </span>
      </span>

      <span className="hidden h-4 w-px bg-[var(--border)] sm:block" />

      <span className="flex items-center gap-2">
        <Micro>Turns</Micro>
        <span className="font-mono text-[12px]">{turns}</span>
      </span>

      <span className="flex-1" />

      <span className="flex items-center gap-2">
        <Micro>Endpointing</Micro>
        <Chip
          active={mode === 'vad'}
          onClick={state === 'listening' ? undefined : () => onMode('vad')}
        >
          auto
        </Chip>
        <Chip
          active={mode === 'push_to_talk'}
          onClick={
            state === 'listening' ? undefined : () => onMode('push_to_talk')
          }
        >
          hold
        </Chip>
      </span>
    </div>
  );
}

/* ── execution blocks ───────────────────────────────────────────────── */

/**
 * A spoken turn.
 *
 * The transcript is the only element rendered in a recessed surface at 15px —
 * bigger than the interface around it, because it is the thing the user needs
 * to check before anything runs. Words the recogniser was unsure of carry a
 * dotted underline, which is the cheapest possible way to turn a single
 * misheard token into something the eye catches on.
 */
function UserBlock({
  entry,
  n,
}: {
  entry: Extract<Entry, { kind: 'user' }>;
  n: number;
}) {
  const conf = entry.confidence ?? 0;
  return (
    <article className="seat glass p-4">
      <header className="flex flex-wrap items-center gap-2.5 pb-2.5">
        <span className="rounded bg-[var(--glass-hi)] px-2 py-0.5 font-mono text-[10px] font-semibold uppercase tracking-[0.12em] text-[var(--grad-c)]">
          {String(n).padStart(2, '0')} · user
        </span>
        <span className="font-mono text-[10px] faint">{clockTime(entry.at)}</span>
        {entry.asrMs !== undefined && (
          <span className="font-mono text-[10px] faint">
            | asr {entry.asrMs} ms
          </span>
        )}
        <span className="flex-1" />
        {entry.typed ? (
          <Micro>typed</Micro>
        ) : (
          <span className="flex items-center gap-1.5">
            <IconMic size={13} className="text-[var(--grad-c)]" />
            <span
              className="font-mono text-[10px]"
              style={{ color: conf < 0.7 ? 'var(--warn)' : 'var(--grad-c)' }}
            >
              conf {conf.toFixed(3)}
            </span>
          </span>
        )}
      </header>

      <Inset className="px-4 py-3">
        <p className="font-mono text-[15px] leading-relaxed">
          {entry.words?.length ? (
            entry.words.map((w, i) => (
              <span
                key={i}
                title={`confidence ${w.confidence.toFixed(2)}`}
                className={
                  w.confidence < 0.85
                    ? 'border-b border-dotted border-[var(--warn)]'
                    : undefined
                }
              >
                {w.word}
                {i < entry.words!.length - 1 ? ' ' : ''}
              </span>
            ))
          ) : (
            <>{entry.text}</>
          )}
        </p>
      </Inset>

      {entry.audioMs !== undefined && (
        <footer className="flex flex-wrap items-center gap-x-3 pt-2">
          <Micro>audio {(entry.audioMs / 1000).toFixed(1)}s</Micro>
          <Micro>·</Micro>
          <Micro>
            {entry.words?.length ?? entry.text.split(/\s+/).length} tokens
          </Micro>
        </footer>
      )}
    </article>
  );
}

function AssistantBlock({ at, turn }: { at: number; turn: Turn }) {
  const deterministic = turn.intent.source === 'grammar';
  return (
    <article className="seat glass p-4">
      <header className="flex flex-wrap items-center gap-2.5 pb-2.5">
        <span className="grad-fill rounded px-2 py-0.5 font-mono text-[10px] font-bold uppercase tracking-[0.12em] text-white">
          neelkit
        </span>
        <span className="font-mono text-[10px] faint">{clockTime(at)}</span>
        <span className="font-mono text-[10px] faint">
          | {turn.latency_ms} ms
        </span>
        <span className="flex-1" />
        <span
          className="flex items-center gap-1.5 rounded-full px-2 py-0.5"
          style={{
            background: 'var(--glass-hi)',
            color: deterministic ? 'var(--live)' : 'var(--grad-b)',
          }}
        >
          <span className="pip" aria-hidden />
          <span className="font-mono text-[10px] font-semibold uppercase tracking-[0.12em]">
            {deterministic ? 'deterministic' : turn.intent.source}
          </span>
        </span>
      </header>

      <p className="max-w-3xl text-[15px] leading-relaxed">{turn.message}</p>

      {turn.intent.name && (
        <footer className="flex flex-wrap items-center gap-x-3 gap-y-1 pt-2">
          <Micro>intent</Micro>
          <code className="font-mono text-[11px] dim">{turn.intent.name}</code>
          {turn.intent.pattern && (
            <>
              <Micro>rule</Micro>
              <code className="truncate font-mono text-[11px] faint">
                {turn.intent.pattern}
              </code>
            </>
          )}
        </footer>
      )}
    </article>
  );
}

/**
 * A completed tool call, rendered as a subprocess record rather than a chat
 * message: a terminal header with the exit state, the command that ran, and
 * whatever it printed.
 */
function ToolBlock({ call }: { call: CallResult }) {
  const failed = call.status === 'failed';
  const accent = failed ? 'var(--danger)' : riskColor(call.risk);
  const [copied, setCopied] = useState(false);

  return (
    <div className="seat inset-surface overflow-hidden">
      <header className="flex flex-wrap items-center gap-2.5 border-b border-[var(--inset-border)] bg-[rgba(255,255,255,0.03)] px-3 py-2">
        <IconTerminal size={14} className="text-[var(--text-3)]" />
        <span className="micro">subprocess</span>
        <code className="font-mono text-[11px] dim">{call.tool}</code>
        <span className="flex-1" />
        <span
          className="flex items-center gap-1.5 font-mono text-[10px]"
          style={{ color: accent }}
        >
          <span className="pip" aria-hidden />
          {failed ? 'failed' : call.status}
        </span>
        <span className="font-mono text-[10px] faint">{call.duration_ms} ms</span>
        <RiskBadge risk={call.risk} />
      </header>

      <div className="flex items-center gap-2 px-3 py-2">
        <span className="select-none font-mono text-[12px] faint">$</span>
        <code className="min-w-0 flex-1 truncate font-mono text-[12.5px] dim">
          {call.preview}
        </code>
        <button
          onClick={() => {
            void navigator.clipboard?.writeText(call.preview);
            setCopied(true);
            setTimeout(() => setCopied(false), 1400);
          }}
          className="flex shrink-0 items-center gap-1 text-[var(--text-3)] transition-colors hover:text-[var(--text)]"
          title="Copy command"
        >
          {copied ? <IconCheck size={13} /> : <IconCopy size={13} />}
        </button>
      </div>

      {call.summary && (
        <p className="border-t border-[var(--inset-border)] px-3 py-2 text-[13px]">
          {call.summary}
        </p>
      )}
      {call.error && (
        <p
          className="border-t border-[var(--inset-border)] px-3 py-2 text-[13px]"
          style={{ color: 'var(--danger)' }}
        >
          {call.error}
        </p>
      )}
      {call.stdout && (
        <div className="border-t border-[var(--inset-border)] p-3">
          <Output text={call.stdout} />
        </div>
      )}
    </div>
  );
}

/* ── empty state ────────────────────────────────────────────────────── */

function Welcome({ onPick }: { onPick: (t: string) => void }) {
  const examples = [
    'list the files',
    'what changed since my last commit',
    'create a git branch called feature auth',
    'search my project for authentication logic',
    'make a note to refactor the audio ring buffer',
  ];
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-4 px-4 py-10 text-center">
      <div className="flex flex-col items-center gap-1.5">
        <h1 className="text-[22px] font-semibold tracking-[-0.02em]">
          Speak, and it does the thing.
        </h1>
        <p className="max-w-md text-[13.5px] leading-relaxed dim">
          Reads run immediately. Anything that writes, executes or leaves this
          machine shows you the exact resolved command and waits for you.
        </p>
      </div>
      <div className="flex flex-wrap justify-center gap-1.5">
        {examples.map((e) => (
          <button
            key={e}
            onClick={() => onPick(e)}
            className="rounded-full border border-[var(--inset-border)] bg-[var(--inset)] px-3 py-1.5 font-mono text-[11.5px] text-[var(--text-2)] transition-colors hover:border-[var(--border-hi)] hover:text-[var(--text)]"
          >
            {e}
          </button>
        ))}
      </div>
    </div>
  );
}
