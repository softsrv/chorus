//go:build integration

package app_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/softsrv/chorus/internal/db"
)

// localDate is the calendar date in tz today, at midnight UTC so it compares
// and formats as a plain date.
func localDate(t *testing.T, tz string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load %s: %v", tz, err)
	}
	now := time.Now().In(loc)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// yearsBefore is the date years before d, or false when d is Feb 29 (the
// date does not exist years earlier, so the boundary case has no single day).
func yearsBefore(d time.Time, years int) (time.Time, bool) {
	if d.Month() == time.February && d.Day() == 29 {
		return time.Time{}, false
	}
	return time.Date(d.Year()-years, d.Month(), d.Day(), 0, 0, 0, 0, time.UTC), true
}

func pgDate(d time.Time) pgtype.Date { return pgtype.Date{Time: d, Valid: true} }

func optInt(v int32) *int32 { return &v }

// TestIntegration_QuestAgeRangeFunctions proves CLM-1: the age rule lives in
// quest_member_age and quest_age_eligible (migration 000018), whole years in
// the given timezone, the age advancing on the birthday itself, limits
// inclusive, and a NULL birthday or NULL limit passing. No other query or Go
// code restates the arithmetic.
func TestIntegration_QuestAgeRangeFunctions(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	const chicago = "America/Chicago"

	memberAge := func(bd pgtype.Date, tz string) *int32 {
		t.Helper()
		var age *int32
		if err := pool.QueryRow(ctx, "SELECT quest_member_age($1::date, $2)", bd, tz).Scan(&age); err != nil {
			t.Fatalf("quest_member_age(%v, %s): %v", bd, tz, err)
		}
		return age
	}
	eligible := func(bd pgtype.Date, tz string, minAge, maxAge *int32) bool {
		t.Helper()
		var ok bool
		if err := pool.QueryRow(ctx, "SELECT quest_age_eligible($1::date, $2, $3::int, $4::int)", bd, tz, minAge, maxAge).Scan(&ok); err != nil {
			t.Fatalf("quest_age_eligible: %v", err)
		}
		return ok
	}

	if got := memberAge(pgtype.Date{}, chicago); got != nil {
		t.Errorf("quest_member_age(NULL) = %d, want NULL", *got)
	}

	today := localDate(t, chicago)
	if birthday10, ok := yearsBefore(today, 10); ok {
		dayBefore := birthday10.AddDate(0, 0, 1) // turns 10 tomorrow
		if got := memberAge(pgDate(birthday10), chicago); got == nil || *got != 10 {
			t.Errorf("age on the 10th birthday = %v, want 10", got)
		}
		if got := memberAge(pgDate(dayBefore), chicago); got == nil || *got != 9 {
			t.Errorf("age the day before the 10th birthday = %v, want 9", got)
		}
		cases := []struct {
			name           string
			bd             pgtype.Date
			minAge, maxAge *int32
			want           bool
		}{
			{"day before reaching min", pgDate(dayBefore), optInt(10), nil, false},
			{"birthday reaches min", pgDate(birthday10), optInt(10), nil, true},
			{"max is inclusive", pgDate(birthday10), nil, optInt(10), true},
			{"above max", pgDate(birthday10), nil, optInt(9), false},
			{"inside range", pgDate(birthday10), optInt(8), optInt(12), true},
			{"below range", pgDate(birthday10), optInt(11), optInt(14), false},
			{"no limits", pgDate(birthday10), nil, nil, true},
			{"no birthday, range set", pgtype.Date{}, optInt(10), optInt(14), true},
			{"no birthday, no limits", pgtype.Date{}, nil, nil, true},
		}
		for _, c := range cases {
			if got := eligible(c.bd, chicago, c.minAge, c.maxAge); got != c.want {
				t.Errorf("%s: quest_age_eligible = %v, want %v", c.name, got, c.want)
			}
		}
	}

	// Across a timezone date boundary: UTC+14 and UTC-11 are always on
	// different dates, so a birthday that is today in the east has not come
	// yet in the west.
	const east, west = "Pacific/Kiritimati", "Pacific/Pago_Pago"
	if bd, ok := yearsBefore(localDate(t, east), 12); ok {
		if got := memberAge(pgDate(bd), east); got == nil || *got != 12 {
			t.Errorf("age in %s on the birthday = %v, want 12", east, got)
		}
		if got := memberAge(pgDate(bd), west); got == nil || *got != 11 {
			t.Errorf("age in %s the day before the birthday = %v, want 11", west, got)
		}
		if eligible(pgDate(bd), west, optInt(12), nil) || !eligible(pgDate(bd), east, optInt(12), nil) {
			t.Errorf("min 12 must refuse in %s and allow in %s", west, east)
		}
	}

	// Through ShareQuest, in a household in Chicago: a target below the
	// quest's minimum is refused and nothing is written; a target on the
	// birthday that reaches it, or with no birthday, is added. A member of
	// another household is never read as a target. Rolled back.
	if birthday10, ok := yearsBefore(today, 10); ok {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		hh, other := uuid.New(), uuid.New()
		for _, id := range []uuid.UUID{hh, other} {
			if _, err := tx.Exec(ctx, "INSERT INTO households (id, name, timezone) VALUES ($1, 'Ages', $2)", id, chicago); err != nil {
				t.Fatalf("insert household: %v", err)
			}
		}
		member := func(hid uuid.UUID, name string, bd pgtype.Date) uuid.UUID {
			t.Helper()
			id := uuid.New()
			if _, err := tx.Exec(ctx, "INSERT INTO family_members (id, household_id, name, points, birthday) VALUES ($1, $2, $3, 0, $4)", id, hid, name, bd); err != nil {
				t.Fatalf("insert member %s: %v", name, err)
			}
			return id
		}
		holder := member(hh, "Holder", pgtype.Date{})
		tooYoung := member(hh, "Young", pgDate(birthday10.AddDate(0, 0, 1)))
		turnsTen := member(hh, "Ten", pgDate(birthday10))
		noBirthday := member(hh, "NoBday", pgtype.Date{})
		// Same name and an in-range birthday, but another household.
		outsider := member(other, "Out", pgDate(birthday10))
		questID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO household_quests (id, household_id, name, points, repeat_days, shareable, min_age, max_age, claimed_by, claimed_at)
			VALUES ($1, $2, 'Mow the lawn', 20, 7, true, 10, 14, $3, NOW())`, questID, hh, holder); err != nil {
			t.Fatalf("insert quest: %v", err)
		}
		q := db.New(tx)
		share := func(target uuid.UUID) int {
			t.Helper()
			added, err := q.ShareQuest(ctx, db.ShareQuestParams{HouseholdID: hh, ID: questID, MemberID: holder, TargetMemberID: target})
			if err != nil {
				t.Fatalf("ShareQuest: %v", err)
			}
			return len(added)
		}
		participants := func() int {
			t.Helper()
			var n int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM household_quest_participants WHERE quest_id = $1", questID).Scan(&n); err != nil {
				t.Fatalf("count participants: %v", err)
			}
			return n
		}
		if n := share(tooYoung); n != 0 || participants() != 0 {
			t.Errorf("share with a member a day short of min_age added %d (participants %d), want nothing", n, participants())
		}
		if n := share(outsider); n != 0 || participants() != 0 {
			t.Errorf("share with a member of another household added %d, want nothing", n)
		}
		if n := share(turnsTen); n != 2 {
			t.Errorf("share with a member on the birthday reaching min_age added %d, want holder and target", n)
		}
		if n := share(noBirthday); n != 1 {
			t.Errorf("share with a member with no birthday added %d, want 1", n)
		}
	}

	// The arithmetic is defined once: no query or Go file computes an age
	// from a birthday itself.
	forbiddenSQL := regexp.MustCompile(`(?i)\b(age|date_part|extract)\s*\(`)
	forbiddenGo := regexp.MustCompile(`(?i)birthday\S*\.(Year|YearDay|AddDate|Sub)\(`)
	self, _ := filepath.Abs("quest_age_range_migration_integration_test.go")
	queriesDir := filepath.Join(filepath.Dir(migrationsDir(t)), "queries")
	internalDir := filepath.Join(filepath.Dir(filepath.Dir(migrationsDir(t))), "internal")
	scanned := 0
	for _, root := range []string{queriesDir, internalDir} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			isSQL, isGo := strings.HasSuffix(path, ".sql"), strings.HasSuffix(path, ".go")
			if !isSQL && !isGo {
				return nil
			}
			if abs, _ := filepath.Abs(path); abs == self {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			text := string(content)
			for _, loc := range forbiddenSQL.FindAllStringIndex(text, -1) {
				// In Go, only SQL counts: the backquoted strings sqlc
				// generates and tests embed.
				if isSQL || inSQLString(text, loc[0]) {
					t.Errorf("%s restates age arithmetic (%q); call quest_member_age / quest_age_eligible", path, text[loc[0]:loc[1]])
				}
			}
			if isGo {
				if loc := forbiddenGo.FindStringIndex(text); loc != nil {
					t.Errorf("%s computes an age in Go (%q); the age comes from quest_member_age", path, text[loc[0]:loc[1]])
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if scanned == 0 {
		t.Fatal("static check scanned no files")
	}
}

// inSQLString reports whether offset falls inside a Go raw string literal
// (the backquoted SQL sqlc generates and tests embed).
func inSQLString(text string, offset int) bool {
	return strings.Count(text[:offset], "`")%2 == 1
}

// TestIntegration_QuestAgeRangeMigration proves CLM-14: migration 000018 is
// expand-only. Rows seeded at 000017 come out with NULL limits and birthday,
// a pre-existing member still accepts and is shared a pre-existing quest, the
// CHECKs refuse negatives and min > max, and down drops the columns,
// constraints and functions. Everything runs in one rolled-back transaction.
func TestIntegration_QuestAgeRangeMigration(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	upSQL := readMigration(t, "000018_quest_age_range.up.sql")
	downSQL := readMigration(t, "000018_quest_age_range.down.sql")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	columnExists := func(table, column string) bool {
		t.Helper()
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2)`, table, column).Scan(&ok); err != nil {
			t.Fatalf("column %s.%s: %v", table, column, err)
		}
		return ok
	}
	functionCount := func() int {
		t.Helper()
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = current_schema() AND p.proname IN ('quest_member_age', 'quest_age_eligible')`).Scan(&n); err != nil {
			t.Fatalf("count functions: %v", err)
		}
		return n
	}
	atVersion17 := func() bool {
		return !columnExists("household_quests", "min_age") && !columnExists("household_quests", "max_age") &&
			!columnExists("family_members", "birthday") && functionCount() == 0
	}

	// 1. Back to version 17 if 18 is applied.
	if columnExists("household_quests", "min_age") {
		if _, err := tx.Exec(ctx, downSQL); err != nil {
			t.Fatalf("run down to version 17: %v", err)
		}
	}
	if !atVersion17() {
		t.Fatal("not at version 17 before seeding")
	}

	// 2. Seed at version 17: a household with two members and a shareable quest.
	hhID, annID, benID, questID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := tx.Exec(ctx, "INSERT INTO households (id, name, timezone) VALUES ($1, 'Age Fam', 'America/Chicago')", hhID); err != nil {
		t.Fatalf("insert household: %v", err)
	}
	for id, name := range map[uuid.UUID]string{annID: "Ann", benID: "Ben"} {
		if _, err := tx.Exec(ctx, "INSERT INTO family_members (id, household_id, name, points) VALUES ($1, $2, $3, 0)", id, hhID, name); err != nil {
			t.Fatalf("insert member %s: %v", name, err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO household_quests (id, household_id, name, points, repeat_days, shareable)
		VALUES ($1, $2, 'Mow the lawn', 20, 7, true)`, questID, hhID); err != nil {
		t.Fatalf("insert quest: %v", err)
	}

	// 3. Up: limits and birthdays are NULL everywhere.
	if _, err := tx.Exec(ctx, upSQL); err != nil {
		t.Fatalf("run up migration: %v", err)
	}
	var limited, withBirthday int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM household_quests WHERE min_age IS NOT NULL OR max_age IS NOT NULL),
		(SELECT count(*) FROM family_members WHERE birthday IS NOT NULL)`).Scan(&limited, &withBirthday); err != nil {
		t.Fatalf("count after up: %v", err)
	}
	if limited != 0 || withBirthday != 0 {
		t.Fatalf("after up: %d quests with limits, %d members with a birthday; want 0 and 0", limited, withBirthday)
	}
	if functionCount() != 2 {
		t.Fatalf("functions after up = %d, want 2", functionCount())
	}

	// The pre-existing member accepts and shares the pre-existing quest.
	q := db.New(tx)
	claimed, err := q.ClaimQuest(ctx, db.ClaimQuestParams{MemberID: &annID, HouseholdID: hhID, ID: questID})
	if err != nil {
		t.Fatalf("pre-existing member accepts pre-existing quest after up: %v", err)
	}
	if claimed.MinAge.Valid || claimed.MaxAge.Valid {
		t.Fatalf("claimed quest limits = %v/%v, want NULL", claimed.MinAge, claimed.MaxAge)
	}
	added, err := q.ShareQuest(ctx, db.ShareQuestParams{HouseholdID: hhID, ID: questID, MemberID: annID, TargetMemberID: benID})
	if err != nil || len(added) != 2 {
		t.Fatalf("pre-existing share after up: added %v, err %v; want holder and Ben", added, err)
	}

	// The CHECKs refuse negatives and min > max; a valid range and one-sided
	// limits are accepted.
	checks := []struct {
		name           string
		minAge, maxAge *int32
		ok             bool
	}{
		{"negative min", optInt(-1), nil, false},
		{"negative max", nil, optInt(-1), false},
		{"min above max", optInt(14), optInt(10), false},
		{"range", optInt(10), optInt(14), true},
		{"equal", optInt(10), optInt(10), true},
		{"min only", optInt(6), nil, true},
		{"max only", nil, optInt(12), true},
		{"zero", optInt(0), optInt(0), true},
	}
	for _, c := range checks {
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		_, err = sp.Exec(ctx, "UPDATE household_quests SET min_age = $2, max_age = $3 WHERE id = $1", questID, c.minAge, c.maxAge)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
		if err := sp.Rollback(ctx); err != nil && err != pgx.ErrTxClosed {
			t.Fatalf("rollback savepoint: %v", err)
		}
	}

	// 4. Down: columns, constraints and functions are gone.
	if _, err := tx.Exec(ctx, "UPDATE household_quests SET min_age = 10, max_age = 14 WHERE id = $1", questID); err != nil {
		t.Fatalf("set a range before down: %v", err)
	}
	if _, err := tx.Exec(ctx, "UPDATE family_members SET birthday = '2015-04-02' WHERE id = $1", annID); err != nil {
		t.Fatalf("set a birthday before down: %v", err)
	}
	if _, err := tx.Exec(ctx, downSQL); err != nil {
		t.Fatalf("run down: %v", err)
	}
	if !atVersion17() {
		t.Fatal("columns or functions remain after down")
	}
	var constraints int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conname IN ('household_quests_min_age_check', 'household_quests_max_age_check', 'household_quests_age_range_check')`).Scan(&constraints); err != nil {
		t.Fatalf("count constraints: %v", err)
	}
	if constraints != 0 {
		t.Fatalf("%d age constraints remain after down", constraints)
	}
	// Down is IF EXISTS throughout: running it again changes nothing.
	if _, err := tx.Exec(ctx, downSQL); err != nil {
		t.Fatalf("re-run down: %v", err)
	}
}
