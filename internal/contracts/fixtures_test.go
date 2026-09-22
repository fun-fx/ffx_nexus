package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func fixturesDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Join(filepath.Dir(file), "..", "..", "priv", "fixtures", "contracts")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("missing fixture tree %s: %v", dir, err)
	}
	return dir
}

func TestSSEManifestHashes(t *testing.T) {
	dir := filepath.Join(fixturesDir(t), "sse")
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var man struct {
		Files []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
			Bytes  int    `json:"bytes"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatal(err)
	}
	if len(man.Files) == 0 {
		t.Fatal("sse/manifest.json has no files")
	}
	seen := map[string]bool{}
	for _, f := range man.Files {
		body, err := os.ReadFile(filepath.Join(dir, f.Path))
		if err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		sum := sha256.Sum256(body)
		got := hex.EncodeToString(sum[:])
		if got != f.SHA256 {
			t.Errorf("%s: sha256 %s, manifest %s", f.Path, got, f.SHA256)
		}
		if len(body) != f.Bytes {
			t.Errorf("%s: %d bytes, manifest %d", f.Path, len(body), f.Bytes)
		}
		seen[f.Path] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sse" {
			continue
		}
		if !seen[e.Name()] {
			t.Errorf("%s exists on disk but is missing from sse/manifest.json", e.Name())
		}
	}
}

func TestErrorShapes(t *testing.T) {
	dir := filepath.Join(fixturesDir(t), "errors")

	v1 := readJSON(t, filepath.Join(dir, "v1_openai.json"))
	body, _ := v1["body"].(map[string]any)
	errObj, _ := body["error"].(map[string]any)
	if _, ok := errObj["message"]; !ok {
		t.Error("/v1 body.error.message missing")
	}
	if _, ok := errObj["type"]; !ok {
		t.Error("/v1 body.error.type missing")
	}
	if _, ok := errObj["code"]; ok {
		t.Error("/v1 body.error must not carry apierr code (OpenAI SDKs read error.type)")
	}
	if _, ok := errObj["request_id"]; ok {
		t.Error("/v1 body.error must not carry request_id; it is the X-Request-Id header")
	}

	api := readJSON(t, filepath.Join(dir, "api_nexus.json"))
	body, _ = api["body"].(map[string]any)
	errObj, _ = body["error"].(map[string]any)
	for _, k := range []string{"code", "message", "request_id"} {
		if _, ok := errObj[k]; !ok {
			t.Errorf("/api body.error.%s missing", k)
		}
	}

	comment, err := os.ReadFile(filepath.Join(dir, "sse_comment.sse"))
	if err != nil {
		t.Fatal(err)
	}
	want := ": stream error\n: nexus-request-id=req_golden\n"
	if string(comment) != want {
		t.Errorf("sse_comment.sse mismatch\nwant %q\ngot  %q", want, comment)
	}
}

func TestSchemaLedgerCoversMigrations(t *testing.T) {
	dir := fixturesDir(t)
	raw, err := os.ReadFile(filepath.Join(dir, "schema", "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	var led struct {
		Table      string `json:"ledger_table"`
		Migrations []struct {
			ID     string `json:"id"`
			Engine string `json:"engine"`
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
		} `json:"migrations"`
	}
	if err := json.Unmarshal(raw, &led); err != nil {
		t.Fatal(err)
	}
	if led.Table != "schema_migrations" {
		t.Fatalf("ledger_table %q", led.Table)
	}
	_, thisFile, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..")
	for _, m := range led.Migrations {
		path := filepath.Join(repo, "migrations", m.Engine, m.Name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", m.ID, err)
			continue
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != m.SHA256 {
			t.Errorf("%s: checksum drifted; re-run scripts/dump_schema.sh", m.ID)
		}
	}
	if len(led.Migrations) < 30 {
		t.Fatalf("expected postgres+clickhouse files, got %d", len(led.Migrations))
	}
}

func TestPINHasOracleFields(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(fixturesDir(t), "PIN.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pin map[string]any
	if err := json.Unmarshal(raw, &pin); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"production_go_commit", "chart_version", "sibling_elixir_repo", "fixture_tree"} {
		if pin[k] == nil || pin[k] == "" {
			t.Errorf("PIN.json missing %s", k)
		}
	}
	if pin["sibling_elixir_repo"] != "fun-fx/nexus_ex" {
		t.Errorf("sibling repo %v", pin["sibling_elixir_repo"])
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
