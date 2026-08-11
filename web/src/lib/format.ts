// formatTokens renders a token count in compact metric notation —
// "842", "49.1K", "1.23M" — matching how the vendor usage dashboards
// operators compare us against present the same numbers.
//
// The exact count is not lost: every call site puts it in a title=
// tooltip. Precision is chosen so the string stays roughly four
// characters wide regardless of magnitude, which keeps the right-aligned
// Tokens column from jittering as rows refresh.
export function formatTokens(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return "0";
  if (n < 1_000) return String(Math.round(n));
  if (n < 1_000_000) return `${(n / 1_000).toFixed(1)}K`;
  return `${(n / 1_000_000).toFixed(2)}M`;
}

// formatExact is the tooltip companion to formatTokens: the unrounded
// count with thousands separators, for when someone needs the real number.
export function formatExact(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return "0";
  return Math.round(n).toLocaleString("en-US");
}

// formatTimestampShort renders a wall-clock timestamp in a compact
// narrower-than-toLocaleString form so a 140-px column also won't wrap
// "Aug 11, 2026, 6:51:24 PM" across five lines. The choice is:
//   - same calendar day    → "HH:MM AM/PM"
//   - one calendar day earlier (yesterday) → "yesterday • HH:MM"
//   - same calendar year   → "Aug 11 • HH:MM AM/PM"
//   - older                → "Aug 11, 2025 • HH:MM AM/PM"
//
// The full timestamp is preserved on the cell's title= attribute so a
// hover still reveals seconds + the exact date.
export function formatTimestampShort(iso: string, now: Date = new Date()): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const HHMM = d.toLocaleTimeString(undefined, {
    hour: "numeric",
    minute: "2-digit",
  });
  const sameDay = isSameCalendarDay(d, now);
  if (sameDay) return HHMM;
  const yesterday = new Date(now);
  yesterday.setDate(now.getDate() - 1);
  if (isSameCalendarDay(d, yesterday)) return `yesterday • ${HHMM}`;
  const monthDay = d.toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
  });
  const sameYear = d.getFullYear() === now.getFullYear();
  if (sameYear) return `${monthDay} • ${HHMM}`;
  return `${monthDay}, ${d.getFullYear()} • ${HHMM}`;
}

function isSameCalendarDay(a: Date, b: Date): boolean {
  return (
    a.getFullYear() === b.getFullYear() &&
    a.getMonth() === b.getMonth() &&
    a.getDate() === b.getDate()
  );
}
