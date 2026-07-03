# Nexus Quickstart

Five minutes from `curl ... | bash` to your first chat completion. Runs
entirely on your laptop or server — **no SaaS lock-in**, no account on
nexus.ffx.ai required. You only need Docker and Git.

> **TL;DR**
> ```bash
> curl -fsSL https://raw.githubusercontent.com/fun-fx/ffx_nexus/main/scripts/install.sh | bash
> # or, once DNS is wired up:
> # curl -fsSL https://install.nexus.ffx.ai | bash
> # then open http://localhost:8091 in your browser
> ```

---

## 0. Prerequisites

| Tool | Why | Install |
| --- | --- | --- |
| **Git** | Clone the repo | <https://git-scm.com/downloads> |
| **Docker** (Desktop or Engine) + `docker compose` v2 | Postgres / Redis / ClickHouse / (optional) Ollama | <https://docs.docker.com/get-docker/> |
| **Go 1.22+** | Build the Nexus binary | <https://go.dev/dl/> |
| ~3 GB free RAM | ClickHouse + Ollama are the heaviest components | — |

A `curl` and a modern browser (Chrome / Safari / Edge) are assumed.

> **No LLM provider key is required up front.** Nexus starts in zero-
> dependency mode and will load your provider key from `.env` (env mode)
> or from the encrypted credential store (BYOK mode).

---

## 1. Install

### One-line installer (recommended)

```bash
curl -fsSL https://install.nexus.ffx.ai | bash
```

The installer detects your OS, clones the repo to `~/.nexus/src`, starts
the dev stack via `docker compose`, builds the Go binary, and launches
Nexus in the background. When complete, it prints:

```
Console (UI):  http://localhost:8091
Gateway (API): http://localhost:8090
```

Open the console in your browser. Continue with step 2.

### Manual install (if you prefer to see what's happening)

```bash
# 1. Clone
git clone https://github.com/fun-fx/ffx_nexus.git
cd ffx_nexus

# 2. Start the dev stack (Postgres, Redis, ClickHouse, Ollama)
docker compose -f deploy/docker-compose.yml up -d postgres redis clickhouse ollama

# 3. Wait for the consoles to be ready (~30s)
curl -fsS http://localhost:8123/ping

# 4. Build the gateway binary
go build -o ./bin/nexus ./cmd/nexus

# 5. Pick at least one provider key from .env, then launch
export GEMINI_API_KEY=sk-...      # or OPENAI_API_KEY, ANTHROPIC_API_KEY, ...
./bin/nexus
```

Console: <http://localhost:8091>
Gateway: <http://localhost:8090>

---

## 2. Create your first account

1. Open <http://localhost:8091>.
2. Click **Sign in** → **Create account**.
3. Fill in:
   * **email** + **password** (8 characters or more).
   * **provider** dropdown — pick whichever LLM provider you use
     (Gemini / OpenAI / Anthropic / ...).
   * **your LLM API key** — paste the key from your provider. Nexus
     encrypts it at rest with AES-GCM under `NEXUS_MASTER_KEY` and never
     logs it in plaintext.
4. Click **Create account**.
5. The next screen shows a **virtual key** (`nxs_live_...`). This is the
   only time it is shown — copy it to a safe place (1Password, your
   shell's secret manager, etc.).

> **Why two keys?** Your *provider* key pays the upstream LLM bill and
> stays in the provider's account. Your *virtual* key authenticates
> requests to Nexus and carries policy (allowed models, budget, RPM).
> Apps use the virtual key — never the provider key.

---

## 3. Validate the key

```bash
curl http://localhost:8090/v1/models \
  -H "Authorization: Bearer nxs_live_..."
```

You should see a JSON list of available models. If you get a 401, the
key was mistyped — re-create it from the **Account → My virtual keys**
panel.

---

## 4. Make your first chat completion

```bash
curl http://localhost:8090/v1/chat/completions \
  -H "Authorization: Bearer nxs_live_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-2.5-flash",
    "messages": [{"role": "user", "content": "Say hi in five words"}]
  }'
```

If you have the OpenAI / Anthropic Python SDK already installed, swap
the base URL instead of altering the request shape:

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8090/v1",  # ← Nexus, not OpenAI
    api_key="nxs_live_...",
)
resp = client.chat.completions.create(
    model="gemini-2.5-flash",
    messages=[{"role": "user", "content": "Hello, Nexus."}],
)
print(resp.choices[0].message.content)
```

---

## 5. Watch your traffic in real time

Go back to <http://localhost:8091> and look at:

* **Cards** (top) — requests (1h), error rate, avg & p95 latency, cache
  hit rate, guardrail events, tokens, cost.
* **Model routing** table — per-model effective quality / safety /
  latency / cost.
* **Eval scores (24h)** table — heuristic + external judge scores.
* **Recent traces** table — every request, with `cache`, `blocked`, and
  `byok` flags where applicable. Refreshes every 5 s; the **LIVE** dot
  in the top right indicates the WebSocket is connected.

> **Try this:** send the same chat completion twice in a row. The second
> should come back in tens of milliseconds (the latency column drops) and
> show a `cache` badge. That is the **semantic cache** kicking in.

---

## 6. What you can do next

| Your goal | Where in the UI |
| --- | --- |
| Add a second provider key (BYOK) so you can mix models | **Account → My provider keys (BYOK)** |
| Mint a separate virtual key per app or teammate | **Account → My virtual keys** |
| Set monthly budget / RPM per virtual key | API: `POST /api/keys` (`monthly_budget_usd`, `rpm_limit`) |
| Block PII / prune unsafe prompts | Set `NEXUS_GUARDRAILS_ENABLED=true` (Azure-side: see [guardrails](security.md)) |
| See what users actually do (admin only) | **Audit** tab |
| Run the same prompt in dev vs prod | Repeat at the prod ingress URL; same trace shape |

---

## 7. Stop / restart / wipe

```bash
# Stop the gateway only (keeps Postgres/Redis/ClickHouse running)
kill "$(cat ~/.nexus/nexus.pid)"

# Stop the entire dev stack (data preserved)
docker compose -f ~/.nexus/src/deploy/docker-compose.yml down

# Wipe everything (Postgres data, ClickHouse traces, etc.)
docker compose -f ~/.nexus/src/deploy/docker-compose.yml down -v
rm -rf ~/.nexus
```

---

## Troubleshooting

**`docker daemon is not running — start Docker and retry`** — Open
Docker Desktop (or `systemctl start docker` on Linux) and run the
installer again.

**`scripts/install.sh` exits with code 40 / "go toolchain missing"** —
Install Go 1.22+ (`brew install go` on macOS) and rerun.

**Browser shows the UI but `/v1/models` returns 401** — your virtual key
is wrong or was revoked. Mint a new one in **Account → My virtual
keys**.

**Request returns 403 `no API key registered for provider X`** — your
account is in `strict_byok` mode and you haven't added a provider key
for that provider yet. Add one in **Account → My provider keys (BYOK)**.

**Gateway stays on /healthz but requests are slow** — this is almost
always the first call caching an embedding or compiling a model. From
the second request onward latency should drop.

For anything else, see <https://github.com/fun-fx/ffx_nexus/issues>.
