'use client';

import { useEffect, useRef, useState } from 'react';
import { SPECTRUM_BANDS, type ConnectionState } from '@/lib/session';
import { IconMic, IconMicOff } from './icons';
import { Keycap, Micro } from './ui';

/**
 * The voice dock: orb, live spectrum, partial transcript, text fallback.
 *
 * This is the only always-present control in the application, and it has one
 * job beyond starting capture — making it unambiguous whether the machine is
 * listening right now. Every signal here is driven by real audio: the bars are
 * frequency bins off the AnalyserNode, the ring follows server-side voice
 * activity detection, and the timecode counts real elapsed capture. A dock that
 * animates while the microphone is closed would be worse than a static one,
 * because it answers the question wrongly rather than not at all.
 */
export function VoiceDock({
  state,
  speaking,
  partial,
  spectrum,
  onConnect,
  onListen,
  onStop,
  onSubmit,
}: {
  state: ConnectionState;
  speaking: boolean;
  partial: string;
  spectrum: Float32Array;
  onConnect: () => void;
  onListen: () => void;
  onStop: () => void;
  onSubmit: (text: string) => void;
}) {
  const [text, setText] = useState('');
  const listening = state === 'listening';
  const connected = state === 'connected' || listening;
  const elapsed = useElapsed(listening);

  const primary = () => {
    if (listening) onStop();
    else if (connected) onListen();
    else onConnect();
  };

  const orbLabel = listening
    ? 'Stop listening'
    : connected
      ? 'Start listening'
      : 'Connect to neelkitd';

  return (
    <div className="glass flex flex-col gap-3 p-3 sm:p-4">
      <div className="flex items-center gap-4">
        {/* ── the orb ─────────────────────────────────────────────────── */}
        <div className="relative flex shrink-0 items-center justify-center">
          {listening && (
            <>
              <span
                className="absolute -inset-2 rounded-full opacity-40 blur-lg grad-fill"
                aria-hidden
              />
              <span
                className="breathe absolute -inset-0.5 rounded-full"
                style={{
                  boxShadow: `0 0 0 2px ${
                    speaking ? 'var(--live)' : 'rgba(255,255,255,0.14)'
                  }`,
                }}
                aria-hidden
              />
            </>
          )}
          <button
            onClick={primary}
            aria-label={orbLabel}
            aria-pressed={listening}
            className={`relative flex h-20 w-20 flex-col items-center justify-center rounded-full transition-all duration-300 hover:scale-[1.04] active:scale-95 sm:h-24 sm:w-24 ${
              connected
                ? 'grad-fill text-white shadow-[0_0_32px_rgba(124,92,255,0.35)]'
                : 'border border-[var(--border)] bg-[var(--inset)] text-[var(--text-3)]'
            }`}
          >
            {listening ? <IconMic size={30} /> : <IconMicOff size={30} />}
            <span className="mt-0.5 font-mono text-[9px] font-semibold uppercase tracking-[0.12em] opacity-90">
              {listening ? 'live 16k' : connected ? 'armed' : 'offline'}
            </span>
          </button>
        </div>

        {/* ── status + spectrum ───────────────────────────────────────── */}
        <div className="flex min-w-0 flex-1 flex-col gap-2">
          <div className="flex items-center justify-between gap-3">
            <div className="flex min-w-0 items-center gap-2">
              <Micro>Streaming STT</Micro>
              <span className="truncate text-[13px]">
                {partial ? (
                  <>
                    {partial}
                    <span className="breathe ml-0.5 inline-block text-[var(--grad-c)]">
                      ▋
                    </span>
                  </>
                ) : (
                  <span className="faint">
                    {listening
                      ? 'waiting for speech…'
                      : connected
                        ? 'press the orb to open the microphone'
                        : 'not connected'}
                  </span>
                )}
              </span>
            </div>
            <div className="flex shrink-0 items-center gap-3">
              <span
                className="font-mono text-[10px] font-semibold uppercase tracking-[0.12em]"
                style={{
                  color: speaking ? 'var(--live)' : 'var(--text-3)',
                }}
              >
                vad {speaking ? 'active' : 'idle'}
              </span>
              <span className="hidden font-mono text-[11px] faint sm:inline">
                {elapsed}
              </span>
            </div>
          </div>

          <Spectrum bands={spectrum} live={listening} />

          <div className="flex flex-wrap items-center justify-between gap-2">
            <div className="flex items-center gap-1.5">
              <Micro>Enter</Micro>
              <Keycap>⏎</Keycap>
              <Micro>sends · esc clears</Micro>
            </div>
            <form
              onSubmit={(e) => {
                e.preventDefault();
                const t = text.trim();
                if (!t) return;
                onSubmit(t);
                setText('');
              }}
              className="inset-surface flex w-full items-center gap-2 px-3 py-1.5 sm:w-80"
            >
              <input
                value={text}
                onChange={(e) => setText(e.target.value)}
                onKeyDown={(e) => e.key === 'Escape' && setText('')}
                placeholder="or type a command…"
                aria-label="Type a command"
                className="w-full bg-transparent font-mono text-[12px] outline-none placeholder:text-[var(--text-3)]"
              />
              <button
                type="submit"
                disabled={!text.trim()}
                className="shrink-0 rounded-full px-2 py-0.5 font-mono text-[10px] font-semibold uppercase tracking-[0.12em] text-[var(--grad-c)] transition-opacity disabled:opacity-30"
              >
                Send
              </button>
            </form>
          </div>
        </div>
      </div>
    </div>
  );
}

/**
 * 32 frequency bands, painted across the signature spectrum left to right.
 *
 * Height is clamped to a visible minimum so the bar row keeps its shape in
 * silence rather than collapsing to a line — the display should read as an
 * instrument at rest, not as a broken one.
 */
function Spectrum({ bands, live }: { bands: Float32Array; live: boolean }) {
  return (
    <div className="inset-surface flex h-10 items-end gap-[3px] px-2 py-1.5">
      {Array.from({ length: SPECTRUM_BANDS }, (_, i) => {
        const v = live ? (bands[i] ?? 0) : 0;
        const pct = 8 + v * 92;
        const t = i / (SPECTRUM_BANDS - 1);
        const color =
          t < 0.5
            ? mix('var(--grad-a)', 'var(--grad-b)', t * 2)
            : mix('var(--grad-b)', 'var(--grad-c)', (t - 0.5) * 2);
        return (
          <span
            key={i}
            className="flex-1 rounded-full transition-[height] duration-75"
            style={{
              height: `${pct}%`,
              background: color,
              opacity: live ? 0.45 + v * 0.55 : 0.18,
            }}
            aria-hidden
          />
        );
      })}
      <span className="sr-only">
        {live ? 'Microphone is open' : 'Microphone is closed'}
      </span>
    </div>
  );
}

function mix(a: string, b: string, t: number): string {
  return `color-mix(in oklab, ${b} ${Math.round(t * 100)}%, ${a})`;
}

/** Capture timecode at 100ms resolution. */
function useElapsed(running: boolean): string {
  const [ms, setMs] = useState(0);
  const start = useRef(0);

  useEffect(() => {
    if (!running) {
      setMs(0);
      return;
    }
    start.current = Date.now();
    const t = setInterval(() => setMs(Date.now() - start.current), 100);
    return () => clearInterval(t);
  }, [running]);

  const total = Math.floor(ms / 100);
  const m = String(Math.floor(total / 600)).padStart(2, '0');
  const s = String(Math.floor((total % 600) / 10)).padStart(2, '0');
  return `${m}:${s}.${total % 10}`;
}
