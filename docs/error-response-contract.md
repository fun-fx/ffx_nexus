# Error response contract — OpenAI vs Nexus

Nexus has **two** error response surfaces. They live next to each other on
purpose, but they do NOT share a body shape because the audiences are
completely different:

1.  **The OpenAI-compatible surface** at `/v1/chat/completions`,
    `/v1/embeddings`, `/v1/images`, `/v1/moderations`, and
    `/v1/responses`. Customers reach this through stock OpenAI SDKs
    (`openai`, `openai-node`, etc.). The body is the OpenAI error shape:

    ```json
    {
      "error": {
        "message": "...",       
        "type": "..."            // e.g. "invalid_request_error",
                                 //  "model_not_found", "upstream_error",
                                 //  "rate_limit_exceeded",
                                 //  "authentication_error"
      }
    }
    ```

    The shape MUST stay identical so existing SDKs continue to parse it.
    A customer that runs:

    ```python
    resp = client.chat.completions.create(...)
    except openai.BadRequestError as e:
        log(e.body["error"]["type"])
    ```

    is hooking on `error.type`. Renaming the field is a breaking change
    that ships a customer-incident across every account. **Do not
    rename.** The Nexus correlation id rides on the **X-Request-Id
    header** so we keep the SDK-parseable shape and still let operators
    join a customer's report to the server log line.

2.  **The Nexus-first surface** at `/api/...` (console, audit,
    bootstrap, SSO callbacks). The body is `apierr.Body`:

    ```json
    {
      "error": {
        "code": "forbidden",   // stable, branches on this
        "message": "...",       // safe, ops-friendly
        "request_id": "..."     // join to server log
      }
    }
    ```

    The `code` is the public wire contract; the message is the safe
    human-readable form; the `request_id` is the correlation id the
    server log carries. Customers on this surface typically parse
    `code` for retry / UI decisions and surface `message` to humans.

The leak guard runs the same Scrub pass on both surfaces' messages.
A SQLSTATE / stack trace / DSN never reaches either body. The shared
pass lives in `internal/apierr.Scrub`; both surfaces depend on it.

## Streaming

SSE streams (chat completions, responses) cannot immediately switch
to a JSON error body once `Content-Type: text/event-stream` is sent.
The error path emits an SSE comment line in the shape:

```
: stream error
: nexus-request-id=<id>
```

Lines starting with `:` are conventionally ignored by SSE parsers:
older SDKs treat them as comments and continue polling. A Nexus-aware
SDK can read the `: nexus-request-id=` line for correlation. The id
is the same X-Request-Id the eventual response carries on non-stream
requests.

Files:

- `internal/apierr/apierr.go` — Code, Body, Render, Scrub.
- `internal/resp/resp.go` — HTTP error renderer + slog interface.
- `internal/gateway/handler.go` — `writeError` (gateway surface, OpenAI shape).
- `internal/console/server.go` — `s.fail`, `s.failWithMessage` (console surface).

The two surfaces share the protected-signature list; future additions
must grow `apierr.protectedSignatures` AND the test mirror in
`apierr.scrub_test.go` in the same commit.
