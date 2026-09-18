// Package contracts holds tests that lock Phase 0 golden fixtures.
//
// These tests do not exercise the gateway request path. They fail if a
// checked-in SSE body, error JSON shape, or migration checksum drifts
// without an explicit manifest/ledger update — so Elixir can treat
// priv/fixtures/contracts as the spec.
package contracts
