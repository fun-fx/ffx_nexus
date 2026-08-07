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
// An operator answering a mid-flight question is the one case where a new
// user message does not mean a new turn; turnRootUserIndex handles it.
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
	rootUser := turnRootUserIndex(msgs)
	if rootUser < 0 {
		return ""
	}
	lastUser = msgs[rootUser].Content
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

// turnRootUserIndex returns the index of the user message that opened the
// current turn, or -1 when the history has no user message at all.
//
// Usually that is simply the last user message. The exception is an
// interruption: the operator answers a mid-flight question, or types a
// correction while the agent is still working. That arrives as a fresh
// user message, and keying on it would split one piece of work into two
// console rows right at the moment the operator is watching it.
//
// The two cases are distinguishable from the payload alone, with no clock
// and no server-side state, by looking at what sits immediately before the
// user message:
//
//	[... assistant:"done",            user:Q2]  -> agent had finished, new turn
//	[... assistant:tool_calls, tool:…, user:Q2]  -> agent was mid-loop, same turn
//
// An assistant message carrying tool calls, or a tool result, means the
// agent had not yet delivered an answer, so the new message continues the
// turn in progress. A plain assistant reply means it had, so the next
// question stands on its own.
//
// This is deliberately narrower than grouping a whole chat thread. Cursor
// keeps one long history per chat, so keying on the first user message
// would fold an afternoon of unrelated questions — a code-prime refactor
// and a text-prime draft — into a single row, which is the exact behaviour
// the derived key replaced.
//
// The walk repeats so a turn survives several interruptions in a row.
func turnRootUserIndex(msgs []Message) int {
	i := lastUserIndexBefore(msgs, len(msgs))
	for i > 0 {
		if !agentWasMidWork(msgs[i-1]) {
			break
		}
		prev := lastUserIndexBefore(msgs, i)
		if prev < 0 {
			break
		}
		i = prev
	}
	return i
}

// lastUserIndexBefore returns the index of the newest user message strictly
// before end, or -1 if there is none.
func lastUserIndexBefore(msgs []Message, end int) int {
	for i := end - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return i
		}
	}
	return -1
}

// agentWasMidWork reports whether m shows the agent still had work in
// flight rather than having delivered its answer.
func agentWasMidWork(m Message) bool {
	if m.Role == "tool" {
		return true
	}
	return m.Role == "assistant" && len(m.ToolCalls) > 0
}
