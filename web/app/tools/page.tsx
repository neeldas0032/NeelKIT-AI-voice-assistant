'use client';

import { useEffect, useState } from 'react';
import {
  Empty,
  ErrorNote,
  Inset,
  Micro,
  Output,
  Panel,
  RiskBadge,
  Spec,
  Spinner,
  riskColor,
} from '@/components/ui';
import { api } from '@/lib/api';
import type { ToolDescriptor, ToolsResponse } from '@/lib/types';

export default function ToolsPage() {
  const [data, setData] = useState<ToolsResponse | null>(null);
  const [error, setError] = useState<string>();
  const [open, setOpen] = useState<string | null>(null);

  const load = () => {
    setError(undefined);
    void api
      .tools()
      .then(setData)
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  };

  useEffect(load, []);

  // Grouped by namespace so the page reads like the registry: fs, git, notes,
  // search, shell — the same prefixes that appear in every audit row.
  const grouped = (data?.tools ?? []).reduce<Record<string, ToolDescriptor[]>>(
    (acc, t) => {
      const ns = t.name.split('.')[0];
      (acc[ns] ||= []).push(t);
      return acc;
    },
    {},
  );

  return (
    <div className="mx-auto flex w-full max-w-[1100px] flex-col gap-3 pt-12 lg:pt-0">
      <div className="pt-1">
        <h1 className="text-[22px] font-semibold tracking-[-0.02em]">
          Patch bay
        </h1>
        <p className="mt-1 max-w-2xl text-[13.5px] leading-relaxed dim">
          Every capability wired into this assistant. It is the same list the
          language model receives — nothing is hidden from this page and offered
          to the model separately.
        </p>
      </div>

      {error && <ErrorNote error={error} onRetry={load} />}

      {data && (
        <div className="glass grid gap-x-8 gap-y-2 px-4 py-3 sm:grid-cols-3">
          <Spec label="Workspace" value={data.workspace} />
          <Spec
            label="Mode"
            value={data.read_only ? 'read only' : 'read / write'}
            color={data.read_only ? 'var(--live)' : 'var(--warn)'}
          />
          <Spec label="Registered" value={`${data.tools.length} tools`} />
        </div>
      )}

      {!data && !error && <Spinner label="Reading registry" />}

      {data && data.tools.length === 0 && (
        <Empty title="nothing registered" hint="Check the server's plugin list." />
      )}

      {Object.entries(grouped)
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([ns, tools]) => (
          <Panel key={ns} title={ns} pad={false}>
            <ul>
              {tools.map((t) => (
                <li
                  key={t.name}
                  className="border-b border-[var(--border)] px-4 py-3 last:border-0"
                >
                  <div className="flex flex-wrap items-center gap-2">
                    <span
                      className="pip"
                      style={{ color: riskColor(t.risk) }}
                      aria-hidden
                    />
                    <span className="text-[14px] font-medium">{t.title}</span>
                    <code className="font-mono text-[11px] faint">{t.name}</code>
                    <span className="flex-1" />
                    {t.idempotent && <Micro>retryable</Micro>}
                    <RiskBadge risk={t.risk} />
                  </div>

                  <p className="mt-1 max-w-3xl text-[13px] leading-relaxed dim">
                    {t.description}
                  </p>

                  {t.examples?.length ? (
                    <p className="mt-2 flex items-baseline gap-2">
                      <Micro>say</Micro>
                      <span
                        className="font-mono text-[12px]"
                        style={{ color: 'var(--grad-c)' }}
                      >
                        “{t.examples[0].utterance}”
                      </span>
                    </p>
                  ) : null}

                  <button
                    onClick={() => setOpen(open === t.name ? null : t.name)}
                    aria-expanded={open === t.name}
                    className="micro mt-2 transition-colors hover:text-[var(--text-2)]"
                  >
                    {open === t.name ? '− schema' : '+ schema'}
                  </button>
                  {open === t.name && (
                    <div className="mt-2 max-w-3xl">
                      <Output text={JSON.stringify(t.schema, null, 2)} />
                    </div>
                  )}
                </li>
              ))}
            </ul>
          </Panel>
        ))}

      <Inset className="p-4">
        <Micro>How a capability is added</Micro>
        <p className="mt-1.5 max-w-3xl text-[13px] leading-relaxed dim">
          A tool is a package with an <code className="font-mono">init()</code>{' '}
          that registers itself, plus one import line in{' '}
          <code className="font-mono">cmd/neelkitd/plugins.go</code>. Nothing in
          the assistant, the planner, the API or this page changes — the same
          pattern the standard library uses for database drivers.
        </p>
      </Inset>
    </div>
  );
}
