-- wrk_ceiling.lua — V5 single-pod ceiling probe.
-- Posts an OpenAI-compatible chat completion body so the mock-upstream
-- path is exercised end-to-end (nexus :8080 -> mock-upstream :9102).
-- Authorization header value is supplied at wrk invocation time via
-- the VKEY env variable, so this script doesn't need to know anything
-- about the auth flow.

wrk.method = "POST"
wrk.headers["Content-Type"] = "application/json"
wrk.headers["Authorization"] = "Bearer " .. (os.getenv("VKEY") or "")

local payload = '{"model":"smoke","stream":false,"messages":[{"role":"user","content":"ceiling probe"}]}'

function request()
  wrk.headers["Content-Length"] = #payload
  return wrk.format("POST", wrk.url, wrk.headers, payload)
end
