package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/evalplugin"
)

func resolverWith(env map[string]string) *envSecretResolver {
	return &envSecretResolver{lookup: func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}}
}

func TestEnvResolverPrefersQualifiedName(t *testing.T) {
	r := resolverWith(map[string]string{
		"NEXUS_PLUGIN_SECRET_LANGFUSE_CREDS_PUBLIC_KEY": "pk-qualified",
		"NEXUS_PLUGIN_SECRET_LANGFUSE_CREDS_SECRET_KEY": "sk-qualified",
		"PUBLIC_KEY": "pk-bare",
	})
	creds, err := r.Resolve(context.Background(), evalplugin.AuthSpec{
		SecretRef: "langfuse-creds",
		KeyRef:    "public_key|secret_key",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	pub, secret, ok := creds.Pair()
	if !ok {
		t.Fatal("expected a credential pair")
	}
	if pub != "pk-qualified" || secret != "sk-qualified" {
		t.Errorf("got (%q, %q), want the qualified values", pub, secret)
	}
}

// The bare key name is what a plain `envFrom: secretRef:` projection
// produces, so it has to keep working for the zero-config path.
func TestEnvResolverFallsBackToBareKeyName(t *testing.T) {
	r := resolverWith(map[string]string{
		"PUBLIC_KEY": "pk-bare",
		"SECRET_KEY": "sk-bare",
	})
	creds, err := r.Resolve(context.Background(), evalplugin.AuthSpec{
		SecretRef: "langfuse-creds",
		KeyRef:    "public_key|secret_key",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	pub, secret, _ := creds.Pair()
	if pub != "pk-bare" || secret != "sk-bare" {
		t.Errorf("got (%q, %q), want the bare values", pub, secret)
	}
}

// keyRef order is the Basic auth order, so it must survive resolution.
func TestEnvResolverPreservesKeyRefOrder(t *testing.T) {
	r := resolverWith(map[string]string{
		"NEXUS_PLUGIN_SECRET_C_SECRET_KEY": "sk",
		"NEXUS_PLUGIN_SECRET_C_PUBLIC_KEY": "pk",
	})
	creds, err := r.Resolve(context.Background(), evalplugin.AuthSpec{
		SecretRef: "c", KeyRef: "secret_key|public_key",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if creds.Values[0] != "sk" || creds.Values[1] != "pk" {
		t.Errorf("order not preserved: %v", creds.Values)
	}
}

// The error has to name the variable to set: "secret not found" without
// the expected name leaves an operator with nothing to act on.
func TestEnvResolverNamesTheMissingVariable(t *testing.T) {
	r := resolverWith(map[string]string{})
	_, err := r.Resolve(context.Background(), evalplugin.AuthSpec{
		SecretRef: "langfuse-creds", KeyRef: "public_key|secret_key",
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	want := "NEXUS_PLUGIN_SECRET_LANGFUSE_CREDS_PUBLIC_KEY"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name %s", err, want)
	}
}

func TestEnvResolverRejectsEmptyKeyRef(t *testing.T) {
	r := resolverWith(map[string]string{"PUBLIC_KEY": "pk"})
	_, err := r.Resolve(context.Background(), evalplugin.AuthSpec{SecretRef: "langfuse-creds"})
	if err == nil {
		t.Fatal("expected an error when keyRef is empty")
	}
	if !strings.Contains(err.Error(), "keyRef") {
		t.Errorf("error should point at keyRef, got %q", err)
	}
}

// A partially-populated Secret must not yield half-formed Basic auth.
func TestEnvResolverFailsOnPartialPair(t *testing.T) {
	r := resolverWith(map[string]string{"PUBLIC_KEY": "pk"})
	_, err := r.Resolve(context.Background(), evalplugin.AuthSpec{
		SecretRef: "langfuse-creds", KeyRef: "public_key|secret_key",
	})
	if err == nil {
		t.Fatal("expected an error when only one key resolves")
	}
}

func TestEnvTokenNormalisation(t *testing.T) {
	for in, want := range map[string]string{
		"langfuse-creds": "LANGFUSE_CREDS",
		"public.key":     "PUBLIC_KEY",
		" mixed-Case ":   "MIXED_CASE",
	} {
		if got := envToken(in); got != want {
			t.Errorf("envToken(%q) = %q, want %q", in, got, want)
		}
	}
}
