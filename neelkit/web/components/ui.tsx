import type { ReactNode } from 'react';
import type { Risk } from '@/lib/types';

/**
 * Shared primitives.
 *
 * Every one of these is a shape from the design system rather than a wrapper
 * around a Tailwind class string. The test for putting something here is
 * whether naming it removes a decision from the call site: `<Micro>` means
 * nobody has to remember that section labels are 10px mono at 0.12em, and
 * nothing drifts to 11px in one corner of the app.
 */

/* ── type ─────────────────────────────────────────────────────────────── */

export function Micro({
  children,
  className = '',
}: {
  children: ReactNode;
  className?: string;
}) {
  return <span className={`micro ${className}`}>{children}</span>;
}

/* ── surfaces ─────────────────────────────────────────────────────────── */

export function Panel({
  title,
  action,
  children,
  className = '',
  pad = true,
}: {
  title?: ReactNode;
  action?: ReactNode;
  children: ReactNode;
  className?: string;
  pad?: boolean;
}) {
  return (
    <section className={`glass ${className}`}>
      {title && (
        <header className="flex items-center justify-between gap-3 border-b border-[var(--border)] px-4 py-2.5">
          <Micro>{title}</Micro>
          {action}
        </header>
      )}
      <div className={pad ? 'p-4' : ''}>{children}</div>
    </section>
  );
}

/** A recessed readout. Terminal output, transcripts, metrics. */
export function Inset({
  children,
  className = '',
}: {
  children: ReactNode;
  className?: string;
}) {
  return <div className={`inset-surface ${className}`}>{children}</div>;
}

/* ── controls ─────────────────────────────────────────────────────────── */

type ButtonVariant = 'signature' | 'secondary' | 'ghost' | 'danger';

const BUTTON: Record<ButtonVariant, string> = {
  // The gradient is the loudest thing available, so it marks exactly one action
  // per surface — the one the user came here to take.
  signature:
    'grad-fill text-white shadow-[0_0_20px_rgba(124,92,255,0.28)] hover:brightness-110',
  secondary:
    'bg-[var(--glass)] border border-[var(--border)] text-[var(--text)] hover:border-[var(--border-hi)] hover:bg-[var(--glass-hi)]',
  ghost: 'text-[var(--text-2)] hover:bg-[var(--glass)] hover:text-[var(--text)]',
  danger:
    'bg-transparent border border-[color-mix(in_oklab,var(--danger)_40%,transparent)] text-[var(--danger)] hover:bg-[color-mix(in_oklab,var(--danger)_12%,transparent)]',
};

export function Button({
  children,
  onClick,
  variant = 'secondary',
  size = 'md',
  disabled,
  title,
  type = 'button',
}: {
  children: ReactNode;
  onClick?: () => void;
  variant?: ButtonVariant;
  size?: 'sm' | 'md';
  disabled?: boolean;
  title?: string;
  type?: 'button' | 'submit';
}) {
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      title={title}
      className={`inline-flex items-center justify-center gap-1.5 rounded-full font-medium transition-all active:scale-[0.98] disabled:pointer-events-none disabled:opacity-40 ${
        size === 'sm' ? 'px-3 py-1 text-[12px]' : 'px-4 py-1.5 text-[14px]'
      } ${BUTTON[variant]}`}
    >
      {children}
    </button>
  );
}

export function Keycap({ children }: { children: ReactNode }) {
  return <span className="keycap">{children}</span>;
}

export function Chip({
  children,
  active,
  onClick,
  color,
}: {
  children: ReactNode;
  active?: boolean;
  onClick?: () => void;
  color?: string;
}) {
  const tint = color ?? 'var(--grad-b)';
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={!onClick}
      className="inline-flex h-7 items-center gap-1.5 rounded-full px-3 text-[12px] transition-colors disabled:pointer-events-none"
      style={
        active
          ? {
              border: `1px solid ${tint}`,
              background: `color-mix(in oklab, ${tint} 14%, transparent)`,
              color: 'var(--text)',
            }
          : {
              border: '1px solid var(--inset-border)',
              background: 'var(--inset)',
              color: 'var(--text-2)',
            }
      }
    >
      {children}
    </button>
  );
}

/* ── telemetry ────────────────────────────────────────────────────────── */

/**
 * Risk is the single most important thing on screen, so it is the one place a
 * colour is allowed to mean something on its own — and it is always paired with
 * the word, because colour alone is not an accessible signal.
 */
export function riskColor(risk: Risk): string {
  switch (risk) {
    case 'read':
      return 'var(--live)';
    case 'write':
      return 'var(--warn)';
    case 'execute':
      return 'var(--danger)';
    case 'network':
      return 'var(--grad-c)';
  }
}

export function RiskBadge({ risk }: { risk: Risk }) {
  const c = riskColor(risk);
  return (
    <span
      className="inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 font-mono text-[10px] font-semibold uppercase tracking-[0.12em]"
      style={{
        color: c,
        background: `color-mix(in oklab, ${c} 12%, transparent)`,
        border: `1px solid color-mix(in oklab, ${c} 30%, transparent)`,
      }}
    >
      <span className="pip" style={{ color: c }} aria-hidden />
      {risk}
    </span>
  );
}

/** A label, a dotted leader, and a value — one line of a spec sheet. */
export function Spec({
  label,
  value,
  color,
}: {
  label: string;
  value: ReactNode;
  color?: string;
}) {
  return (
    <div className="flex items-baseline gap-2">
      <Micro>{label}</Micro>
      <span className="min-w-4 flex-1 border-b border-dotted border-[var(--border)]" />
      <span
        className="mono-sm truncate"
        style={{ color: color ?? 'var(--text)' }}
      >
        {value}
      </span>
    </div>
  );
}

/** One large figure with a caption. The unit of the telemetry rail. */
export function Stat({
  label,
  value,
  unit,
  note,
  color,
}: {
  label: string;
  value: string;
  unit?: string;
  note?: string;
  color?: string;
}) {
  return (
    <Inset className="flex flex-col gap-0.5 p-3">
      <Micro>{label}</Micro>
      <div className="flex items-baseline gap-1.5">
        <span className="code-stat" style={{ color: color ?? 'var(--text)' }}>
          {value}
        </span>
        {unit && <span className="mono-sm faint">{unit}</span>}
      </div>
      {note && <span className="micro truncate">{note}</span>}
    </Inset>
  );
}

/* ── states ───────────────────────────────────────────────────────────── */

export function Spinner({ label = 'Working' }: { label?: string }) {
  return (
    <div className="flex items-center gap-2.5 p-4">
      <span
        className="h-3 w-3 rounded-full border-2 border-[var(--border)] border-t-[var(--grad-b)] orbit"
        aria-hidden
      />
      <Micro>{label}</Micro>
    </div>
  );
}

export function Empty({ title, hint }: { title: string; hint?: string }) {
  return (
    <div className="flex flex-col items-center gap-1.5 px-4 py-10 text-center">
      <Micro>{title}</Micro>
      {hint && (
        <p className="max-w-sm text-[13px] leading-relaxed dim">{hint}</p>
      )}
    </div>
  );
}

export function ErrorNote({
  error,
  onRetry,
}: {
  error: string;
  onRetry?: () => void;
}) {
  return (
    <div
      role="alert"
      className="flex flex-wrap items-center gap-3 rounded-2xl px-4 py-3"
      style={{
        background: 'color-mix(in oklab, var(--danger) 10%, transparent)',
        border: '1px solid color-mix(in oklab, var(--danger) 28%, transparent)',
      }}
    >
      <span className="pip" style={{ color: 'var(--danger)' }} aria-hidden />
      <span className="flex-1 text-[13px]" style={{ color: 'var(--danger)' }}>
        {error}
      </span>
      {onRetry && (
        <Button size="sm" variant="ghost" onClick={onRetry}>
          retry
        </Button>
      )}
    </div>
  );
}

/**
 * Captured output. Always recessed, always monospaced, always scrollable rather
 * than clipped — a truncated stack trace is worse than no stack trace, because
 * it looks complete.
 */
export function Output({ text }: { text: string }) {
  if (!text.trim()) return null;
  return (
    <Inset className="max-h-72 overflow-auto p-3">
      <pre className="code-block whitespace-pre-wrap break-words dim">
        {text}
      </pre>
    </Inset>
  );
}

/* ── time ─────────────────────────────────────────────────────────────── */

export function relativeTime(unixSeconds: number): string {
  const s = Math.max(0, Math.floor(Date.now() / 1000 - unixSeconds));
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  if (s < 604800) return `${Math.floor(s / 86400)}d ago`;
  return new Date(unixSeconds * 1000).toLocaleDateString();
}

/** hh:mm:ss.mmm — the timestamp format on every execution block header. */
export function clockTime(ms: number = Date.now()): string {
  const d = new Date(ms);
  const p = (n: number, w = 2) => String(n).padStart(w, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(
    d.getMilliseconds(),
    3,
  )}`;
}
