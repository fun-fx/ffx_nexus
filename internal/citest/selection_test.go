// Package citest verifies properties of the CI configuration itself.
//
// It exists because of a specific failure, three times over. Code queried a
// column the migration set never created; a test that would have caught it was
// already written and committed; and CI never ran it. The integration job used
//
//	go test ./internal/core/ -run Integration
//
// and the test was named TestBenchmarkScheduleCreateAndList. Meanwhile the
// default `go test ./...` skipped it for want of NEXUS_TEST_POSTGRES_URL. So the
// test existed, passed locally for whoever wrote it, and was invisible in CI.
//
// A -run filter is a silent exclusion: nothing reports what it left out, and the
// list of excluded tests grows every time somebody adds one. That is the root
// cause, and the fix is not "remember to check the filter" — it is to make an
// unjustified filter fail the build.
package citest

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// justifiedFilters maps a `-run` pattern appearing in .github/workflows/ci.yml to
// the reason it is acceptable.
//
// The bar is high on purpose. A filter is justified only when running the rest of
// the package in that job would be WRONG — not when it would be slow, and not
// when the other tests "should already be covered elsewhere". "Covered elsewhere"
// is exactly the belief that hid this defect: internal/core's schedule test was
// assumed to be covered by the unit run, which skipped it.
//
// Acceptable reasons look like: the job provides one datastore and the excluded
// tests need a different one; the excluded tests are live-vendor tests that would
// call a paid API.
var justifiedFilters = map[string]string{
	// No entries. Every integration job runs its whole package.
	//
	// If you are about to add one, first check whether the tests you want to
	// exclude would pass in that job. If they would, do not add the filter. If
	// they would fail because the job lacks a dependency, the better fix is
	// usually a t.Skip inside the test that names the missing dependency, because
	// a skip is reported in the run log and a -run filter is not.
}

// liveVendorPattern matches test names that call a real third-party API. These
// are excluded from every CI job by not being selected anywhere, which is
// deliberate and is asserted below rather than left implicit.
var liveVendorPattern = regexp.MustCompile(`Live$|^TestLive|_Live`)

type workflow struct {
	Jobs map[string]struct {
		Steps []struct {
			Name string `yaml:"name"`
			Run  string `yaml:"run"`
			Env  map[string]string
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func loadWorkflow(t *testing.T) workflow {
	t.Helper()
	path := filepath.Join(repoRoot(t), ".github", "workflows", "ci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatalf("%s declares no jobs; the parser is looking at the wrong shape", path)
	}
	return wf
}

var runFilterRe = regexp.MustCompile(`-run\s+'?"?([^\s'"]+)'?"?`)

// goTestCommand is one `go test` invocation found in the workflow.
type goTestCommand struct {
	job    string
	step   string
	cmd    string
	filter string
}

func goTestCommands(t *testing.T) []goTestCommand {
	t.Helper()
	var out []goTestCommand
	for jobName, job := range loadWorkflow(t).Jobs {
		for _, step := range job.Steps {
			for _, line := range strings.Split(step.Run, "\n") {
				line = strings.TrimSpace(line)
				if !strings.Contains(line, "go test") {
					continue
				}
				cmd := goTestCommand{job: jobName, step: step.Name, cmd: line}
				if m := runFilterRe.FindStringSubmatch(line); m != nil {
					cmd.filter = m[1]
				}
				out = append(out, cmd)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].cmd < out[j].cmd })
	return out
}

// The headline guard.
func TestEveryTestSelectionFilterIsJustified(t *testing.T) {
	cmds := goTestCommands(t)
	if len(cmds) == 0 {
		t.Fatal("found no `go test` commands in ci.yml; the parser has drifted from " +
			"the workflow's shape and this guard is doing nothing")
	}

	var unjustified []string
	for _, c := range cmds {
		if c.filter == "" {
			continue
		}
		if _, ok := justifiedFilters[c.filter]; !ok {
			unjustified = append(unjustified, "  job "+c.job+", step "+c.step+
				"\n    "+c.cmd+"\n    filter: -run "+c.filter)
		}
	}

	if len(unjustified) > 0 {
		t.Fatalf(`%d CI test-selection filter(s) have no recorded justification:

%s

A -run filter silently excludes tests, and nothing reports what it left out. That
is how a fresh-install defect survived three times: the test that caught it was
written and committed, but the integration job ran -run Integration and the test
was not named that way, while the default unit run skipped it for lack of a
database URL.

Prefer running the whole package. If some tests cannot pass in that job because it
lacks a dependency, put a t.Skip inside those tests naming the dependency — a skip
appears in the run log, a -run filter does not.

If the filter really is correct, add the pattern to justifiedFilters in
internal/citest/selection_test.go with the reason.`,
			len(unjustified), strings.Join(unjustified, "\n\n"))
	}
}

// A justification for a filter that is no longer used is stale documentation, and
// a stale entry is how the next filter gets waved through.
func TestNoStaleFilterJustifications(t *testing.T) {
	inUse := map[string]bool{}
	for _, c := range goTestCommands(t) {
		if c.filter != "" {
			inUse[c.filter] = true
		}
	}
	for pattern := range justifiedFilters {
		if !inUse[pattern] {
			t.Errorf("justifiedFilters records a reason for -run %q, which no CI step "+
				"uses. Remove it so the list keeps describing the workflow.", pattern)
		}
	}
}

// Cache reuse across runs would let a test that passed against an old schema keep
// reporting success. Every CI invocation must defeat the cache.
func TestEveryCITestInvocationDefeatsTheCache(t *testing.T) {
	for _, c := range goTestCommands(t) {
		if strings.Contains(c.cmd, "-list") {
			continue // listing does not execute anything
		}
		if !strings.Contains(c.cmd, "-count=1") {
			t.Errorf("job %q step %q runs go test without -count=1:\n    %s\n"+
				"Go caches a successful test result keyed on the package's inputs. A "+
				"migration file is not one of those inputs, so a schema change can "+
				"leave a passing cached result in place for a test that would now "+
				"fail.", c.job, c.step, c.cmd)
		}
	}
}

// The integration jobs must actually run the packages whose defects need a real
// datastore. Asserting this by name means deleting the step fails the build
// rather than quietly reducing coverage.
func TestIntegrationJobsCoverTheDatastoreBackedPackages(t *testing.T) {
	required := map[string]string{
		"./internal/migrate/": "migration ledger, adoption, concurrent apply",
		"./internal/core/":    "org-scoped SQL and the schema contract",
		"./cmd/nexus/":        "readiness withholding traffic on an unmigrated schema",
	}

	joined := ""
	for _, c := range goTestCommands(t) {
		joined += c.cmd + "\n"
	}
	for pkg, why := range required {
		if !strings.Contains(joined, pkg) {
			t.Errorf("no CI step runs %s, which covers %s. These tests need a real "+
				"database and are skipped by the unit job, so dropping the step removes "+
				"the coverage entirely and silently.", pkg, why)
		}
	}
}

// Live-vendor tests would call a paid third-party API. They are excluded by never
// being selected, which is correct — but it is also invisible, so state it.
func TestLiveVendorTestsAreNotSelectedByAnyJob(t *testing.T) {
	for _, c := range goTestCommands(t) {
		if c.filter == "" {
			continue
		}
		if liveVendorPattern.MatchString(c.filter) {
			t.Errorf("job %q step %q selects %q, which matches the live-vendor naming "+
				"convention. CI must not call a paid vendor API.", c.job, c.step, c.filter)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(wd)) // internal/citest -> repo root
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("cannot locate the repository root from %s", wd)
	}
	return root
}
