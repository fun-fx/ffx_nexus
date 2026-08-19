package apierr_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadLatestDocPin exercises the in-memory markdown parser the
// detector uses for monotonicity. The parser picks the LAST
// committed pin from docs/pin-hit-count-history.md; failing to
// parse a known good fixture means the production detector is
// flying blind.
func TestReadLatestDocPin(t *testing.T) {
	dir := t.TempDir()
	// The parser expects docs/pin-hit-count-history.md below
	// the repo root, mirroring production layout.
	docs := filepath.Join(dir, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	md := `| Commit / Date   | Old pin | New pin | Reason                |
|-----------------|---------|---------|-----------------------|
| older migration | 80      | 60      | some reason           |
| newer migration | 60      | 40      | more migration        |
| newest migration| 40      | 12      | x                     |
`
	if err := os.WriteFile(filepath.Join(docs, "pin-hit-count-history.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := readLatestDocPin(t, dir)
	t.Logf("got=%d ok=%v", got, ok)
	if !ok {
		t.Fatal("expected parse ok=true")
	}
	if got != 12 {
		t.Errorf("got %d, want 12 (most recent New pin)", got)
	}

	// Single-row doc (old format): the parser treats the lone
	// numeric column as the only pin.
	one := `| (this baseline) | (none) | 139 | initial capture |
`
	if err := os.WriteFile(filepath.Join(docs, "pin-hit-count-history.md"), []byte(one), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok = readLatestDocPin(t, dir)
	if !ok || got != 139 {
		t.Errorf("single-row: got %d ok=%v, want 139 true", got, ok)
	}
}

// TestReadLatestDocPinMissing verifies that the detector does not
// fail when the doc hasn't been published yet; the production
// caller treats missing as ok=false and skips the cross-check.
func TestReadLatestDocPinMissing(t *testing.T) {
	dir := t.TempDir() // no file inside
	got, ok := readLatestDocPin(t, dir)
	if ok || got != 0 {
		t.Errorf("got (%d, %v) want (0, false) for missing file", got, ok)
	}
}

// TestReadLatestDocPinIgnoresHeader ensures the parser does not
// pick "old"/"new" / numeric-looking header cells as a pin.
func TestReadLatestDocPinIgnoresHeader(t *testing.T) {
	dir := t.TempDir()
	docs := filepath.Join(dir, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	md := strings.ReplaceAll(`| Commit / Date   | Old pin | New pin | Reason |
|---|---|---|---|
| migration | 80 | 60 | reason |
`, "Old pin", "Old")
	if err := os.WriteFile(filepath.Join(docs, "pin-hit-count-history.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := readLatestDocPin(t, dir)
	if !ok || got != 60 {
		t.Errorf("got %d ok=%v, want 60 true", got, ok)
	}
}
