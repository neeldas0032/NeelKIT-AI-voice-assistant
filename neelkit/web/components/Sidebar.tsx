'use client';

import Link from 'next/link';
import { usePathname, useRouter } from 'next/navigation';
import { useCallback, useEffect, useState } from 'react';
import { api } from '@/lib/api';
import type { Conversation, ModelsResponse, ToolsResponse } from '@/lib/types';
import {
  IconClose,
  IconGrid,
  IconMenu,
  IconPlus,
  IconSearch,
  IconSliders,
  IconWave,
} from './icons';
import { Micro, relativeTime } from './ui';

const LINKS = [
  { href: '/', label: 'Session', Icon: IconWave },
  { href: '/history', label: 'History', Icon: IconSearch },
  { href: '/tools', label: 'Patch bay', Icon: IconGrid },
  { href: '/settings', label: 'Setup', Icon: IconSliders },
];

/**
 * The anchor rail of the workstation.
 *
 * It answers three standing questions without being asked: where am I, what has
 * this machine been doing, and what is it wired to. The third is the one that
 * matters most on a tool that can run commands — the workspace path in the
 * footer is the blast radius, and it should never be more than a glance away.
 *
 * Below 1024px it becomes a drawer. Nothing is dropped in that mode: a phone
 * user still needs to see which directory is exposed.
 */
export function Sidebar() {
  const pathname = usePathname();
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [models, setModels] = useState<ModelsResponse | null>(null);
  const [tools, setTools] = useState<ToolsResponse | null>(null);
  const [ready, setReady] = useState<boolean | null>(null);

  const refresh = useCallback(() => {
    void api
      .conversations(12)
      .then((r) => setConversations(r.conversations ?? []))
      .catch(() => setConversations([]));
  }, []);

  useEffect(refresh, [refresh, pathname]);

  useEffect(() => {
    void api.models().then(setModels).catch(() => setModels(null));
    void api.tools().then(setTools).catch(() => setTools(null));
  }, []);

  // One piece of live state, polled: whether the server behind this page is
  // usable. Everything else assumes it is, so a dead backend has to be visible
  // before someone speaks into a microphone and wonders why nothing happened.
  useEffect(() => {
    let cancelled = false;
    const check = () =>
      api
        .ready()
        .then((r) => !cancelled && setReady(r.ready))
        .catch(() => !cancelled && setReady(false));
    void check();
    const t = setInterval(() => void check(), 20_000);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, []);

  // Close the drawer on navigation, or it stays open over the page it just
  // took you to.
  useEffect(() => setOpen(false), [pathname]);

  const lamp =
    ready === null ? 'var(--text-3)' : ready ? 'var(--live)' : 'var(--danger)';

  return (
    <>
      {/* drawer trigger — below lg only */}
      <button
        onClick={() => setOpen(true)}
        aria-label="Open navigation"
        className="glass fixed left-3 top-3 z-50 flex h-10 w-10 items-center justify-center !rounded-full text-[var(--text-2)] lg:hidden"
      >
        <IconMenu size={18} />
      </button>

      {open && (
        <button
          aria-label="Close navigation"
          onClick={() => setOpen(false)}
          className="fixed inset-0 z-40 bg-black/60 backdrop-blur-sm lg:hidden"
        />
      )}

      <aside
        className={`fixed left-0 top-0 z-50 h-dvh w-[250px] p-2 transition-transform duration-200 lg:translate-x-0 ${
          open ? 'translate-x-0' : '-translate-x-full'
        }`}
      >
        <div className="glass flex h-full flex-col justify-between gap-3 !rounded-[20px] p-3">
          <div className="flex min-h-0 flex-col gap-3">
            {/* maker's mark */}
            <div className="flex items-center justify-between gap-2 px-1">
              <Link href="/" className="flex items-center gap-2">
                <IconWave size={18} className="text-[var(--grad-c)]" />
                <span className="text-[17px] font-semibold tracking-[-0.02em]">
                  Neel<span className="grad-text">KIT</span>
                </span>
              </Link>
              <button
                onClick={() => setOpen(false)}
                aria-label="Close navigation"
                className="text-[var(--text-3)] lg:hidden"
              >
                <IconClose size={16} />
              </button>
            </div>

            <button
              onClick={() => {
                router.push('/');
                router.refresh();
              }}
              className="grad-fill flex w-full items-center justify-center gap-1.5 rounded-full py-1.5 text-[13px] font-medium text-white shadow-[0_0_20px_rgba(124,92,255,0.25)] transition-all hover:brightness-110 active:scale-[0.98]"
            >
              <IconPlus size={15} />
              New session
            </button>

            <nav className="flex flex-col gap-0.5">
              {LINKS.map(({ href, label, Icon }) => {
                const active =
                  href === '/' ? pathname === '/' : pathname.startsWith(href);
                return (
                  <Link
                    key={href}
                    href={href}
                    aria-current={active ? 'page' : undefined}
                    className={`flex items-center gap-2.5 rounded-lg px-2 py-1.5 text-[13px] transition-colors ${
                      active
                        ? 'border-l-2 border-[var(--grad-b)] bg-[var(--glass-hi)] font-medium text-[var(--text)]'
                        : 'border-l-2 border-transparent text-[var(--text-2)] hover:bg-[var(--glass)] hover:text-[var(--text)]'
                    }`}
                  >
                    <Icon size={15} />
                    {label}
                  </Link>
                );
              })}
            </nav>

            {/* Real conversations, not a decorative session list. */}
            <div className="flex min-h-0 flex-col gap-1 pt-1">
              <Micro className="px-2">Recent sessions</Micro>
              <div className="flex min-h-0 flex-col gap-0.5 overflow-y-auto">
                {conversations.length === 0 ? (
                  <span className="px-2 py-1 text-[12px] faint">
                    nothing recorded yet
                  </span>
                ) : (
                  conversations.map((c) => (
                    <Link
                      key={c.id}
                      href="/history"
                      className="group flex flex-col rounded-lg px-2 py-1 text-[var(--text-2)] transition-colors hover:bg-[var(--glass)] hover:text-[var(--text)]"
                    >
                      <span className="truncate text-[12.5px]">
                        {c.title || 'untitled'}
                      </span>
                      <span className="font-mono text-[10px] faint">
                        {relativeTime(c.updated_at)}
                        {c.message_count ? ` · ${c.message_count}` : ''}
                      </span>
                    </Link>
                  ))
                )}
              </div>
            </div>
          </div>

          {/* What this machine is wired to. */}
          <div className="inset-surface flex flex-col gap-1.5 p-2.5">
            <Row
              label="ASR"
              value={models?.asr.model ?? '—'}
              color="var(--grad-c)"
            />
            <Row
              label="LLM"
              value={
                models?.llm.provider && models.llm.provider !== 'none'
                  ? `${models.llm.provider}::${models.llm.model}`
                  : 'none'
              }
              color={
                models?.llm.provider && models.llm.provider !== 'none'
                  ? 'var(--grad-b)'
                  : 'var(--text-3)'
              }
            />
            <div className="flex items-center gap-1.5 border-t border-[var(--border)] pt-1.5">
              <span className="pip" style={{ color: lamp }} aria-hidden />
              <span
                className="truncate font-mono text-[10px]"
                style={{ color: 'var(--text-3)' }}
                title={tools?.workspace}
              >
                {tools?.workspace ?? 'workspace unknown'}
              </span>
            </div>
          </div>
        </div>
      </aside>
    </>
  );
}

function Row({
  label,
  value,
  color,
}: {
  label: string;
  value: string;
  color: string;
}) {
  return (
    <div className="flex items-baseline justify-between gap-2">
      <Micro>{label}</Micro>
      <span
        className="truncate font-mono text-[10px]"
        style={{ color }}
        title={value}
      >
        {value}
      </span>
    </div>
  );
}
