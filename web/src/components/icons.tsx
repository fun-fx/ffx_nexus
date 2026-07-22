/**
 * Inline icon set — small SVGs mapped by id. Keeps bundle small vs lucide
 * dependency. Pixel-snapped at 16/20 default size, color via currentColor.
 */

import type { JSX } from "react";

type IconProps = { size?: number; className?: string };

function base(d: JSX.Element, p: IconProps = {}) {
  const { size = 16, className } = p;
  return (
    <svg
      viewBox="0 0 24 24"
      width={size}
      height={size}
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      className={className}
    >
      {d}
    </svg>
  );
}

export const Icon = {
  grid: (p: IconProps) =>
    base(
      <>
        <rect x="3" y="3" width="7" height="7" rx="1.5" />
        <rect x="14" y="3" width="7" height="7" rx="1.5" />
        <rect x="3" y="14" width="7" height="7" rx="1.5" />
        <rect x="14" y="14" width="7" height="7" rx="1.5" />
      </>,
      p,
    ),
  play: (p: IconProps) =>
    base(
      <polygon points="6 4 20 12 6 20 6 4" fill="currentColor" stroke="none" />,
      p,
    ),
  zap: (p: IconProps) =>
    base(
      <polygon points="13 2 3 14 12 14 11 22 21 10 12 10 13 2" />,
      p,
    ),
  chart: (p: IconProps) =>
    base(
      <>
        <path d="M3 3v18h18" />
        <path d="M7 13l3-3 4 4 5-6" />
      </>,
      p,
    ),
  keys: (p: IconProps) =>
    base(
      <>
        <circle cx="8" cy="15" r="4" />
        <path d="M10.85 12.15L19 4" />
        <path d="M18 5l3 3" />
        <path d="M15 8l3 3" />
      </>,
      p,
    ),
  users: (p: IconProps) =>
    base(
      <>
        <circle cx="9" cy="8" r="3.2" />
        <circle cx="17" cy="9" r="2.4" />
        <path d="M3 19c.6-3 3-4.5 6-4.5s5.4 1.5 6 4.5" />
        <path d="M14.5 18.5c.5-1.6 2-2.5 3.5-2.5" />
      </>,
      p,
    ),
  shield: (p: IconProps) =>
    base(
      <path d="M12 3l8 3v5c0 5-3.5 8.5-8 10-4.5-1.5-8-5-8-10V6l8-3z" />,
      p,
    ),
  list: (p: IconProps) =>
    base(
      <>
        <path d="M8 6h13" />
        <path d="M8 12h13" />
        <path d="M8 18h13" />
        <circle cx="4" cy="6" r="1.2" fill="currentColor" />
        <circle cx="4" cy="12" r="1.2" fill="currentColor" />
        <circle cx="4" cy="18" r="1.2" fill="currentColor" />
      </>,
      p,
    ),
  doc: (p: IconProps) =>
    base(
      <>
        <path d="M14 3H6a2 2 0 00-2 2v14a2 2 0 002 2h12a2 2 0 002-2V9z" />
        <path d="M14 3v6h6" />
      </>,
      p,
    ),
  sparkles: (p: IconProps) =>
    base(
      <>
        <path d="M12 3l1.6 4.4L18 9l-4.4 1.6L12 15l-1.6-4.4L6 9l4.4-1.6L12 3z" />
        <path d="M19 14l.8 2.2L22 17l-2.2.8L19 20l-.8-2.2L16 17l2.2-.8L19 14z" />
      </>,
      p,
    ),
  logout: (p: IconProps) =>
    base(
      <>
        <path d="M9 21H5a2 2 0 01-2-2V5a2 2 0 012-2h4" />
        <path d="M16 17l5-5-5-5" />
        <path d="M21 12H9" />
      </>,
      p,
    ),
  copy: (p: IconProps) =>
    base(
      <>
        <rect x="9" y="9" width="11" height="11" rx="2" />
        <path d="M5 15V5a2 2 0 012-2h10" />
      </>,
      p,
    ),
  check: (p: IconProps) =>
    base(<path d="M4 12l5 5L20 6" />, p),
  x: (p: IconProps) =>
    base(
      <>
        <path d="M6 6l12 12" />
        <path d="M18 6L6 18" />
      </>,
      p,
    ),
  menu: (p: IconProps) =>
    base(
      <>
        <path d="M3 6h18" />
        <path d="M3 12h18" />
        <path d="M3 18h18" />
      </>,
      p,
    ),
  google: (p: IconProps) =>
    base(
      <>
        <path d="M21 12c0 4.97-3.582 9-8 9-4.418 0-8-4.03-8-9s3.582-9 8-9c2.4 0 4.6 1.06 6.12 2.74" />
        <path d="M21 12h-9" />
      </>,
      p,
    ),
  arrowRight: (p: IconProps) =>
    base(
      <>
        <path d="M5 12h14" />
        <path d="M13 5l7 7-7 7" />
      </>,
      p,
    ),
  refresh: (p: IconProps) =>
    base(
      <>
        <path d="M3 12a9 9 0 0115-6.7L21 8" />
        <path d="M21 3v5h-5" />
        <path d="M21 12a9 9 0 01-15 6.7L3 16" />
        <path d="M3 21v-5h5" />
      </>,
      p,
    ),
  chevron: (p: IconProps) =>
    base(<path d="M9 6l6 6-6 6" />, p),
  dash: (p: IconProps) =>
    base(<path d="M5 12h14" />, p),
};
