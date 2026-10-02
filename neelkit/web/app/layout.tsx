import type { Metadata, Viewport } from 'next';

// Fonts are self-hosted from npm rather than fetched from a CDN at runtime.
// A local-first tool that phones out to Google for its typeface is not
// local-first, and a build that needs the network is a build that fails on a
// train.
import '@fontsource-variable/geist';
import '@fontsource/jetbrains-mono/400.css';
import '@fontsource/jetbrains-mono/500.css';
import '@fontsource/jetbrains-mono/600.css';

import './globals.css';
import { Sidebar } from '@/components/Sidebar';

export const metadata: Metadata = {
  title: 'NeelKIT',
  description:
    'A local-first voice assistant for developers. Speak, and it does the thing.',
  applicationName: 'NeelKIT',
};

export const viewport: Viewport = {
  width: 'device-width',
  initialScale: 1,
  themeColor: '#08090b',
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body className="min-h-dvh">
        {/* A keyboard user should be able to reach the transcript without
            tabbing through the whole navigation on every page. */}
        <a
          href="#main"
          className="sr-only focus:not-sr-only focus:absolute focus:left-3 focus:top-3 focus:z-50 focus:rounded-full focus:bg-[var(--inset)] focus:px-4 focus:py-2 focus:text-sm focus:outline focus:outline-[var(--grad-b)]"
        >
          Skip to content
        </a>

        <div className="ambient" aria-hidden />

        <Sidebar />

        {/* The rail is fixed, so the document is inset rather than flexed
            beside it. Below 1024px the rail becomes a drawer and the inset
            collapses to zero. */}
        <div className="relative z-10 min-h-dvh lg:pl-[250px]">
          <main id="main" className="px-3 pb-8 pt-3 sm:px-4">
            {children}
          </main>
        </div>
      </body>
    </html>
  );
}
