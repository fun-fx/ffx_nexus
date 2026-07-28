// Plugin adapter registration lives in cmd/nexus/plugin_adapters.go.
// This file hosts the signature for each TransmitFunc + CollectFunc
// pair so docs/eval-plugins.md's reference table maps one-to-one to
// the Go code.
//
// The actual HTTP bodies live in langsmith.go, langfuse.go,
// datadog.go, otel.go, webhook.go, braintrust.go, arize.go.
// Testmode / empty collectors fall through to no-op senders so
// the binary boots even when no plugin is configured.

package main

import (
	"encoding/json"
	"errors"
)

// adapterError wraps a vendor-specific error so the worker logs
// include both the vendor name and a stable code for dashboards.
type adapterError struct {
	vendor string
	code   string
	err    error
}

func (a *adapterError) Error() string {
	if a.err == nil {
		return a.vendor + ": " + a.code
	}
	return a.vendor + ": " + a.code + ": " + a.err.Error()
}
func (a *adapterError) Unwrap() error { return a.err }

// jsonBody marshals the rendered payload as a JSON envelope. All
// adapters consume {map[string]any} from the dispatcher and emit
// the same shape on the wire.
func jsonBody(payload map[string]any) ([]byte, string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	return b, "application/json", nil
}

// vendorUnknown is the canonical error adapter implementations
// return when a request shape can't be reconciled with the plugin
// spec. Callers translate it into a 400/502 response when bubbling
// up to the admin REST surface.
var vendorUnknown = errors.New("vendor adapter unknown")
