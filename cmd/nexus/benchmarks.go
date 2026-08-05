package main

import (
	"context"
	"time"

	"github.com/ffxnexus/nexus/internal/benchmark"
)

// benchmarkPollInterval is how often unsettled runs are re-read from the
// provider.
//
// A hosted run takes minutes to hours, so a tight loop only burns API
// calls. One minute keeps the console close enough to live that an
// operator watching a run sees it settle without reaching for Refresh,
// which forces a pass on demand anyway.
const benchmarkPollInterval = time.Minute

// benchmarkTokens resolves the provider API token out of the encrypted
// plugin-key vault.
//
// Reusing that store rather than adding a second secret surface means
// the token gets the same master-key encryption and the same durability
// as vendor plugin keys — including surviving a deploy, which is the
// property those keys originally lacked.
type benchmarkTokens struct {
	keys *consoleKeyResolver
}

// Token implements benchmark.Tokens. A missing entry is reported as an
// empty string rather than an error: "no key pasted yet" is a normal
// state on a fresh install, and the caller turns it into a message that
// says where to paste one.
func (b benchmarkTokens) Token(_ context.Context, provider string) (string, error) {
	if b.keys == nil {
		return "", nil
	}
	// One provider today. Naming it explicitly means a second provider
	// cannot silently reuse PrimeIntellect's credential.
	if provider != benchmark.ProviderPrime {
		return "", nil
	}
	kv, ok := b.keys.Get(benchmark.CredentialName)
	if !ok {
		return "", nil
	}
	return kv[benchmark.CredentialKey], nil
}

// TeamID implements benchmark.Tokens. An empty string means bill the
// personal wallet tied to the API key.
func (b benchmarkTokens) TeamID(_ context.Context, provider string) (string, error) {
	if b.keys == nil || provider != benchmark.ProviderPrime {
		return "", nil
	}
	kv, ok := b.keys.Get(benchmark.CredentialName)
	if !ok {
		return "", nil
	}
	return kv[benchmark.CredentialTeamIDKey], nil
}
