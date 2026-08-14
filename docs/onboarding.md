# Team onboarding

When a new team joins a shared Nexus instance, three things have
to be in place before a single API call works:

## The network path

Most shared Nexus instances run behind a private ingress: the
console and the gateway are reachable only on the team's network.
A new team needs the gateway URL (an OpenAI-compatible surface,
typically `https://nexus.<domain>/v1`) and the console URL (a
browser-side admin surface). The provider keys belong to the
team, never to the platform operator, so the next step is
**registering the team's provider key** in the console.

## The key surface

The team gets **two keys** to manage: a *provider* key, which pays
the upstream bill and stays inside the platform, and a *virtual*
key (`nxs_live_…`), which authenticates every call through Nexus
and carries the team's policy: which models are allowed, what
the per-minute rate limit is, and how much monthly budget is
attached to this team. The virtual key is what every
application — Cursor, scripts, the team's own services —
authenticates with. The provider key never leaves Nexus.

## The smoke-test

A new team verifies their setup with two requests:

1. `GET /v1/models` — confirms the key resolves and the model
   catalogue is reachable. A `401` means the virtual key is bad
   or revoked; a `403` means the team's policy has not
   authorised the requested model yet.
2. `POST /v1/chat/completions` — one round-trip through the
   gateway is enough to confirm the eval layer attaches to the
   request and a trace ends up on the team's Overview page.

After that, the team works the way it always would — Cursor
with a custom base URL, an OpenAI-shaped SDK with the gateway as
its `base_url`, or anything else that speaks the open protocol.
