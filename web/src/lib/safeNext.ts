/**
 * safeNext constrains a post-login redirect target to a same-origin relative
 * path.
 *
 * Two call sites need this and they are not equally exposed. RequireAuth
 * builds `?next=` from the location the user was already on, so its input is
 * trustworthy. Login *reads* `?next=` off the query string, which an attacker
 * supplies directly by handing someone a crafted login link — that is the
 * sink that actually matters. Both funnel through here so the weaker guard
 * cannot drift away from the stronger one, which is exactly what happened
 * before: two copies, both checking for "//" and neither checking for a
 * backslash.
 *
 * The backslash is the whole point. GHSA-wrjc-x8rr-h8h6 and
 * GHSA-jjmj-jmhj-qwj2 escape react-router's relative-path handling with a
 * backslash rather than a second slash, and "/\evil.com" satisfies both
 * "starts with /" and "does not start with //" while browsers normalise it to
 * "//evil.com" — an external origin. A guard that only rejects "//" reads
 * like protection and is not.
 *
 * Rejecting a backslash anywhere in the path is stricter than the advisories
 * require. That is deliberate: no legitimate console route contains one, so
 * the strictness costs nothing and does not depend on knowing every place a
 * browser or router will normalise the character.
 *
 * This is a check at the sink, so it holds regardless of the react-router
 * version underneath. It and the dependency bump are independent fixes for
 * the same exposure; neither makes the other redundant.
 */
export function safeNext(path: string | null | undefined): string | null {
  if (!path) return null;
  // Must be root-relative. Anything else is either absolute (scheme or
  // host) or resolves against the current directory, and neither is a
  // console route.
  if (!path.startsWith("/")) return null;
  // Protocol-relative: "//host" is an external origin.
  if (path.startsWith("//")) return null;
  // Backslash: normalised to "/" by browsers, so "/\host" becomes "//host".
  if (path.includes("\\")) return null;
  // Control characters, including the encoded newline/tab that some parsers
  // strip before resolving. Stripping happens after our check, so a target
  // that looks inert here can become "//host" by the time it is used.
  if (/[\u0000-\u001f\u007f]/.test(path)) return null;
  return path;
}
