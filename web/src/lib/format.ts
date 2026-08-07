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
