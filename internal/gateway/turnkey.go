package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// deriveTurnKey builds a stable identifier for one agent turn — a single
// user question plus every follow-up model call the agent makes while
// answering it.
//
// The wire gives us nothing to correlate those calls with. session_id is
// only populated when the client sets metadata.session_id (not observed
// from Cursor in prod) and parent_span_id is the per-HTTP X-Request-Id,
// which splits an agent loop into one group per call rather than joining
// them. So we derive the key from the payload instead.
//
// The observation this rests on: an agent loop appends its intermediate
// work as assistant/tool messages, never as user ones (see the Responses
// conversion in responses.go, where function_call_output becomes
// Role: "tool"). The last user-role message therefore stays byte-identical
// for every call in a turn and changes the moment the human asks something
// else. Mixing in the system prompt and the caller's user id keeps two
// different people — or the same person in two different agent modes —
// from colliding on an identical question.
//
// Returns "" when there is no user message to key on, which leaves the
// trace ungrouped rather than lumping it in with unrelated traffic.
//
// Known limit: asking the exact same question twice in a row under the
// same system prompt produces the same key, so those turns merge. The
// drill-down still lists the individual calls, and splitting on an idle
// gap at query time is the fix if it ever becomes a real annoyance.
func deriveTurnKey(userID string, msgs []Message) string {
	var lastUser, system string
	foundUser := false
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			lastUser = msgs[i].Content
			foundUser = true
			break
		}
	}
	if !foundUser {
		return ""
	}
	for _, m := range msgs {
		// "developer" is the Responses-era spelling of "system"; Cursor's
		// instructions field lands as one or the other depending on path.
		if m.Role == "system" || m.Role == "developer" {
			system = m.Content
			break
		}
	}

	h := sha256.New()
	// NUL separators so ("ab", "c") and ("a", "bc") cannot hash alike.
	h.Write([]byte(userID))
	h.Write([]byte{0})
	h.Write([]byte(system))
	h.Write([]byte{0})
	h.Write([]byte(strings.TrimSpace(lastUser)))
	return hex.EncodeToString(h.Sum(nil))[:16]
}
