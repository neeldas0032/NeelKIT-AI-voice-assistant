'use client';

import { useCallback, useEffect, useState } from 'react';
import { IconSearch } from '@/components/icons';
import {
  Button,
  Empty,
  ErrorNote,
  Micro,
  Panel,
  Spinner,
  relativeTime,
} from '@/components/ui';
import { api } from '@/lib/api';
import type { Conversation, Message, TranscriptHit } from '@/lib/types';

export default function HistoryPage() {
  const [conversations, setConversations] = useState<Conversation[] | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [messages, setMessages] = useState<Message[] | null>(null);
  const [query, setQuery] = useState('');
  const [hits, setHits] = useState<TranscriptHit[] | null>(null);
  const [error, setError] = useState<string>();

  const load = useCallback(async () => {
    setError(undefined);
    try {
      const r = await api.conversations();
      setConversations(r.conversations ?? []);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setConversations([]);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    if (!selected) {
      setMessages(null);
      return;
    }
    let cancelled = false;
    setMessages(null);
    void api
      .messages(selected)
      .then((r) => !cancelled && setMessages(r.messages ?? []))
      .catch((e) => !cancelled && setError(String(e)));
    return () => {
      cancelled = true;
    };
  }, [selected]);

  // Debounced. The index is local and fast, but a request per keystroke still
  // produces out-of-order responses that make the result list flicker.
  useEffect(() => {
    const q = query.trim();
    if (!q) {
      setHits(null);
      return;
    }
    const t = setTimeout(() => {
      void api
        .search(q)
        .then((r) => setHits(r.results ?? []))
        .catch(() => setHits([]));
    }, 220);
    return () => clearTimeout(t);
  }, [query]);

  const remove = async (id: string) => {
    try {
      await api.deleteConversation(id);
      if (selected === id) setSelected(null);
      await load();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };

  return (
    <div className="mx-auto flex w-full max-w-[1300px] flex-col gap-3 pt-12 lg:pt-0">
      <div className="flex flex-wrap items-center gap-3 pt-1">
        <h1 className="text-[22px] font-semibold tracking-[-0.02em]">History</h1>
        <span className="flex-1" />
        <div className="inset-surface flex w-full items-center gap-2 px-3 py-2 sm:w-80">
          <IconSearch size={14} className="shrink-0 text-[var(--text-3)]" />
          <input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="search everything you have said…"
            aria-label="Search transcripts"
            className="w-full bg-transparent font-mono text-[12px] outline-none placeholder:text-[var(--text-3)]"
          />
        </div>
      </div>

      {error && <ErrorNote error={error} onRetry={load} />}

      {hits !== null ? (
        <Panel
          title={`${hits.length} match${hits.length === 1 ? '' : 'es'}`}
          pad={false}
          action={
            <Button size="sm" variant="ghost" onClick={() => setQuery('')}>
              clear
            </Button>
          }
        >
          {hits.length === 0 ? (
            <Empty
              title="no match"
              hint="Search covers every final transcript. Partial words work — typing “auth” finds “authentication”."
            />
          ) : (
            <ul>
              {hits.map((h) => (
                <li
                  key={h.id}
                  className="flex gap-3 border-b border-[var(--border)] px-4 py-3 last:border-0"
                >
                  <span className="w-14 shrink-0 pt-0.5 font-mono text-[10px] uppercase tracking-[0.1em] faint">
                    {relativeTime(h.created_at)}
                  </span>
                  <div className="min-w-0 flex-1">
                    <p className="text-[13.5px] leading-snug">
                      {highlight(h.snippet || h.text)}
                    </p>
                    <p className="mt-1 flex flex-wrap gap-x-3 font-mono text-[10px] uppercase tracking-[0.1em] faint">
                      {h.confidence > 0 && (
                        <span>conf {Math.round(h.confidence * 100)}%</span>
                      )}
                      {h.conversation_id && (
                        <button
                          className="transition-colors hover:text-[var(--grad-c)]"
                          onClick={() => {
                            setQuery('');
                            setSelected(h.conversation_id!);
                          }}
                        >
                          open thread →
                        </button>
                      )}
                    </p>
                  </div>
                </li>
              ))}
            </ul>
          )}
        </Panel>
      ) : (
        <div className="grid gap-3 lg:grid-cols-[minmax(0,320px)_1fr]">
          <Panel title="Threads" pad={false}>
            {conversations === null ? (
              <Spinner />
            ) : conversations.length === 0 ? (
              <Empty
                title="nothing recorded"
                hint="Everything said in a session is kept here."
              />
            ) : (
              <ul>
                {conversations.map((c) => (
                  <li
                    key={c.id}
                    className="border-b border-[var(--border)] last:border-0"
                  >
                    <div
                      className="group flex items-center gap-2.5 px-4 py-2.5"
                      style={
                        selected === c.id
                          ? { background: 'var(--glass-hi)' }
                          : undefined
                      }
                    >
                      <span
                        className="pip"
                        style={{
                          color:
                            selected === c.id
                              ? 'var(--grad-c)'
                              : 'var(--text-3)',
                          opacity: selected === c.id ? 1 : 0.4,
                        }}
                        aria-hidden
                      />
                      <button
                        onClick={() => setSelected(c.id)}
                        className="min-w-0 flex-1 text-left"
                      >
                        <p className="truncate text-[13px]">
                          {c.title || 'untitled'}
                        </p>
                        <p className="mt-0.5 font-mono text-[10px] uppercase tracking-[0.1em] faint">
                          {relativeTime(c.updated_at)}
                          {c.message_count ? ` · ${c.message_count} lines` : ''}
                        </p>
                      </button>
                      <button
                        onClick={() => void remove(c.id)}
                        aria-label={`Delete ${c.title || 'thread'}`}
                        className="micro shrink-0 opacity-0 transition-opacity hover:text-[var(--danger)] focus-visible:opacity-100 group-hover:opacity-100"
                      >
                        del
                      </button>
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </Panel>

          <Panel
            title={selected ? 'Transcript' : 'No thread selected'}
            pad={false}
          >
            {!selected ? (
              <Empty
                title="pick a thread"
                hint="Deleting one removes what was said. The audit record of anything that ran on your machine is kept."
              />
            ) : messages === null ? (
              <Spinner />
            ) : messages.length === 0 ? (
              <Empty title="empty thread" />
            ) : (
              <ul className="flex flex-col gap-3 p-4">
                {messages.map((m) => (
                  <li key={m.id} className="flex gap-3">
                    <span
                      className="w-9 shrink-0 pt-0.5 text-right font-mono text-[10px] uppercase tracking-[0.1em]"
                      style={{
                        color:
                          m.role === 'user'
                            ? 'var(--grad-c)'
                            : m.role === 'tool'
                              ? 'var(--live)'
                              : 'var(--text-2)',
                      }}
                    >
                      {m.role === 'user' ? 'in' : m.role === 'tool' ? 'run' : 'out'}
                    </span>
                    <div className="min-w-0 flex-1">
                      <p
                        className={`leading-snug ${
                          m.role === 'tool'
                            ? 'font-mono text-[12px] dim'
                            : 'text-[13.5px]'
                        }`}
                      >
                        {m.content}
                      </p>
                      <p className="mt-0.5 font-mono text-[10px] uppercase tracking-[0.1em] faint">
                        {m.source} · {relativeTime(m.created_at)}
                      </p>
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </Panel>
        </div>
      )}

      <Micro className="px-1">
        transcripts are indexed locally with sqlite fts5 · nothing is uploaded
      </Micro>
    </div>
  );
}

/** FTS5 returns matches wrapped in brackets; render those as highlights. */
function highlight(snippet: string) {
  return snippet.split(/(\[[^\]]*\])/g).map((p, i) =>
    p.startsWith('[') && p.endsWith(']') ? (
      <mark
        key={i}
        className="rounded-[2px] px-0.5"
        style={{
          background: 'color-mix(in oklab, var(--grad-c) 26%, transparent)',
          color: 'var(--text)',
        }}
      >
        {p.slice(1, -1)}
      </mark>
    ) : (
      <span key={i}>{p}</span>
    ),
  );
}
