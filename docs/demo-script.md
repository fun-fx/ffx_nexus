# Demo Script — A Fresh User's First 90 Seconds With Nexus

A recorded walkthrough of the *very first* user journey — signup in an empty
Nexus instance, mint a virtual key, hit the gateway, see the trace land in
the dashboard. The script assumes Nexus is running locally on a freshly
reset dev stack (see `scripts/demo_reset.sh`).

> **한국어 버전**: [`docs/demo-script.ko.md`](demo-script.ko.md)

The script deliberately avoids pasting real provider keys on screen; we use
the `AQ.Ab8R...xxxx` style placeholder. Use a *throwaway* Gemini / OpenAI
key for the live demo, or a recorded-key-with-redaction overlay.

---

## 0. Before you press record

1. Close every other tab and notification (System Settings → notifications
   → do-not-disturb for an hour is perfect).
2. Set your dock to auto-hide so it doesn't pop up in the recording.
3. Open a fresh Chrome **Incognito** window so cookies and cached
   credentials don't bleed into the demo.
4. Resize the window to a clean 1440 × 900.
5. Start screen capture: **Cmd+Shift+5 → Record Selected Portion**
   (recommended) so a stray Slack notification can't get in.

If you want a uniform look, set Chrome zoom to 100 % and pick a neutral
light theme (Settings → Appearance → Light).

---

## 1. Intro (≈ 0:00–0:20)

> **Voiceover:**
> "Hi, I'm going to show you what it's like to use Nexus for the first
> time. I'll sign up, paste a provider key, mint a virtual key, send one
> chat completion, then watch the dashboard update in real time. Total
> elapsed: about ninety seconds."

Cursor: idle on the empty `localhost:5173` page.

---

## 2. Open the dashboard (≈ 0:20–0:30)

Action:

1. Open Chrome.
2. Type `localhost:5173` into the address bar.
3. Hit <kbd>Return</kbd>.

> **Voiceover:**
> "This is the Nexus dashboard. Everything is empty — no users, no traces,
> because we just reset the dev environment. The first thing we'll do is
> create an account."

Wait until the page fully renders (look for the **Overview / Sign in**
tabs at the top and the **LIVE** indicator in the top right — it should
read **OFFLINE** at this point because we are not logged in).

---

## 3. Sign in → Create account (≈ 0:30–1:10)

Action:

1. Click **Sign in** (top right).
2. In the panel that opens, click **Create account** (right tab).
3. Fill in the form:
   * email: `demo@nexus.local`
   * password: `hunter2hunter` (typed visibly so the audience can follow)
   * provider dropdown: **gemini**
   * label: (leave blank)
   * your LLM API key: paste (e.g.) `AQ.Ab8R…vA4` — the first 8 chars
     then ellipsis, the rest masked. **Never** paste a real key.
4. Press **Create account**.

> **Voiceover:**
> "BYOK means each user brings their own provider key — your LLM bill
> stays with your provider. Nexus encrypts the key at rest and never logs
> it in plaintext, so we never see your key either."

---

## 4. Copy the virtual key (≈ 1:10–1:30)

Action:

1. After the form submits, a panel titled **Account created** appears,
   showing a virtual key in a `<code>` block — a long string starting
   with `nxs_live_`.
2. Triple-click the key to select it, press <kbd>Cmd+C</kbd>, then click
   **Continue to dashboard**.

> **Voiceover:**
> "Nexus just minted a virtual key for this account. The virtual key is
> the only credential your apps ever see — they use it as the
> `Authorization` Bearer header against the gateway. It's shown once;
> if you lose it you mint a new one. Notice we never have to leave the
> browser."

---

## 5. Make the first chat completion (≈ 1:30–2:10)

Action:

1. Switch to the terminal window (have it pre-opened in your workspace).
2. Run the curl below — paste the virtual key from earlier into the
   `Authorization` header. The audience will see the full request and
   response.

```bash
curl http://localhost:8090/v1/chat/completions \
  -H "Authorization: Bearer nxs_live_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-2.5-flash",
    "messages": [{"role": "user", "content": "Say hi in five words"}]
  }'
```

> **Voiceover:**
> "We send one chat completion. It's the standard OpenAI protocol — only
> the base URL changed. From our app's perspective nothing else moves."

3. The response JSON contains `"content": "Hi. Hello there, friend."`
   (or similar — whatever your model returns).

---

## 6. Back to the dashboard — the trace appears (≈ 2:10–2:45)

Action:

1. Switch back to Chrome.
2. Within five seconds, scroll down to the **Recent traces** table.

> **Voiceover:**
> "The dashboard polls ClickHouse every five seconds. Within a moment
> the trace I just generated appears: the model, the latency, the
> tokens, the cost, and any flags — here 'cache' would appear if it
> were a cache hit, 'blocked' if a guardrail fired, 'byok' if the key
> came from the encrypted store."

Point out with the cursor:

* status cell (200, green)
* the provider tag (`gemini`)
* tokens (in/out)
* latency column
* the LIVE indicator at the top right (now green)

---

## 7. Trigger the cache (≈ 2:45–3:15)

Action:

1. In the terminal, press <kbd>↑</kbd> to repeat the *exact same* curl.
2. Wait for the response (much faster this time — sub-second).
3. Switch back to Chrome.

> **Voiceover:**
> "Now I rerun the *exact same* request. It comes back in just a few
> milliseconds, with a 'cache' badge in the flags column. This is the
> semantic cache — repeat traffic is free, and the latency column
> on the dashboard drops to reflect that."

Optional: pause on the new row, point to the `cache` badge and the
much smaller latency number.

---

## 8. Trigger a guardrail (≈ 3:15–4:05)

Action:

1. Replace the prompt with one that contains an email address of a
   contact that does not belong to the user. e.g.

   ```bash
   curl http://localhost:8090/v1/chat/completions \
     -H "Authorization: Bearer nxs_live_..." \
     -H "Content-Type: application/json" \
     -d '{
       "model": "gemini-2.5-flash",
       "messages": [{"role": "user", "content": "Email my old colleague at jane.doe@example.com"}]
     }'
   ```

2. Watch the terminal return a 403 with `input_blocked:pii_input`.

> **Voiceover:**
> "If someone tries to use Nexus to extract personal data, the
> guardrail layer blocks the request before any tokens are billed. You
> can see this in the trace row: status 4xx, the 'blocked' flag, and the
> Guardrail events counter on the dashboard has now ticked up by one."

Switch back to Chrome, point out:

* the `blocked` badge on the trace row
* the **Guardrail events** card count has incremented

---

## 9. Closing (≈ 4:05–4:30)

Cut back to the Overview page, scroll to the top and on the cards:

> **Voiceover:**
> "That's Nexus — a self-hosted AI gateway you can install with one
> command, deploy in five minutes, and watch work in real time. The
> source is Apache 2.0, the dashboard is MIT, and there's nothing else
> to plug in. Thanks for watching."

End the recording.

---

## Cleanup after the demo

```bash
bash scripts/demo_reset.sh        # reset Postgres + ClickHouse
kill "$(cat $HOME/.nexus/nexus.pid)"
pkill -f vite                    # stop the dashboard
```

The next demo starts from the same empty instance.

---

## Optional cuts (if the demo runs long)

* **Skip the curl entirely.** Use the OpenAI Python SDK with
  `base_url="http://localhost:8090/v1"` — same effect.
* **Skip the guardrail section.** Saves ≈ 50 s; the cache section alone
  is the strongest single visual.
* **Switch to a bigger screen.** `localhost:5173` is responsive up to
  4 K — at 1440 × 900 the side-by-side cards stay readable.
