# Providers

## Provider catalog opt-in

By default Nexus runs in `strict_byok`: every caller registers their own
upstream key (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, …) via
`POST /api/credentials`. For admin-only flows — dogfooding, demos, internal
assistants — you can opt in to a *server-side* provider fallback by setting
the matching env var / Kubernetes Secret key. The keys are opt-in
independent of each other, so you can enable The Grid for production without
adding Groq or Mistral.

| Server-side key | Toggle location |
| --- | --- |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` / `GEMINI_API_KEY` | env / Secret entry; both modes (`byok` / `strict_byok`) honoured |
| `GROQ_API_KEY` | env / Secret entry; chat-model ids auto-listed from the catalog |
| `MISTRAL_API_KEY` | env / Secret entry; chat + embedding ids auto-listed |
| `GRID_API_KEY` | env / Secret entry; 9 instrument ids auto-listed. `Authorization` is auto-stripped on cross-origin 307 redirects (security). |

### Opt-in on Kubernetes (Cozystack)

The prod values file uses `existingSecret: nexus`, so the chart's Secret
template is a no-op and changes via `helm upgrade -f` do **not** touch live
credentials. Patch the cluster Secret out-of-band to add a provider key:

```bash
# Login to the prod cluster (Tailscale + kubectl already set up).
kubectl -n tenant-nexus get secret nexus -o jsonpath='{.data}' | jq 'keys'
# → shows current keys (BASE64-encoded). Add one more:

kubectl -n tenant-nexus patch secret nexus --type merge \
  -p '{"stringData":{"GRID_API_KEY":"<grid-api-key-from-app.thegrid.ai>"}}'

# Trigger a rolling restart so the pod picks up the new env:
kubectl -n tenant-nexus rollout restart deployment/nexus

# Verify the provider is registered:
kubectl -n tenant-nexus exec deploy/nexus -- \
  curl -s http://localhost:8080/v1/models | jq '.data[] | select(.id | startswith("grid/"))'
# → lists grid/text-prime, grid/text-max, … once the pod has restarted
```

### Opt-in locally

```bash
# 1. Grab a Grid API key from https://app.thegrid.ai/profile/api-keys
export GRID_API_KEY=...
go run ./cmd/nexus   # nexus boots and registers grid/* in the registry

# 2. Use it via any OpenAI-compatible client:
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer nxs_live_..." \
  -d '{"model":"grid/text-prime","messages":[{"role":"user","content":"hi"}]}'
```

The Grid responds with a `307 Temporary Redirect` to an actual supplier, and
Nexus follows it with the Grid key (Authorization is stripped automatically
if the supplier's host differs from `api.thegrid.ai`).
