'use client';

import { useState } from 'react';
import type { CallResult } from '@/lib/types';
import { IconShield } from './icons';
import { Button, Keycap, Micro, Output, RiskBadge, riskColor } from './ui';

/**
 * The approval card — the most important surface in the application.
 *
 * Everything that can change the machine passes through it, and the only
 * defence against a misheard word or an invented plan is being able to read, in
 * one glance, exactly what will run. So the card is built around that one
 * string: it is the largest type on the card, set in mono, recessed like a
 * terminal readout, and labelled WILL RUN in the plainest words available.
 *
 * It is the only element in the design that carries a coloured halo. That is
 * deliberate — nothing else in the interface asks for a decision, so nothing
 * else is allowed to look like it does.
 */
export function ApprovalCard({
  call,
  onApprove,
  onDeny,
  busy,
}: {
  call: CallResult;
  onApprove: () => void;
  onDeny: () => void;
  busy?: boolean;
}) {
  const [showArgs, setShowArgs] = useState(false);
  const accent = riskColor(call.risk);

  return (
    <div
      className="seat glass overflow-hidden !border-[color-mix(in_oklab,var(--warn)_38%,var(--border))]"
      style={{
        boxShadow: `0 0 0 3px color-mix(in oklab, ${accent} 10%, transparent)`,
      }}
    >
      <header className="flex flex-wrap items-center gap-2.5 border-b border-[var(--border)] px-4 py-2.5">
        <IconShield size={15} className="text-[var(--warn)]" />
        <span className="font-mono text-[11px] font-semibold uppercase tracking-[0.14em] text-[var(--warn)]">
          Approval required
        </span>
        <span className="flex-1" />
        <code className="font-mono text-[11px] dim">{call.tool}</code>
        <RiskBadge risk={call.risk} />
      </header>

      <div className="flex flex-col gap-3 p-4">
        <p className="text-[13px] dim">{call.reason}</p>

        {/* The command, verbatim. A server-side integration test asserts this
            string describes the same argv that actually executes. */}
        <div className="inset-surface px-4 py-3">
          <Micro className="mb-1.5 block">Will run</Micro>
          <code className="block break-words font-mono text-[14px] leading-[1.55] text-[var(--text)]">
            {call.preview}
          </code>
        </div>

        <div className="flex flex-wrap items-center gap-2">
          <Button variant="signature" onClick={onApprove} disabled={busy}>
            {busy ? 'Running…' : 'Approve'}
            <span className="hidden sm:inline">
              <Keycap>⌘⏎</Keycap>
            </span>
          </Button>
          <Button variant="danger" onClick={onDeny} disabled={busy}>
            Deny
          </Button>
          <span className="flex-1" />
          <button
            onClick={() => setShowArgs((v) => !v)}
            aria-expanded={showArgs}
            className="micro transition-colors hover:text-[var(--text-2)]"
          >
            {showArgs ? '− arguments' : '+ arguments'}
          </button>
        </div>

        {showArgs && <Output text={JSON.stringify(call.args, null, 2)} />}
      </div>
    </div>
  );
}
