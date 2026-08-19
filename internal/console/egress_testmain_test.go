package console

import (
	"os"
	"testing"

	"github.com/ffxnexus/nexus/internal/egress"
)

// Tests in this package point vendor adapters, judges and probes at httptest
// servers, which listen on 127.0.0.1. Tenant-class egress refuses loopback in
// production — that is the control working — so the policy is relaxed here for
// destination-agnostic tests about payload encoding, headers and error handling.
//
// A test in this package that asserts something about WHICH destinations are
// permitted must call egress.TestingStrict(t) first, or it will be verifying the
// relaxed policy and would pass with the guard deleted. The policy itself is
// tested in internal/egress, which never relaxes.
func TestMain(m *testing.M) {
	egress.AllowLoopbackForPackageTests()
	os.Exit(m.Run())
}
