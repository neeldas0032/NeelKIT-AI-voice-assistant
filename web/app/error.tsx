'use client';

import { useEffect } from 'react';
import { ErrorNote } from '@/components/ui';

/**
 * The last line of defence for a rendering bug.
 *
 * Without this, any exception thrown while rendering — one unexpected null in
 * a server response was enough — unmounts the whole app and leaves a blank
 * "this page couldn't load" screen with no way back but a manual reload. Here
 * the sidebar and layout survive, the error is shown in words, and one click
 * re-renders the page. The server, the session and the history are unaffected
 * either way; only the view failed.
 */
export default function PageError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  useEffect(() => {
    console.error(error);
  }, [error]);

  return (
    <div className="mx-auto flex w-full max-w-2xl flex-col gap-3 pt-16">
      <ErrorNote
        error={`This view hit an error and stopped: ${error.message}. Your session and history are safe.`}
        onRetry={reset}
      />
    </div>
  );
}
