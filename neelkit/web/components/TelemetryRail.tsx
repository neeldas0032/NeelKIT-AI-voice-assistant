'use client';

import type { CallResult, ModelsResponse, Turn } from '@/lib/types';
import { Inset, Micro, Stat, riskColor } from './ui';

export interface AsrSample {
  /** Wall-clock the recogniser spent producing the final transcript. */
  latencyMs: number;
  /** Length of the audio it was given. */
  durationMs: number;
  confidence: number;
}

export interface SessionStats {
  lastTurn: Turn | null;
  lastAsr: AsrSample | null;
  calls: CallResult[];
  grammarTurns: number;
  llmTurns: number;
}

/**
 * The telemetry rail.
 *
 * Every figure here is measured by the server and carried on the wire — turn
 * latency, per-call durations, transcript confidence, audio length. None of it
 * is synthesised for the sake of filling a panel: if a number is not being
 * measured, the panel that would have shown it is not here. That is a harder
 * rule to follow than it sounds, because a dashboard with four cards looks more
 * impressive than one with three, and it is the only version of this rail worth
 * putting in front of somebody who will ask where a number came from.
 */
export function TelemetryRail({
  stats,
  models,
  live,
}: {
  stats: SessionStats;
  models: ModelsResponse | null;
  live: boolean;
}) {
  const { lastTurn, lastAsr, calls } = stats;

  const toolMs = lastTurn
    ? (lastTurn.calls ?? []).reduce((n, c) => n + (c.duration_ms || 0), 0)
    : 0;
  const routeMs = lastTurn ? Math.max(0, lastTurn.latency_ms - toolMs) : 0;
  const asrMs = lastAsr?.latencyMs ?? 0;
  const totalMs = asrMs + (lastTurn?.latency_ms ?? 0);

  // Real-time factor: compute time over audio length. The headline number for
  // any streaming recogniser — below 1.0 means it can keep up with speech, and
  // the target here is far below that because the decode has to finish inside
  // the pause at the end of a sentence.
  const rtf =
    lastAsr && lastAsr.durationMs > 0
      ? lastAsr.latencyMs / lastAsr.durationMs
      : null;

  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center justify-between">
        <Micro>Telemetry inspector</Micro>
        <span className="flex items-center gap-1.5">
          <span
            className={live ? 'pip breathe' : 'pip'}
            style={{ color: live ? 'var(--live)' : 'var(--text-3)' }}
            aria-hidden
          />
          <span
            className="font-mono text-[10px] font-semibold uppercase tracking-[0.12em]"
            style={{ color: live ? 'var(--live)' : 'var(--text-3)' }}
          >
            {live ? 'live' : 'idle'}
          </span>
        </span>
      </div>

      {/* ── end-to-end latency ────────────────────────────────────────── */}
      <Inset className="flex flex-col gap-2 p-3">
        <div className="flex items-baseline justify-between">
          <Micro>Last turn</Micro>
          <span className="font-mono text-[13px]">
            {lastTurn ? `${Math.round(totalMs)} ms` : '—'}
          </span>
        </div>

        <div className="flex h-2 w-full overflow-hidden rounded-full bg-[rgba(255,255,255,0.05)]">
          {totalMs > 0 && (
            <>
              <Seg ms={asrMs} total={totalMs} color="var(--grad-c)" label="ASR decode" />
              <Seg ms={routeMs} total={totalMs} color="var(--grad-b)" label="Route + plan" />
              <Seg ms={toolMs} total={totalMs} color="var(--grad-a)" label="Tool execution" />
            </>
          )}
        </div>

        <div className="flex flex-col gap-0.5">
          <Legend color="var(--grad-c)" label="ASR decode" ms={asrMs} />
          <Legend color="var(--grad-b)" label="Route + plan" ms={routeMs} />
          <Legend color="var(--grad-a)" label="Tool exec" ms={toolMs} />
        </div>

        {lastTurn && (
          <span className="micro">
            target &lt; 1000 ms for conversational turn-taking
          </span>
        )}
      </Inset>

      {/* ── the two headline figures ──────────────────────────────────── */}
      <div className="grid grid-cols-2 gap-2">
        <Stat
          label="RTF"
          value={rtf === null ? '—' : `${rtf.toFixed(2)}×`}
          note={
            rtf === null
              ? 'speak to measure'
              : `${(1 / rtf).toFixed(1)}× faster than real time`
          }
          color={rtf !== null && rtf < 0.35 ? 'var(--live)' : 'var(--text)'}
        />
        <Stat
          label="Confidence"
          value={
            lastAsr ? `${Math.round(lastAsr.confidence * 100)}%` : '—'
          }
          note={lastAsr ? 'last final transcript' : 'no audio yet'}
          color={
            lastAsr && lastAsr.confidence < 0.7
              ? 'var(--warn)'
              : 'var(--grad-c)'
          }
        />
      </div>

      {/* ── routing tier: the architectural claim, measured ───────────── */}
      <Inset className="flex flex-col gap-2 p-3">
        <div className="flex items-baseline justify-between">
          <Micro>Resolved by</Micro>
          <span className="font-mono text-[10px] faint">this session</span>
        </div>
        <TierBar
          grammar={stats.grammarTurns}
          llm={stats.llmTurns}
        />
        <span className="micro">
          grammar costs zero tokens and never leaves the process
        </span>
      </Inset>

      {/* ── what the recogniser is ────────────────────────────────────── */}
      <Inset className="flex flex-col gap-1.5 p-3">
        <Micro>Compute</Micro>
        <KV k="Device" v={models?.asr.device ?? '—'} />
        <KV k="Precision" v={models?.asr.precision ?? '—'} />
        <KV k="Decoder workers" v={models ? String(models.asr.workers) : '—'} />
      </Inset>

      {/* ── what actually ran ─────────────────────────────────────────── */}
      <Inset className="flex flex-col gap-1.5 p-3">
        <div className="flex items-baseline justify-between">
          <Micro>Executed</Micro>
          <span className="font-mono text-[10px] faint">
            {calls.length} {calls.length === 1 ? 'call' : 'calls'}
          </span>
        </div>
        {calls.length === 0 ? (
          <span className="text-[12px] faint">nothing has run yet</span>
        ) : (
          calls
            .slice(-6)
            .reverse()
            .map((c) => (
              <div
                key={c.call_id}
                className="flex items-center gap-2 truncate"
                title={c.preview}
              >
                <span
                  className="pip"
                  style={{ color: riskColor(c.risk) }}
                  aria-hidden
                />
                <code className="truncate font-mono text-[11px] dim">
                  {c.tool}
                </code>
                <span className="flex-1" />
                <span className="font-mono text-[10px] faint">
                  {c.duration_ms} ms
                </span>
              </div>
            ))
        )}
      </Inset>
    </div>
  );
}

function Seg({
  ms,
  total,
  color,
  label,
}: {
  ms: number;
  total: number;
  color: string;
  label: string;
}) {
  if (ms <= 0) return null;
  return (
    <span
      style={{ width: `${(ms / total) * 100}%`, background: color }}
      title={`${label}: ${Math.round(ms)} ms`}
    />
  );
}

function Legend({
  color,
  label,
  ms,
}: {
  color: string;
  label: string;
  ms: number;
}) {
  return (
    <div className="flex items-center gap-2">
      <span
        className="h-1.5 w-1.5 shrink-0 rounded-full"
        style={{ background: color }}
        aria-hidden
      />
      <span className="micro flex-1 !normal-case !tracking-normal">
        {label}
      </span>
      <span className="font-mono text-[10px] dim">
        {ms > 0 ? `${Math.round(ms)} ms` : '—'}
      </span>
    </div>
  );
}

function TierBar({ grammar, llm }: { grammar: number; llm: number }) {
  const total = grammar + llm;
  const pct = total === 0 ? 0 : (grammar / total) * 100;
  return (
    <>
      <div className="flex h-2 overflow-hidden rounded-full bg-[rgba(255,255,255,0.05)]">
        <span
          style={{ width: `${pct}%`, background: 'var(--live)' }}
          title={`grammar: ${grammar}`}
        />
        <span
          style={{ width: `${100 - pct}%`, background: 'var(--grad-b)' }}
          title={`language model: ${llm}`}
        />
      </div>
      <div className="flex items-center justify-between font-mono text-[10px]">
        <span style={{ color: 'var(--live)' }}>grammar {grammar}</span>
        <span style={{ color: 'var(--grad-b)' }}>model {llm}</span>
      </div>
    </>
  );
}

function KV({ k, v }: { k: string; v: string }) {
  return (
    <div className="flex items-baseline justify-between gap-2">
      <Micro>{k}</Micro>
      <span className="font-mono text-[11px] dim">{v}</span>
    </div>
  );
}
