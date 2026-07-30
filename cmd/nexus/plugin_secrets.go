package main

// DEPRECATED: plugin credentials no longer flow through this path.
//
// The chart-rendered langfuse-creds Secret and the legacy
// `.Values.envFrom`-projected Kubernetes Secrets that this resolver
// read from are gone (PR cleanup in ffx_nexus_ops repository, chart
// version 0.7.0). Plugin credentials now live exclusively in the
// in-product console-key UX (see plugin_keys.go and
// internal/console/eval_plugin_keys.go).
//
// `envSecretResolver` is kept as a compile-time artefact for one
// release cycle so historical traces in bundle / cache that mention
// `NEXUS_PLUGIN_SECRET_<SECRETREF>_<KEY>` continue to make sense
// in logs. It is no longer wired in `cmd/nexus/main.go`. A future
// release will delete this file outright.

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evaluators/external"
)

// envSecretResolver resolves a plugin manifest's auth block from the
// process environment.
//
// Nexus deliberately carries no Kubernetes client, so it cannot read a
// Secret through the API server. The chart already projects Secrets
// into the pod with `envFrom`, and that is the supported path here: an
// operator adds their vendor Secret to `.Values.envFrom` and its keys
// arrive as environment variables.
//
// For `secretRef: langfuse-creds` with `keyRef: public_key|secret_key`
// each key is looked up in this order:
//
//  1. NEXUS_PLUGIN_SECRET_LANGFUSE_CREDS_PUBLIC_KEY — fully qualified,
//     and the form to prefer when more than one plugin is installed
//     because it cannot collide.
//  2. PUBLIC_KEY — the bare key name, which is what a plain
//     `envFrom: secretRef: langfuse-creds` produces.
//
// Names are upper-cased with every non-alphanumeric character folded to
// an underscore, matching how Kubernetes and shells spell env vars.
type envSecretResolver struct {
	// lookup is os.LookupEnv in production; tests substitute a map.
	lookup func(string) (string, bool)
}

func newEnvSecretResolver() *envSecretResolver {
	return &envSecretResolver{lookup: os.LookupEnv}
}

// Resolve implements external.SecretResolver.
func (r *envSecretResolver) Resolve(_ context.Context, auth evalplugin.AuthSpec) (external.Credentials, error) {
	keys := splitKeyRef(auth.KeyRef)
	if len(keys) == 0 {
		// A manifest may name only a Secret, in which case the Secret's
		// own key names are unknown to us and the operator must spell
		// them out in keyRef. Treating this as an error beats sending an
		// unauthenticated request.
		return external.Credentials{}, fmt.Errorf(
			"auth.keyRef is empty: name the keys inside secret %q, e.g. keyRef: public_key|secret_key",
			auth.SecretRef)
	}
	out := make([]string, 0, len(keys))
	var missing []string
	for _, key := range keys {
		val, name := r.lookupKey(auth.SecretRef, key)
		if val == "" {
			missing = append(missing, name)
			continue
		}
		out = append(out, val)
	}
	if len(missing) > 0 {
		return external.Credentials{}, fmt.Errorf(
			"secret %q key(s) not found in environment: set %s (e.g. add the Secret to .Values.envFrom)",
			auth.SecretRef, strings.Join(missing, ", "))
	}
	return external.Credentials{Values: out}, nil
}

// lookupKey returns the resolved value plus the fully-qualified env var
// name, which is the name reported when nothing resolved so the
// operator is told exactly what to set.
func (r *envSecretResolver) lookupKey(secretRef, key string) (string, string) {
	qualified := "NEXUS_PLUGIN_SECRET_" + envToken(secretRef) + "_" + envToken(key)
	if v, ok := r.lookup(qualified); ok && v != "" {
		return v, qualified
	}
	if bare := envToken(key); bare != "" {
		if v, ok := r.lookup(bare); ok && v != "" {
			return v, qualified
		}
	}
	return "", qualified
}

// splitKeyRef parses the pipe-separated keyRef into ordered key names.
// The order matters: Langfuse's Basic auth is (public key, secret key),
// so Credentials keeps the manifest's ordering.
func splitKeyRef(keyRef string) []string {
	out := make([]string, 0, 2)
	for _, part := range strings.Split(keyRef, "|") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// envToken normalises a Secret or key name into the env var spelling:
// upper case, non-alphanumerics folded to underscore.
func envToken(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToUpper(strings.TrimSpace(s)) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
