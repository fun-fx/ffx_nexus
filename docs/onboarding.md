# Team onboarding (shared Nexus)

Use this guide when your team runs a **shared** Nexus instance (VPN +
ingress), not the local `install.sh` laptop flow. For a solo dev machine,
start with [`quickstart.md`](quickstart.md) instead.

---

## What you need from your admin

| Item | Example | Why |
| --- | --- | --- |
| **Tailscale** (or VPN) access | Join the org tailnet | Console and gateway are usually private |
| **Console URL** | `https://console.<team-domain>` | Sign up, BYOK, virtual keys, traces |
| **Gateway URL** | `https://nexus.<team-domain>/v1` | OpenAI-compatible API for apps and Cursor |
| **Signup allowed?** | `NEXUS_ALLOW_SIGNUP=true` or invite-only | Determines whether you self-register |

Ask your admin for the exact URLs. Do **not** paste provider API keys into chat or tickets.

---

## 1. Connect to the network

1. Install [Tailscale](https://tailscale.com/download) (or your org VPN client).
2. Sign in with the account your admin invited.
3. Confirm you can open the **console URL** in a browser (HTTPS, no certificate warnings after trust is configured).

If the page does not load, you are not on the VPN or the URL is wrong — contact your admin before continuing.

---

## 2. Create an account and register BYOK

Nexus defaults to **strict BYOK**: your requests use **your** provider key; the operator does not pay for your usage.

1. Open the **console URL**.
2. **Sign in** → **Create account** (if signup is enabled).
3. Fill in email, password (8+ characters), and:
   - **Provider** — OpenAI, Anthropic, Gemini, etc.
   - **Your LLM API key** — encrypted at rest; never shown again in plaintext.
4. Click **Create account**.
5. Copy the **virtual key** (`nxs_live_...`) when shown — **this is the only time** the full secret is displayed. Store it in a password manager.

> **Two keys:** the *provider* key pays the upstream bill. The *virtual* key authenticates to Nexus and carries policy (models, RPM, budget). Apps and Cursor use the **virtual** key only.

### Add more providers later

**Account → My provider keys (BYOK)** — add keys for other providers you want to call.

### Mint keys per app

**Account → My virtual keys** — create separate keys for Cursor, a script, or a teammate's app. Set `allowed_models`, RPM, and monthly budget as needed.

---

## 3. Smoke-test the gateway

Replace placeholders with your admin's gateway host (no trailing slash on the host; path is `/v1`).

```bash
export NEXUS_GATEWAY="https://nexus.<team-domain>"
export NEXUS_VKEY="nxs_live_..."

# List models (expect 200 + JSON)
curl -sS "$NEXUS_GATEWAY/v1/models" \
  -H "Authorization: Bearer $NEXUS_VKEY" | head -c 400; echo

# Chat completion (expect 200; uses YOUR provider key via BYOK)
curl -sS "$NEXUS_GATEWAY/v1/chat/completions" \
  -H "Authorization: Bearer $NEXUS_VKEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-2.5-flash",
    "messages": [{"role": "user", "content": "Say hi in five words"}]
  }' | head -c 600; echo
```

Common errors:

| HTTP | Meaning | Fix |
| --- | --- | --- |
| **401** | Bad or revoked virtual key | Mint a new key in the console |
| **403** | No provider key for that model's provider | Add BYOK for that provider |
| **402** | Monthly budget exceeded | Raise budget or wait for reset |
| **429** | RPM limit | Slow down or ask admin to raise `rpm_limit` |

---

## 4. Connect Cursor IDE

Cursor can route OpenAI-compatible traffic through Nexus so traces appear in **Overview**.

1. Open **Cursor Settings → Models** (or **Features → OpenAI**).
2. Enable **Override OpenAI Base URL** (wording may vary by Cursor version).
3. Set:
   - **Base URL:** `https://nexus.<team-domain>/v1`
   - **API Key:** your `nxs_live_...` virtual key (not your OpenAI/Anthropic key).
4. Pick a model Nexus exposes (e.g. a model you have BYOK for). Use the console **Playground** or `/v1/models` to see what is available.

**Limits:** Cursor's built-in Composer/Pro models often **bypass** custom base URLs. Override works best for flows that hit the OpenAI-compatible API path. Send a test message and confirm a new row appears under **Overview → Recent traces** in the console.

---

## 5. What to expect in the console

| Tab | You see |
| --- | --- |
| **Overview** | Live traces, latency, cost, cache/guardrail flags (your traces only; admins see all) |
| **Account** | BYOK credentials, virtual keys, usage |
| **Playground** | Quick chat against the gateway with your session |
| **Eval** (admin) | Heuristics, judge, routing settings — eval runs without ClickHouse; score history needs ClickHouse |

After a successful gateway call, refresh **Overview**. You should see your request within a few seconds (WebSocket **LIVE** dot in the header).

---

## 6. Python / SDK clients

Same pattern as local quickstart — only the base URL changes:

```python
from openai import OpenAI

client = OpenAI(
    base_url="https://nexus.<team-domain>/v1",
    api_key="nxs_live_...",
)
resp = client.chat.completions.create(
    model="gemini-2.5-flash",
    messages=[{"role": "user", "content": "Hello from SDK."}],
)
print(resp.choices[0].message.content)
```

---

## 7. Troubleshooting

**Console loads but gateway curl fails with connection reset** — you may be hitting the console host on port 443 while the gateway lives on a different hostname. Use the **gateway URL** your admin gave you, not the console URL.

**Traces never appear** — confirm the virtual key on the request matches an active key and the call returned HTTP 200 from the gateway (not blocked by guardrails).

**Eval tab missing** — only **admin** users see **Quality & Routing**. Members use Overview and Account only.

**Still stuck?** Open an issue with trace ID, HTTP status, and model id — never paste provider keys or virtual key secrets.

---

## Related docs

- Local install: [`quickstart.md`](quickstart.md)
- BYOK design: [`byok-multitenancy-design.md`](byok-multitenancy-design.md)
- Env reference: [`../README.md`](../README.md)
