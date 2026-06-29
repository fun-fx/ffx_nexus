# Security headers and per-IP rate limiting (v1.1 PR 5)

Status: **CURRENT** — implemented in PR 5 of the v1.1 release.

This document explains the two protections added to the console HTTP server
in preparation for `app.nexus.ffx.ai` (the public ingress in PR 4/6):

1. **Security headers** on every response.
2. **Per-IP rate limit** on the anonymous auth routes.

The implementation lives in:

- `internal/console/security.go` — middleware + `clientIP` helper.
- `internal/limiter/iplimiter.go` — in-memory token-bucket limiter, separate
  from the per-virtual-key `Limiter` that lives next to it.
- `internal/console/server.go` — wiring in `Server.Mux()`.
- `internal/console/security_test.go`, `internal/limiter/iplimiter_test.go` —
  unit tests.

---

## 1. Security headers

Applied globally by `Server.securityHeaders` middleware, **before** CORS and
`withUser`, so they appear on every response (including 401/403/404/429):

| Header | Value | Why |
|---|---|---|
| `Strict-Transport-Security` | `max-age=63072000; includeSubDomains; preload` | Force HTTPS for two years. `preload` opts into browser baked-in HSTS lists. |
| `X-Frame-Options` | `DENY` | Disallow embedding the console in an iframe (clickjacking). |
| `X-Content-Type-Options` | `nosniff` | Stop MIME sniffing. |
| `Referrer-Policy` | `strict-origin-when-cross-origin` | Only send the origin (not the path) on cross-origin nav. |
| `Content-Security-Policy` | see below | Lock down where scripts, styles, and connections can come from. |

CSP:

```
default-src 'self';
connect-src 'self' https://nexus.ffx.ai https://app.nexus.ffx.ai wss://app.nexus.ffx.ai;
style-src 'self' 'unsafe-inline';
script-src 'self';
img-src 'self' data:;
font-src 'self' data:;
frame-ancestors 'none'
```

Notes:
- `connect-src` includes the marketing site (`nexus.ffx.ai`) so the
  marketing → console login handoff works once PR 6 lands.
- `wss://app.nexus.ffx.ai` is the live-trace WebSocket on the public host.
- `style-src 'unsafe-inline'` is required by the dashboard's inline
  `<style>` blocks (Vite emits them by default). Future v1.x may add a nonce.
- `frame-ancestors 'none'` is the modern equivalent of `X-Frame-Options: DENY`
  and is included for browsers that ignore `X-Frame-Options` in favour of CSP.

---

## 2. Per-IP rate limiting

The `Limiter` interface in `internal/limiter/limiter.go` is per-virtual-key
and protects authenticated spend. That interface is the wrong shape for
protecting **unauthenticated** login/register/SSO routes, where we don't
have a key to key against yet. So we added `IPLimiter`:

- Token bucket, capacity 30, refill 30/min, per key.
- Key format: `"<route>:<ip>"` — e.g. `"login:203.0.113.7"`.
- Separate buckets per route so an attacker hammering `/api/auth/login`
  cannot exhaust `/api/auth/register` or `/api/auth/sso/login` and lock
  out legitimate signups / SSO.
- In-memory, per-replica. With a 3-replica deployment the effective limit
  is 90/min/IP, not 30/min/IP. That is acceptable for v1.1 (small clusters,
  small attack surface). If abuse shows up, a Redis-backed variant can be
  added later — the `IPLimiter` API is small enough to swap.

### Where it is applied

| Route | Bucket key | Limit |
|---|---|---|
| `POST /api/auth/login` | `login:<ip>` | 30/min/IP |
| `POST /api/auth/register` | `register:<ip>` | 30/min/IP |
| `GET  /api/auth/sso/login` | `sso-login:<ip>` | 30/min/IP |
| `GET  /api/auth/sso/callback` | `sso-callback:<ip>` | 30/min/IP |

Other anonymous routes (`GET /api/auth/config`, `GET /healthz`) are
deliberately not limited — `/healthz` is hit by every probe, and
`/api/auth/config` is a tiny read used by the login page on every load.

### Source IP resolution

`clientIP(r)` returns:

1. `CF-Connecting-IP` header if present (Cloudflare).
2. `X-Forwarded-For` (leftmost) if present (other reverse proxies).
3. `r.RemoteAddr` (host part) as a last resort.

We deliberately do **not** trust arbitrary client-supplied headers for
non-proxied deployments. Behind Tailscale Funnel or Cloudflare Tunnel
(see PR 4) the proxy sets one of the headers above, so the source IP is
always the real client.

### 429 response

When a bucket is empty, the route returns:

```
HTTP/1.1 429 Too Many Requests
Retry-After: 60
Content-Type: application/json

{"error":"rate limit exceeded; try again later"}
```

---

## 3. What is NOT in v1.1 (deferred)

- **Global per-IP limit on all unauthenticated traffic.** Only the auth
  routes are limited, per design doc §6.4. `/healthz` etc. are excluded
  on purpose.
- **CAPTCHA on signup.** Low priority; revisit if abuse appears.
- **2FA / WebAuthn.** v2.
- **Redis-backed IPLimiter.** Per-replica is enough for v1.1.
- **Per-org rate limit on authenticated traffic.** Already covered by
  the per-virtual-key Limiter (RPM, monthly budget).

---

## 4. Testing

```
go test ./internal/limiter/ -v       # IPLimiter unit tests
go test ./internal/console/ -v       # middleware integration tests
```

`internal/console/security_test.go` covers:
- All five headers present on both 200 and 404 responses.
- 30 login attempts from one IP are accepted (bucket not exhausted).
- 31st attempt returns 429 with `Retry-After`.
- Different IPs are isolated.
- Different routes on the same IP are isolated.
- `clientIP` prefers `CF-Connecting-IP`, falls back to `X-Forwarded-For`,
  then to `RemoteAddr`.

---

## 5. Operational notes

- The limiters are created in `NewServer` and live for the process's
  lifetime. They do not survive a restart, which is fine — a cold start
  gives every IP a fresh 30-request budget.
- They do not appear in `/api/stats` or `/healthz`. No operator needs to
  know the count; a sudden spike of 429s in the gateway log is the
  signal.
- To temporarily lift the limit (e.g. during a load test), set the
  limit to 0 — see `NewIPLimiter(0, time.Minute)`, which short-circuits
  to "always allow". A config flag for this can be added if the need
  becomes recurring.
