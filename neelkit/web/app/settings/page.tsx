'use client';

import { useEffect, useState } from 'react';
import {
  Button,
  ErrorNote,
  Micro,
  Panel,
  Spec,
  Spinner,
  riskColor,
} from '@/components/ui';
import { API_URL, api, getApiKey, setApiKey } from '@/lib/api';
import type { ModelsResponse, ReadyResponse, Risk } from '@/lib/types';

export default function SettingsPage() {
  const [key, setKey] = useState('');
  const [saved, setSaved] = useState(false);
  const [reveal, setReveal] = useState(false);
  const [models, setModels] = useState<ModelsResponse | null>(null);
  const [ready, setReady] = useState<ReadyResponse | null>(null);
  const [error, setError] = useState<string>();

  useEffect(() => setKey(getApiKey()), []);

  const refresh = () => {
    setError(undefined);
    void api
      .ready()
      .then(setReady)
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
    void api.models().then(setModels).catch(() => setModels(null));
  };

  useEffect(refresh, []);

  const save = () => {
    setApiKey(key);
    setSaved(true);
    setTimeout(() => setSaved(false), 1800);
    refresh();
  };

  return (
    <div className="mx-auto flex w-full max-w-[1100px] flex-col gap-3 pt-12 lg:pt-0">
      <div className="pt-1">
        <h1 className="text-[22px] font-semibold tracking-[-0.02em]">Setup</h1>
      </div>

      {error && <ErrorNote error={error} onRetry={refresh} />}

      <Panel title="Access key">
        <div className="flex flex-col gap-3">
          <p className="max-w-2xl text-[13px] leading-relaxed dim">
            neelkitd prints a key once, on first run. It is kept in this browser
            only and sent as an{' '}
            <code className="font-mono text-[12px]">Authorization</code> header —
            never as a cookie, so no other page can make requests on your behalf.
          </p>
          <div className="flex flex-col gap-2 sm:flex-row">
            <input
              type={reveal ? 'text' : 'password'}
              value={key}
              onChange={(e) => setKey(e.target.value)}
              placeholder="nk_…"
              aria-label="API key"
              spellCheck={false}
              autoComplete="off"
              className="inset-surface h-11 flex-1 px-3 font-mono text-[13px] outline-none transition-shadow placeholder:text-[var(--text-3)] focus:border-[var(--grad-b)] focus:shadow-[0_0_0_3px_rgba(124,92,255,0.2)]"
            />
            <Button variant="ghost" onClick={() => setReveal((v) => !v)}>
              {reveal ? 'hide' : 'show'}
            </Button>
            <Button variant="signature" onClick={save}>
              {saved ? 'stored' : 'store'}
            </Button>
          </div>
          <Spec label="Endpoint" value={API_URL} />
        </div>
      </Panel>

      <Panel
        title="Service status"
        pad={false}
        action={
          <Button size="sm" variant="ghost" onClick={refresh}>
            re-check
          </Button>
        }
      >
        {!ready ? (
          <Spinner label="Polling" />
        ) : (
          <ul>
            {Object.entries(ready.checks).map(([name, status]) => {
              const good = status === 'ok';
              const off = status.startsWith('disabled');
              const color = good
                ? 'var(--live)'
                : off
                  ? 'var(--text-3)'
                  : 'var(--warn)';
              return (
                <li
                  key={name}
                  className="flex items-center gap-2.5 border-b border-[var(--border)] px-4 py-2.5 last:border-0"
                >
                  <span className="pip" style={{ color }} aria-hidden />
                  <span className="font-mono text-[11px] uppercase tracking-[0.12em]">
                    {name}
                  </span>
                  <span className="min-w-4 flex-1 border-b border-dotted border-[var(--border)]" />
                  <span className="font-mono text-[11px]" style={{ color }}>
                    {status}
                  </span>
                </li>
              );
            })}
          </ul>
        )}
      </Panel>

      {models && (
        <Panel title="Loaded models" pad={false}>
          <div className="grid gap-x-8 gap-y-2 p-4 sm:grid-cols-2">
            <Spec label="Recogniser" value={models.asr.model} />
            <Spec label="Precision" value={models.asr.precision} />
            <Spec label="Device" value={models.asr.device} />
            <Spec label="Decoder workers" value={String(models.asr.workers)} />
            <Spec
              label="Language model"
              value={models.llm.provider || 'none'}
              color={
                models.llm.provider && models.llm.provider !== 'none'
                  ? 'var(--live)'
                  : 'var(--text-2)'
              }
            />
            <Spec label="Tag" value={models.llm.model || '—'} />
          </div>
          <p className="border-t border-[var(--border)] px-4 py-3 text-[13px] leading-relaxed dim">
            With a local language model configured, no audio, transcript or
            source file leaves this machine. The built-in commands work even with
            no model at all.
          </p>
        </Panel>
      )}

      <Panel title="Permission levels" pad={false}>
        <ul>
          {(
            [
              ['read', 'Runs immediately', 'Listing a directory, reading a file, git status — nothing changes.'],
              ['write', 'Asks first', 'Creating a branch, writing a file. You see the exact command before it runs.'],
              ['execute', 'Always asks', 'Running a program. This one cannot be set to run automatically, whatever the settings say.'],
              ['network', 'Asks first', 'Anything that leaves the machine. Loopback and internal addresses are refused outright.'],
            ] as [Risk, string, string][]
          ).map(([risk, rule, detail]) => (
            <li
              key={risk}
              className="flex gap-3 border-b border-[var(--border)] px-4 py-3 last:border-0"
            >
              <span
                className="w-16 shrink-0 pt-0.5 font-mono text-[10px] font-semibold uppercase tracking-[0.12em]"
                style={{ color: riskColor(risk) }}
              >
                {risk}
              </span>
              <div className="min-w-0 flex-1">
                <p className="text-[13.5px] font-medium">{rule}</p>
                <p className="mt-0.5 text-[13px] leading-relaxed dim">{detail}</p>
              </div>
            </li>
          ))}
          <li className="px-4 py-3">
            <Micro>Audit</Micro>
            <p className="mt-1 text-[13px] leading-relaxed dim">
              Every attempt is written to the log before it runs — including the
              ones you deny. Deleting a conversation does not remove it.
            </p>
          </li>
        </ul>
      </Panel>
    </div>
  );
}
