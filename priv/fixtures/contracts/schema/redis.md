# Redis key and limiter semantics

Source of truth for the Go limiter: `internal/limiter`. There is **no Lua
script** on the hot path today. A rewrite may use Redix + Lua *if* the
observable keys, TTLs, and allow/deny outcomes match this document.

PubSub is **not** the RPM authority. Memory fallback exists for
zero-Redis mode; production RPM across replicas is Redis.

## Key layout

UTC clocks.

| Purpose | Key | Window | TTL |
| --- | --- | --- | --- |
| Requests per minute | `nexus:rpm:{keyID}:{yyyyMMddHHmm}` | calendar minute (`200601021504`) | 2 minutes |
| Monthly spend USD | `nexus:spend:{keyID}:{yyyyMM}` | calendar month (`200601`) | 62 days |

`keyID` is the virtual-key row id, not the plaintext `nxs_live_…` secret.

## Allow (RPM)

If `rpmLimit <= 0`, allow and do not touch Redis.

Otherwise, in one transaction/pipeline:

1. `INCR nexus:rpm:{keyID}:{window}`
2. `EXPIRE` that key `120` seconds
3. Allow iff the post-increment value `<= rpmLimit`

A crash between INCR and EXPIRE can leave an immortal counter; the
rewrite should keep the same pipeline grouping so the blast radius
stays identical.

## Spend

`GET nexus:spend:{keyID}:{month}` → float; missing key is `0`.

`AddSpend` with `costUSD <= 0` is a no-op. Otherwise pipeline:

1. `INCRBYFLOAT` the month key by `costUSD`
2. `EXPIRE` `62 * 24h`

## What must not change in a rewrite

- Window strings are UTC, zero-padded, no separators.
- Prefixes stay `nexus:rpm:` and `nexus:spend:` so a canary on the
  same Redis does not double-count under a new namespace **and** so
  dual-write is obvious if someone adds a second prefix. Dual-write
  from Go and Elixir on one command is forbidden; canary split is at
  the load balancer, one writer per request.
