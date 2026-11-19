//go:build integration

package app_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// scratchSchemaPrefix names every schema this test creates. Leftovers from an
// aborted run are dropped up front: a scratch schema stuck at versions 3–10
// would hold chores_household_id_assignee_id_fkey, which 000011's down step
// looks up database-wide.
const scratchSchemaPrefix = "mig_roundtrip_"

var (
	migrationFileRE          = regexp.MustCompile(`^(\d+)_(.+)\.(up|down)\.sql$`)
	migrationNameInCommentRE = regexp.MustCompile(`\b\d+_[a-zA-Z0-9_]+\.(?:up|down)\.sql\b`)
)

type migrationPair struct {
	version int
	name    string // file name without the .up.sql / .down.sql suffix
	up      string // SQL
	down    string // SQL
}

// loadMigrationPairs lists db/migrations, fails on any .up.sql without a
// .down.sql (or the reverse) or a duplicated version, and returns the pairs
// in version order.
func loadMigrationPairs(t *testing.T) []migrationPair {
	t.Helper()
	dir := migrationsDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	type halves struct{ name, up, down string }
	byVersion := map[int]*halves{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		m := migrationFileRE.FindStringSubmatch(e.Name())
		if m == nil {
			t.Errorf("migration file %s does not match NNNNNN_name.(up|down).sql", e.Name())
			continue
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("parse version of %s: %v", e.Name(), err)
		}
		base := m[1] + "_" + m[2]
		h := byVersion[v]
		if h == nil {
			h = &halves{name: base}
			byVersion[v] = h
		} else if h.name != base {
			t.Errorf("version %d is used by both %s and %s", v, h.name, base)
			continue
		}
		if m[3] == "up" {
			h.up = e.Name()
		} else {
			h.down = e.Name()
		}

		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		lines := strings.SplitN(string(content), "\n", 2)
		if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "--") {
			firstLine := lines[0]
			matches := migrationNameInCommentRE.FindAllString(firstLine, -1)
			pairName := m[1] + "_" + m[2] + ".down.sql"
			if m[3] == "down" {
				pairName = m[1] + "_" + m[2] + ".up.sql"
			}
			for _, match := range matches {
				if match != e.Name() && match != pairName {
					t.Errorf("%s: first-line comment names %s, want its own name or its pair's", e.Name(), match)
				}
			}
		}
	}
	versions := make([]int, 0, len(byVersion))
	for v := range byVersion {
		versions = append(versions, v)
	}
	slices.Sort(versions)
	pairs := make([]migrationPair, 0, len(versions))
	for _, v := range versions {
		h := byVersion[v]
		if h.up == "" {
			t.Errorf("%s.down.sql has no matching .up.sql", h.name)
			continue
		}
		if h.down == "" {
			t.Errorf("%s.up.sql has no matching .down.sql", h.name)
			continue
		}
		pairs = append(pairs, migrationPair{
			version: v,
			name:    h.name,
			up:      readMigration(t, h.up),
			down:    readMigration(t, h.down),
		})
	}
	if t.Failed() {
		t.FailNow()
	}
	if len(pairs) == 0 {
		t.Fatalf("no migrations found in %s", dir)
	}
	return pairs
}

// scratchSchema creates schema name (dropping any leftover first) and returns
// a pool whose every connection has search_path=name, so unqualified names in
// the migration files resolve only to the scratch schema. The schema is
// dropped and the pool closed when t ends; subtests run one at a time, so a
// schema is always gone before the next one is created.
func scratchSchema(t *testing.T, admin *pgxpool.Pool, name string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	quoted := `"` + name + `"`
	if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE"); err != nil {
		t.Fatalf("drop leftover schema %s: %v", name, err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatalf("create schema %s: %v", name, err)
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect scratch pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quoted+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", name, err)
		}
	})
	var current *string
	if err := pool.QueryRow(ctx, "SELECT current_schema()").Scan(&current); err != nil {
		t.Fatalf("current_schema on scratch pool: %v", err)
	}
	if current == nil || *current != name {
		t.Fatalf("scratch pool current_schema = %v, want %s", current, name)
	}
	return pool
}

func execMigration(t *testing.T, pool *pgxpool.Pool, label, sql string) {
	t.Helper()
	// One Exec per file: without arguments pgx sends it as a single simple
	// query, as golang-migrate does, so multi-statement files and DO $$ blocks
	// (000011) run whole.
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
}

func applyUps(t *testing.T, pool *pgxpool.Pool, pairs []migrationPair) {
	t.Helper()
	for _, p := range pairs {
		execMigration(t, pool, "up "+p.name, p.up)
	}
}

// snapshotQueries describe the scratch schema's structure. $1 is the schema
// name; every query is ordered so equal schemas give equal snapshots.
var snapshotQueries = []struct{ kind, sql string }{
	{"relation", `
		SELECT c.relname || ' kind=' || c.relkind::text
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r','p','v','m','S','f')
		ORDER BY 1`},
	{"column", `
		SELECT c.relname || '.' || a.attname
			|| ' type=' || format_type(a.atttypid, a.atttypmod)
			|| ' notnull=' || a.attnotnull::text
			|| ' default=' || coalesce(pg_get_expr(d.adbin, d.adrelid), '')
			|| ' identity=' || a.attidentity::text
			|| ' generated=' || a.attgenerated::text
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE n.nspname = $1 AND c.relkind IN ('r','p','v','m','f')
			AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY 1`},
	// Validity is its own field (validated=true|false), so the definition text
	// is compared without pg_get_constraintdef's trailing " NOT VALID" and a
	// validity change shows up as exactly one field; see validityDriftAllowed.
	{"constraint", `
		SELECT coalesce(c.relname, '-') || ' ' || con.conname
			|| ' type=' || con.contype::text
			|| ' ' || regexp_replace(pg_get_constraintdef(con.oid), ' NOT VALID$', '')
			|| ' validated=' || con.convalidated::text
		FROM pg_constraint con
		JOIN pg_namespace n ON n.oid = con.connamespace
		LEFT JOIN pg_class c ON c.oid = con.conrelid
		WHERE n.nspname = $1
		ORDER BY 1`},
	{"index", `
		SELECT tablename || ' ' || indexname || ' ' || indexdef
		FROM pg_indexes WHERE schemaname = $1
		ORDER BY 1`},
	{"function", `
		SELECT p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ') '
			|| CASE WHEN p.prokind IN ('f','p') THEN pg_get_functiondef(p.oid) ELSE p.prokind::text END
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1
		ORDER BY 1`},
	{"trigger", `
		SELECT c.relname || ' ' || tg.tgname || ' ' || pg_get_triggerdef(tg.oid)
		FROM pg_trigger tg
		JOIN pg_class c ON c.oid = tg.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND NOT tg.tgisinternal
		ORDER BY 1`},
	{"type", `
		SELECT t.typname || ' type=' || t.typtype::text || ' '
			|| coalesce((SELECT string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder)
				FROM pg_enum e WHERE e.enumtypid = t.oid), '')
			|| coalesce(format_type(t.typbasetype, t.typtypmod), '')
		FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = $1 AND t.typtype IN ('e','d','r','m')
		ORDER BY 1`},
}

// snapshotSchema returns one line per relation, column, constraint, index,
// function, trigger and user-defined type in schema.
func snapshotSchema(t *testing.T, pool *pgxpool.Pool, schema string) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	for _, q := range snapshotQueries {
		rows, err := pool.Query(ctx, q.sql, schema)
		if err != nil {
			t.Fatalf("snapshot %s: %v", q.kind, err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatalf("snapshot %s scan: %v", q.kind, err)
			}
			out = append(out, q.kind+": "+line)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("snapshot %s rows: %v", q.kind, err)
		}
	}
	return out
}

// validityDriftAllowed lists the only (version, constraint) pairs whose down
// step may restore a constraint NOT VALID where it was valid before up.
// Down 11 re-adds chores_household_id_assignee_id_fkey NOT VALID on purpose:
// rows of members retired by up 11 may no longer satisfy it, so validating
// would fail the rollback. Every other validity change still fails.
var validityDriftAllowed = []struct {
	version        int
	table, conname string
}{
	{11, "chores", "chores_household_id_assignee_id_fkey"},
}

// tolerateValidityDrift returns got with each allowlisted constraint line for
// version that is validated=false in got and validated=true in want, and
// otherwise identical, replaced by want's line. Nothing else is touched.
func tolerateValidityDrift(version int, want, got []string) []string {
	out := slices.Clone(got)
	for _, a := range validityDriftAllowed {
		if a.version != version {
			continue
		}
		prefix := "constraint: " + a.table + " " + a.conname + " "
		for _, w := range want {
			if !strings.HasPrefix(w, prefix) || !strings.HasSuffix(w, " validated=true") {
				continue
			}
			notValid := strings.TrimSuffix(w, "true") + "false"
			if i := slices.Index(out, notValid); i >= 0 {
				out[i] = w
			}
		}
	}
	return out
}

// diffSnapshots lists the lines only in want ("-") and only in got ("+").
func diffSnapshots(want, got []string) string {
	count := map[string]int{}
	for _, l := range want {
		count[l]++
	}
	for _, l := range got {
		count[l]--
	}
	var b strings.Builder
	for _, l := range want {
		if count[l] > 0 {
			fmt.Fprintf(&b, "\n  - %s", l)
			count[l]--
		}
	}
	for _, l := range got {
		if count[l] < 0 {
			fmt.Fprintf(&b, "\n  + %s", l)
			count[l]++
		}
	}
	return b.String()
}

// TestIntegration_MigrationsRoundTrip proves CLM-1: every migration has a
// working down step. In a scratch schema (never the shared public schema the
// other integration tests use):
//   - every *.up.sql has a matching *.down.sql;
//   - all ups, then all downs in reverse, leave the schema empty, and all ups
//     then succeed again;
//   - for each N, the schema after up 1..N then down N equals the schema at
//     N-1, and up N then reapplies.
//
// The shared schema's tables are asserted still present at the end.
func TestIntegration_MigrationsRoundTrip(t *testing.T) {
	admin := testDB(t) // schema create/drop and checks only; never runs migration files
	ctx := context.Background()
	pairs := loadMigrationPairs(t)

	// The validity tolerance is one (version, constraint) pair; anything else
	// that down leaves NOT VALID, or any other change, must still differ.
	t.Run("validity_allowlist_is_narrow", func(t *testing.T) {
		const fk = "constraint: chores chores_household_id_assignee_id_fkey type=f " +
			"FOREIGN KEY (household_id, assignee_id) REFERENCES household_members(household_id, user_id) ON DELETE SET NULL (assignee_id)"
		const other = "constraint: chores chores_pkey type=p PRIMARY KEY (id)"
		cases := []struct {
			name      string
			version   int
			want, got []string
			equal     bool
		}{
			{"allowed pair", 11, []string{fk + " validated=true"}, []string{fk + " validated=false"}, true},
			{"same constraint other version", 12, []string{fk + " validated=true"}, []string{fk + " validated=false"}, false},
			{"other constraint version 11", 11, []string{other + " validated=true"}, []string{other + " validated=false"}, false},
			{"allowed pair but definition changed", 11, []string{fk + " validated=true"},
				[]string{strings.Replace(fk, "SET NULL (assignee_id)", "CASCADE", 1) + " validated=false"}, false},
			{"allowed pair dropped", 11, []string{fk + " validated=true"}, nil, false},
		}
		for _, c := range cases {
			got := tolerateValidityDrift(c.version, c.want, c.got)
			if eq := slices.Equal(c.want, got); eq != c.equal {
				t.Errorf("%s: equal after tolerance = %v, want %v:%s", c.name, eq, c.equal, diffSnapshots(c.want, got))
			}
		}
	})

	// Drop leftovers from an aborted run before any scratch schema is used.
	rows, err := admin.Query(ctx,
		"SELECT nspname FROM pg_namespace WHERE starts_with(nspname, $1)", scratchSchemaPrefix)
	if err != nil {
		t.Fatalf("list leftover scratch schemas: %v", err)
	}
	var leftovers []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			t.Fatalf("scan leftover schema: %v", err)
		}
		leftovers = append(leftovers, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("list leftover scratch schemas: %v", err)
	}
	for _, n := range leftovers {
		if _, err := admin.Exec(ctx, `DROP SCHEMA "`+n+`" CASCADE`); err != nil {
			t.Fatalf("drop leftover schema %s: %v", n, err)
		}
	}

	// Subtests below never call t.Parallel: each scratch schema is dropped in
	// its subtest's cleanup before the next subtest creates its own.
	t.Run("full_round_trip", func(t *testing.T) {
		schema := scratchSchemaPrefix + "full"
		pool := scratchSchema(t, admin, schema)

		applyUps(t, pool, pairs)
		atLatest := snapshotSchema(t, pool, schema)
		if len(atLatest) == 0 {
			t.Fatalf("schema %s is empty after all ups", schema)
		}

		for i := len(pairs) - 1; i >= 0; i-- {
			execMigration(t, pool, "down "+pairs[i].name, pairs[i].down)
		}
		if left := snapshotSchema(t, pool, schema); len(left) != 0 {
			t.Fatalf("schema %s not empty after all downs:\n  %s", schema, strings.Join(left, "\n  "))
		}

		applyUps(t, pool, pairs)
		if got := snapshotSchema(t, pool, schema); !slices.Equal(atLatest, got) {
			t.Fatalf("schema after re-applying all ups differs from the first time:%s", diffSnapshots(atLatest, got))
		}
	})

	for i, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			schema := fmt.Sprintf("%sv%06d", scratchSchemaPrefix, p.version)
			pool := scratchSchema(t, admin, schema)

			applyUps(t, pool, pairs[:i])
			before := snapshotSchema(t, pool, schema)

			execMigration(t, pool, "up "+p.name, p.up)
			after := snapshotSchema(t, pool, schema)

			execMigration(t, pool, "down "+p.name, p.down)
			got := tolerateValidityDrift(p.version, before, snapshotSchema(t, pool, schema))
			if !slices.Equal(before, got) {
				t.Fatalf("down %s did not restore the schema it had before up (- missing, + extra):%s",
					p.name, diffSnapshots(before, got))
			}

			execMigration(t, pool, "re-up "+p.name, p.up)
			if got := snapshotSchema(t, pool, schema); !slices.Equal(after, got) {
				t.Fatalf("re-applying up %s gave a different schema than the first time:%s",
					p.name, diffSnapshots(after, got))
			}
		})
	}

	// The shared schema the other integration tests use is untouched.
	for _, table := range []string{"users", "households", "household_members", "chores"} {
		var reg *string
		if err := admin.QueryRow(ctx, "SELECT to_regclass('public.' || $1)::text", table).Scan(&reg); err != nil {
			t.Fatalf("to_regclass public.%s: %v", table, err)
		}
		if reg == nil {
			t.Errorf("shared table public.%s is gone after the round trip", table)
		}
	}
	var leftover int
	if err := admin.QueryRow(ctx,
		"SELECT count(*) FROM pg_namespace WHERE starts_with(nspname, $1)", scratchSchemaPrefix).Scan(&leftover); err != nil {
		t.Fatalf("count scratch schemas: %v", err)
	}
	if leftover != 0 {
		t.Errorf("%d scratch schema(s) left behind", leftover)
	}
}
