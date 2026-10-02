/**
 * Icons, inline.
 *
 * The reference design pulls Material Symbols from Google Fonts. This product
 * claims that nothing leaves the machine, and an icon font fetched from a CDN
 * on every page load is a request to Google carrying the user's IP and referer.
 * Eight paths inlined here cost about 2 KB and keep the claim true offline.
 */

type P = { className?: string; size?: number };

const base = (size: number) => ({
  width: size,
  height: size,
  viewBox: '0 0 24 24',
  fill: 'none' as const,
  stroke: 'currentColor',
  strokeWidth: 1.6,
  strokeLinecap: 'round' as const,
  strokeLinejoin: 'round' as const,
  'aria-hidden': true,
});

export const IconWave = ({ className, size = 16 }: P) => (
  <svg {...base(size)} className={className}>
    <path d="M3 12h2M8 6v12M12 3v18M16 8v8M20 11h1" />
  </svg>
);

export const IconMic = ({ className, size = 16 }: P) => (
  <svg {...base(size)} className={className}>
    <rect x="9" y="2" width="6" height="11" rx="3" />
    <path d="M5 11a7 7 0 0 0 14 0M12 18v4" />
  </svg>
);

export const IconMicOff = ({ className, size = 16 }: P) => (
  <svg {...base(size)} className={className}>
    <path d="M9 9V5a3 3 0 0 1 6 0v4m0 3a3 3 0 0 1-5 2.2" />
    <path d="M5 11a7 7 0 0 0 10.9 5.8M19 11a7 7 0 0 1-.5 2.6M12 18v4M3 3l18 18" />
  </svg>
);

export const IconPlus = ({ className, size = 16 }: P) => (
  <svg {...base(size)} className={className}>
    <path d="M12 5v14M5 12h14" />
  </svg>
);

export const IconChevron = ({ className, size = 16 }: P) => (
  <svg {...base(size)} className={className}>
    <path d="m9 6 6 6-6 6" />
  </svg>
);

export const IconTerminal = ({ className, size = 16 }: P) => (
  <svg {...base(size)} className={className}>
    <path d="m5 7 4 4-4 4M12 15h7" />
  </svg>
);

export const IconSearch = ({ className, size = 16 }: P) => (
  <svg {...base(size)} className={className}>
    <circle cx="11" cy="11" r="7" />
    <path d="m20 20-3.5-3.5" />
  </svg>
);

export const IconGrid = ({ className, size = 16 }: P) => (
  <svg {...base(size)} className={className}>
    <rect x="3" y="3" width="7" height="7" rx="1.5" />
    <rect x="14" y="3" width="7" height="7" rx="1.5" />
    <rect x="3" y="14" width="7" height="7" rx="1.5" />
    <rect x="14" y="14" width="7" height="7" rx="1.5" />
  </svg>
);

export const IconSliders = ({ className, size = 16 }: P) => (
  <svg {...base(size)} className={className}>
    <path d="M4 6h10M18 6h2M4 12h4M12 12h8M4 18h10M18 18h2" />
    <circle cx="16" cy="6" r="2" />
    <circle cx="10" cy="12" r="2" />
    <circle cx="16" cy="18" r="2" />
  </svg>
);

export const IconClock = ({ className, size = 16 }: P) => (
  <svg {...base(size)} className={className}>
    <circle cx="12" cy="12" r="9" />
    <path d="M12 7v5l3 2" />
  </svg>
);

export const IconMenu = ({ className, size = 16 }: P) => (
  <svg {...base(size)} className={className}>
    <path d="M4 7h16M4 12h16M4 17h16" />
  </svg>
);

export const IconClose = ({ className, size = 16 }: P) => (
  <svg {...base(size)} className={className}>
    <path d="M6 6l12 12M18 6 6 18" />
  </svg>
);

export const IconCheck = ({ className, size = 16 }: P) => (
  <svg {...base(size)} className={className}>
    <path d="m4 12 5 5L20 6" />
  </svg>
);

export const IconShield = ({ className, size = 16 }: P) => (
  <svg {...base(size)} className={className}>
    <path d="M12 3l7 3v5c0 4.5-3 8-7 10-4-2-7-5.5-7-10V6z" />
  </svg>
);

export const IconCopy = ({ className, size = 16 }: P) => (
  <svg {...base(size)} className={className}>
    <rect x="9" y="9" width="11" height="11" rx="2" />
    <path d="M5 15V5a2 2 0 0 1 2-2h8" />
  </svg>
);
