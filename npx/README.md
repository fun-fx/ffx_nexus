# @ffxnexus/nexus

Run the [Nexus](https://github.com/fun-fx/ffx_nexus) LLM gateway on your
machine. One command, no database to install, no config file to write.

```bash
npx -y @ffxnexus/nexus
```

Gateway on `http://localhost:8080`, console on `http://localhost:8081`. Open
the console, create an account, add a provider key, mint a virtual key, then
point any OpenAI client at it:

```bash
export OPENAI_BASE_URL=http://localhost:8080/v1
export OPENAI_API_KEY=nxs_live_...
```

## What this package does

It downloads the `nexus` binary for your platform from the matching GitHub
release, verifies it against the published `checksums.txt`, caches it under
`~/.nexus/bin/<version>/`, and runs it as `nexus serve --local`.

Local mode starts a private Postgres next to the gateway so the console can
actually store the provider keys and virtual keys you create. Everything —
the database, the encryption key — lives in `~/.nexus` and survives restarts.

Any arguments you pass go straight to the binary, so `npx @ffxnexus/nexus
serve` runs without the local database and `npx @ffxnexus/nexus migrate`
reaches the migration command.

## Requirements

macOS or Linux, on x86-64 or arm64, with Node 18+ and `tar`. On any other
platform, use the container image:

```bash
docker run -p 8080:8080 -p 8081:8081 -v "$PWD/data:/app/data" ghcr.io/fun-fx/ffx_nexus
```

## Environment

| Variable | Effect |
| --- | --- |
| `NEXUS_LOCAL_STATE_DIR` | Where the binary cache, database and master key live. Default `~/.nexus`. |
| `NEXUS_RELEASE_BASE_URL` | Fetch the binary from a mirror instead of github.com. |
| `NEXUS_GATEWAY_ADDR` / `NEXUS_CONSOLE_ADDR` | Listen addresses. Default `:8080` / `:8081`. |

Local mode is for one machine. For a cluster, use the Helm chart — see
[the deployment docs](https://github.com/fun-fx/ffx_nexus/blob/main/docs/customer-self-hosted-install.md).
