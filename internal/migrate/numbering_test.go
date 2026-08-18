package migrate_test

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	nexus "github.com/ffxnexus/nexus"
	"github.com/ffxnexus/nexus/internal/migrate"
)

// Properties of the migration SET, checked without a database.
//
// The ledger records applied migrations by id and verifies a checksum, so the
// numbering is not cosmetic — it determines apply order, and two files claiming
// the same number means one of them is silently skipped on a fresh install while
// both appear present in the directory listing. That is the same shape as the
// three column defects: the artefact looks complete and the behaviour is not.
//
// These run in the unit job. No database is needed to know that two files are
// numbered 010.

var migrationName = regexp.MustCompile(`^(\d{3})_([a-z0-9_]+)\.sql$`)

func engines() map[migrate.Engine]string {
	return map[migrate.Engine]string{
		migrate.EnginePostgres:   "migrations/postgres",
		migrate.EngineClickHouse: "migrations/clickhouse",
	}
}

func loadOrFail(t *testing.T, engine migrate.Engine) []migrate.Migration {
	t.Helper()
	migs, err := migrate.Load(nexus.Migrations, engine)
	if err != nil {
		t.Fatalf("load %s migrations: %v", engine, err)
	}
	if len(migs) == 0 {
		t.Fatalf("no %s migrations loaded; the embed pattern or the engine's directory "+
			"name has changed, and every test below is now vacuous", engine)
	}
	return migs
}

// Two files with the same number is the defect this test exists for. ClickHouse is
// the engine where it nearly happened: 010_eval_scores_org.sql was added while 010
// was the next free number on the Postgres side too, and the two directories are
// numbered independently.
func TestNoTwoMigrationsShareANumberWithinAnEngine(t *testing.T) {
	for engine, dir := range engines() {
		byNumber := map[int][]string{}
		for _, m := range loadOrFail(t, engine) {
			num, name := parseID(t, m.ID)
			byNumber[num] = append(byNumber[num], name)
		}
		for num, names := range byNumber {
			if len(names) > 1 {
				sort.Strings(names)
				t.Errorf("%s: number %03d is used by %d files: %s\n"+
					"The ledger keys on the id, so one of these is applied and the other "+
					"is recorded as already-applied without ever running. Renumber the "+
					"newer file to the next free number.", dir, num, len(names),
					strings.Join(names, ", "))
			}
		}
	}
}

// Gaps are not fatal, but they usually mean a file was deleted after being applied
// somewhere, which makes an existing installation's ledger disagree with the set.
func TestMigrationNumbersAreContiguousFromOne(t *testing.T) {
	for engine, dir := range engines() {
		var nums []int
		for _, m := range loadOrFail(t, engine) {
			num, _ := parseID(t, m.ID)
			nums = append(nums, num)
		}
		sort.Ints(nums)

		if nums[0] != 1 {
			t.Errorf("%s: numbering starts at %03d, not 001", dir, nums[0])
		}
		for i := 1; i < len(nums); i++ {
			if nums[i] != nums[i-1]+1 {
				t.Errorf("%s: gap between %03d and %03d. A deleted migration leaves "+
					"already-upgraded installations with a ledger row for a file that no "+
					"longer exists, and a future file reusing the number will be treated "+
					"as already applied. Leave a tombstone file instead of deleting.",
					dir, nums[i-1], nums[i])
			}
		}
	}
}

// Load must return migrations in apply order. A directory listing is
// lexicographic, which is only the same as numeric while the numbers are
// zero-padded to a fixed width — so the padding is load-bearing, not style.
func TestLoadReturnsMigrationsInAscendingNumericOrder(t *testing.T) {
	for engine, dir := range engines() {
		migs := loadOrFail(t, engine)
		prev := 0
		for _, m := range migs {
			num, _ := parseID(t, m.ID)
			if num <= prev {
				t.Errorf("%s: %s is ordered after number %03d. Migrations would apply "+
					"out of order, so a migration would run before the one it depends on.",
					dir, m.ID, prev)
			}
			prev = num
		}
	}
}

// Fixed-width zero padding is what keeps lexicographic and numeric order the same.
// Without it, 100 sorts before 99.
func TestEveryMigrationFilenameFollowsTheNumberingConvention(t *testing.T) {
	for engine, dir := range engines() {
		for _, m := range loadOrFail(t, engine) {
			name := baseName(m.ID)
			if !migrationName.MatchString(name) {
				t.Errorf("%s/%s does not match NNN_lower_snake_case.sql. Three-digit "+
					"zero-padded numbering is what makes the lexicographic directory order "+
					"equal the numeric apply order; without it 100 sorts before 099.",
					dir, name)
			}
		}
	}
}

// The checksum is what makes an edit to an already-applied migration abort the
// upgrade instead of leaving two installations with different schemas at the same
// ledger position. It must therefore be stable, content-derived, and distinct per
// file.
func TestChecksumsAreContentDerivedAndDistinct(t *testing.T) {
	for engine, dir := range engines() {
		migs := loadOrFail(t, engine)

		seen := map[string]string{}
		for _, m := range migs {
			if m.Checksum == "" {
				t.Errorf("%s/%s has an empty checksum, so an edit to it after it has "+
					"been applied would go undetected", dir, m.ID)
				continue
			}
			if len(m.Checksum) != 64 {
				t.Errorf("%s/%s checksum %q is not a hex SHA-256", dir, m.ID, m.Checksum)
			}
			if prev, dup := seen[m.Checksum]; dup {
				t.Errorf("%s: %s and %s have the same checksum. Either the files are "+
					"byte-identical (one is redundant) or the checksum is not derived from "+
					"the content, in which case drift detection does nothing.",
					dir, prev, m.ID)
			}
			seen[m.Checksum] = m.ID
		}

		// Stability across loads: a checksum that varied per process would abort
		// every upgrade with spurious drift.
		again := loadOrFail(t, engine)
		if len(again) != len(migs) {
			t.Fatalf("%s: two loads returned %d and %d migrations", dir, len(migs), len(again))
		}
		for i := range migs {
			if migs[i].Checksum != again[i].Checksum {
				t.Errorf("%s/%s checksum is not stable across loads (%s then %s). Every "+
					"upgrade would abort with spurious checksum drift.",
					dir, migs[i].ID, short(migs[i].Checksum), short(again[i].Checksum))
			}
		}
	}
}

// The two engines' directories are numbered independently, and a migration written
// for one engine must never be loaded for the other — ClickHouse DDL executed
// against Postgres fails halfway, leaving a partially migrated database.
func TestEachEngineLoadsOnlyItsOwnMigrations(t *testing.T) {
	pg := loadOrFail(t, migrate.EnginePostgres)
	ch := loadOrFail(t, migrate.EngineClickHouse)

	pgSQL := map[string]bool{}
	for _, m := range pg {
		pgSQL[m.Checksum] = true
	}
	for _, m := range ch {
		if pgSQL[m.Checksum] {
			t.Errorf("ClickHouse migration %s has the same content as a Postgres "+
				"migration, so one directory is loading the other's files", m.ID)
		}
	}

	// ClickHouse DDL that Postgres cannot execute, and vice versa, as a sanity
	// check that the two sets really are engine-specific.
	pgJoined := strings.ToUpper(joinSQL(pg))
	if strings.Contains(pgJoined, "MERGETREE") {
		t.Error("a Postgres migration contains a MergeTree engine clause, which " +
			"Postgres cannot execute; the ClickHouse directory is being loaded for " +
			"Postgres")
	}
}

func joinSQL(migs []migrate.Migration) string {
	var b strings.Builder
	for _, m := range migs {
		b.WriteString(m.SQL)
		b.WriteString("\n")
	}
	return b.String()
}

// parseID splits a migration id into its number and filename. Ids carry the
// engine directory ("postgres/001_init.sql"), so the base name is what the
// numbering convention applies to.
func parseID(t *testing.T, id string) (int, string) {
	t.Helper()
	name := baseName(id)
	m := migrationName.FindStringSubmatch(name)
	if m == nil {
		// Reported by TestEveryMigrationFilenameFollowsTheNumberingConvention; do
		// not fail twice for the same file.
		return -1, id
	}
	num, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse number from %q: %v", id, err)
	}
	return num, name
}

// baseName strips the engine directory from a migration id.
func baseName(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if !strings.HasSuffix(id, ".sql") {
		id += ".sql"
	}
	return id
}

func short(sum string) string {
	if len(sum) > 8 {
		return sum[:8]
	}
	return sum
}

// A migration that declares no SQL would be recorded as applied while doing
// nothing, which is the emptiest possible version of the defect class.
func TestNoMigrationIsEmpty(t *testing.T) {
	for engine, dir := range engines() {
		for _, m := range loadOrFail(t, engine) {
			if strings.TrimSpace(stripComments(m.SQL)) == "" {
				t.Errorf("%s/%s contains no executable SQL, so it is recorded in the "+
					"ledger as applied while changing nothing", dir, m.ID)
			}
		}
	}
}

func stripComments(sql string) string {
	var out []string
	for _, line := range strings.Split(sql, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "--") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// A concise inventory in the run log, so a reviewer can see what the set contains
// without opening two directories.
func TestMigrationSetInventory(t *testing.T) {
	for engine, dir := range engines() {
		migs := loadOrFail(t, engine)
		t.Logf("%s: %d migrations", dir, len(migs))
		for _, m := range migs {
			t.Log("  " + fmt.Sprintf("%-44s %s", m.ID, short(m.Checksum)))
		}
	}
}
