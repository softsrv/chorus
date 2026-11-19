//go:build integration

package app_test

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// TestIntegration_QuestSharingAgeRange proves CLM-1, CLM-3, CLM-4, CLM-6, and CLM-7:
// QuestService.Share returns ErrAgeRestricted when the target member is outside
// the quest's min_age/max_age range, and keeps the quest unchanged; targets in range,
// on boundaries, with no birthday, or on all-ages quests succeed; cross-household
// targets and error precedences are preserved.
func TestIntegration_QuestSharingAgeRange(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	quests := app.NewQuestService(q, pool)
	ctx := context.Background()

	admin := registerVerified(t, pool, authSvc, mailer, "questshareage").User.ID
	const tz = "America/Chicago"
	h, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Age Sharers", Timezone: tz})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}
	other, err := households.Create(ctx, admin, app.HouseholdInput{Name: "Other Household", Timezone: tz})
	if err != nil {
		t.Fatalf("create other household: %v", err)
	}

	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	birthdayForAge := func(years int, daysOffset int) string {
		d := time.Date(today.Year()-years, today.Month(), today.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, daysOffset)
		return d.Format("2006-01-02")
	}

	newMemberWithAge := func(hid uuid.UUID, name, pin string, years int, daysOffset int) (uuid.UUID, app.MemberAction) {
		t.Helper()
		m, err := familySvc.Create(ctx, admin, hid, app.FamilyMemberInput{Name: name, PIN: pin}, "")
		if err != nil {
			t.Fatalf("create member %s: %v", name, err)
		}
		if years >= 0 {
			bd := birthdayForAge(years, daysOffset)
			if _, err := familySvc.SetBirthday(ctx, admin, hid, m.ID, &bd, ""); err != nil {
				t.Fatalf("set birthday for %s (%s): %v", name, bd, err)
			}
		}
		return m.ID, app.MemberAction{MemberID: m.ID, PIN: pin}
	}

	createQuest := func(name string, minAge, maxAge *int64, shareable bool) app.QuestView {
		t.Helper()
		in := app.QuestInput{
			Name:      name,
			Points:    num(20),
			Shareable: shareable,
		}
		if minAge != nil {
			in.MinAge = num(*minAge)
		}
		if maxAge != nil {
			in.MaxAge = num(*maxAge)
		}
		qv, err := quests.Create(ctx, admin, h.ID, in, "")
		if err != nil {
			t.Fatalf("create quest %s: %v", name, err)
		}
		return qv
	}

	holderID, holderAct := newMemberWithAge(h.ID, "Holder", "1111", 12, -30)
	_, target8Act := newMemberWithAge(h.ID, "Target8", "2222", 8, -30)
	_, target15Act := newMemberWithAge(h.ID, "Target15", "3333", 15, -30)
	_, targetTurns10TomorrowAct := newMemberWithAge(h.ID, "Target9", "4444", 10, 1)
	targetTurns10Today, targetTurns10TodayAct := newMemberWithAge(h.ID, "Target10", "5555", 10, 0)
	targetTurns15Tomorrow, targetTurns15TomorrowAct := newMemberWithAge(h.ID, "Target14", "6666", 15, 1)
	_, targetTurns15TodayAct := newMemberWithAge(h.ID, "Target15Boundary", "7777", 15, 0)
	target12, target12Act := newMemberWithAge(h.ID, "Target12", "8888", 12, -30)
	_, targetNoBdayAct := newMemberWithAge(h.ID, "TargetNoBday", "9999", -1, 0)

	target6, target6Act := newMemberWithAge(h.ID, "Target6", "", 6, -30)
	target40, target40Act := newMemberWithAge(h.ID, "Target40", "", 40, -30)
	_, target5Act := newMemberWithAge(h.ID, "Target5", "", 5, -30)
	_, target13Act := newMemberWithAge(h.ID, "Target13", "", 13, -30)
	target3, target3Act := newMemberWithAge(h.ID, "Target3", "", 3, -30)
	target90, target90Act := newMemberWithAge(h.ID, "Target90", "", 90, -30)
	_, outsiderOutOfRangeAct := newMemberWithAge(other.ID, "OutsiderOutOfRange", "", 8, -30)

	// CLM-1: shareable 10-14 quest held by in-range member.
	// Targets aged 8, 15, and boundary cases (9 vs 10, 14 vs 15) return ErrAgeRestricted.
	// Afterwards participants empty, claimed_by still holder, shared false.
	// Then an aged-12 target shares successfully.
	min10, max14 := int64(10), int64(14)
	q1 := createQuest("Mow the lawn (10-14)", &min10, &max14, true)
	if _, err := quests.Accept(ctx, admin, h.ID, q1.ID, holderAct); err != nil {
		t.Fatalf("holder accept: %v", err)
	}

	for _, tc := range []struct {
		name   string
		target app.MemberAction
	}{
		{"aged 8", target8Act},
		{"aged 15", target15Act},
		{"aged 9 (turns 10 tomorrow)", targetTurns10TomorrowAct},
		{"aged 15 (turns 15 today)", targetTurns15TodayAct},
	} {
		_, err := quests.Share(ctx, admin, h.ID, q1.ID, holderAct, tc.target)
		if !errors.Is(err, app.ErrAgeRestricted) {
			t.Errorf("share with %s: got %v, want ErrAgeRestricted", tc.name, err)
		}
		// Assert nothing is written: participants empty, claimed_by still holder, shared false.
		v, err := quests.Get(ctx, admin, h.ID, q1.ID)
		if err != nil {
			t.Fatalf("get quest: %v", err)
		}
		if len(v.Participants) != 0 {
			t.Errorf("after refused share with %s: participants=%v, want empty", tc.name, v.Participants)
		}
		if v.ClaimedBy == nil || *v.ClaimedBy != holderID {
			t.Errorf("after refused share with %s: claimed_by=%v, want holder %v", tc.name, v.ClaimedBy, holderID)
		}
		if v.Shared() {
			t.Errorf("after refused share with %s: shared=true, want false", tc.name)
		}
		if n := participantRows(t, pool, q1.ID); n != 0 {
			t.Errorf("after refused share with %s: %d participant rows, want 0", tc.name, n)
		}
	}

	// Share with aged-12 target then succeeds
	got, err := quests.Share(ctx, admin, h.ID, q1.ID, holderAct, target12Act)
	if err != nil {
		t.Fatalf("share with aged-12: %v", err)
	}
	if !got.Shared() {
		t.Errorf("after share with aged-12: shared=false, want true")
	}
	if want := []uuid.UUID{holderID, target12}; !slices.Equal(got.Participants, want) {
		t.Errorf("after share with aged-12: participants=%v, want %v", got.Participants, want)
	}
	if got.ClaimedBy == nil || *got.ClaimedBy != holderID {
		t.Errorf("after share with aged-12: claimed_by=%v, want holder %v", got.ClaimedBy, holderID)
	}

	// No age arithmetic in the non-test Go code of internal/app: the rule
	// lives in SQL (quest_age_eligible). The checker reads code only, never
	// comments, and is itself checked to flag a violation.
	checkAgeArithmeticChecker(t)
	err = filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		found, err := ageArithmeticInCode(path, nil)
		if err != nil {
			return err
		}
		for _, f := range found {
			t.Errorf("forbidden age arithmetic in Go code: %s", f)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/app: %v", err)
	}

	// CLM-3:
	// On a 10-14 quest, targets aged exactly 10, 12, 14 succeed (fresh quests).
	for _, tc := range []struct {
		name     string
		target   app.MemberAction
		targetID uuid.UUID
	}{
		{"aged exactly 10 (turns 10 today)", targetTurns10TodayAct, targetTurns10Today},
		{"aged 12", target12Act, target12},
		{"aged exactly 14 (turns 15 tomorrow)", targetTurns15TomorrowAct, targetTurns15Tomorrow},
	} {
		q := createQuest("10-14 "+tc.name, &min10, &max14, true)
		if _, err := quests.Accept(ctx, admin, h.ID, q.ID, holderAct); err != nil {
			t.Fatalf("accept %s: %v", tc.name, err)
		}
		res, err := quests.Share(ctx, admin, h.ID, q.ID, holderAct, tc.target)
		if err != nil {
			t.Errorf("10-14 share with %s: got %v, want success", tc.name, err)
		} else if !res.Shared() || !slices.Equal(res.Participants, []uuid.UUID{holderID, tc.targetID}) {
			t.Errorf("10-14 share with %s: participants=%v, want [%v, %v]", tc.name, res.Participants, holderID, tc.targetID)
		}
	}

	// On a 6+ quest, aged 6 and 40 succeed, aged 5 is ErrAgeRestricted.
	min6 := int64(6)
	q6 := createQuest("Read a chapter (6+)", &min6, nil, true)
	if _, err := quests.Accept(ctx, admin, h.ID, q6.ID, holderAct); err != nil {
		t.Fatalf("accept 6+: %v", err)
	}
	if _, err := quests.Share(ctx, admin, h.ID, q6.ID, holderAct, target5Act); !errors.Is(err, app.ErrAgeRestricted) {
		t.Errorf("6+ quest share with aged 5: got %v, want ErrAgeRestricted", err)
	}
	if res, err := quests.Share(ctx, admin, h.ID, q6.ID, holderAct, target6Act); err != nil || !res.Shared() || !slices.Equal(res.Participants, []uuid.UUID{holderID, target6}) {
		t.Errorf("6+ quest share with aged 6: got %v (shared=%v), want success", err, res.Shared())
	}
	q6b := createQuest("Read a chapter 40", &min6, nil, true)
	if _, err := quests.Accept(ctx, admin, h.ID, q6b.ID, holderAct); err != nil {
		t.Fatalf("accept 6+ b: %v", err)
	}
	if res, err := quests.Share(ctx, admin, h.ID, q6b.ID, holderAct, target40Act); err != nil || !res.Shared() || !slices.Equal(res.Participants, []uuid.UUID{holderID, target40}) {
		t.Errorf("6+ quest share with aged 40: got %v (shared=%v), want success", err, res.Shared())
	}

	// On an up-to-12 quest, aged 12 succeeds, aged 13 is refused.
	max12 := int64(12)
	q12 := createQuest("Water plants (<=12)", nil, &max12, true)
	if _, err := quests.Accept(ctx, admin, h.ID, q12.ID, holderAct); err != nil {
		t.Fatalf("accept <=12: %v", err)
	}
	if _, err := quests.Share(ctx, admin, h.ID, q12.ID, holderAct, target13Act); !errors.Is(err, app.ErrAgeRestricted) {
		t.Errorf("<=12 quest share with aged 13: got %v, want ErrAgeRestricted", err)
	}
	if res, err := quests.Share(ctx, admin, h.ID, q12.ID, holderAct, target12Act); err != nil || !res.Shared() || !slices.Equal(res.Participants, []uuid.UUID{holderID, target12}) {
		t.Errorf("<=12 quest share with aged 12: got %v (shared=%v), want success", err, res.Shared())
	}

	// CLM-4:
	// A no-birthday target shares on 10-14, 6+, up-to-12 quests.
	for _, qItem := range []struct {
		name   string
		minAge *int64
		maxAge *int64
	}{
		{"10-14 no-bday", &min10, &max14},
		{"6+ no-bday", &min6, nil},
		{"<=12 no-bday", nil, &max12},
	} {
		q := createQuest(qItem.name, qItem.minAge, qItem.maxAge, true)
		if _, err := quests.Accept(ctx, admin, h.ID, q.ID, holderAct); err != nil {
			t.Fatalf("accept %s: %v", qItem.name, err)
		}
		if res, err := quests.Share(ctx, admin, h.ID, q.ID, holderAct, targetNoBdayAct); err != nil || !res.Shared() {
			t.Errorf("%s share with no-birthday target: got %v, want success", qItem.name, err)
		}
	}

	// Aged 3 and aged 90 share on a null/null quest.
	qAll3 := createQuest("All ages (3)", nil, nil, true)
	if _, err := quests.Accept(ctx, admin, h.ID, qAll3.ID, holderAct); err != nil {
		t.Fatalf("accept all ages 3: %v", err)
	}
	if res, err := quests.Share(ctx, admin, h.ID, qAll3.ID, holderAct, target3Act); err != nil || !res.Shared() || !slices.Equal(res.Participants, []uuid.UUID{holderID, target3}) {
		t.Errorf("all-ages quest share with aged 3: got %v, want success", err)
	}
	qAll90 := createQuest("All ages (90)", nil, nil, true)
	if _, err := quests.Accept(ctx, admin, h.ID, qAll90.ID, holderAct); err != nil {
		t.Fatalf("accept all ages 90: %v", err)
	}
	if res, err := quests.Share(ctx, admin, h.ID, qAll90.ID, holderAct, target90Act); err != nil || !res.Shared() || !slices.Equal(res.Participants, []uuid.UUID{holderID, target90}) {
		t.Errorf("all-ages quest share with aged 90: got %v, want success", err)
	}

	// CLM-6:
	// A target member id from another household (with out-of-range birthday) -> ErrNotFound and nothing written.
	qCross := createQuest("Cross-household target", &min10, &max14, true)
	if _, err := quests.Accept(ctx, admin, h.ID, qCross.ID, holderAct); err != nil {
		t.Fatalf("accept cross: %v", err)
	}
	if _, err := quests.Share(ctx, admin, h.ID, qCross.ID, holderAct, outsiderOutOfRangeAct); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("share with outsider (out-of-range birthday): got %v, want ErrNotFound", err)
	}
	if n := participantRows(t, pool, qCross.ID); n != 0 {
		t.Errorf("cross-household share wrote %d participant rows, want 0", n)
	}

	// CLM-7: Error precedence:
	// 1. Out-of-range target on non-shareable quest -> ErrNotFound.
	qNonShareable := createQuest("Non-shareable 10-14", &min10, &max14, false)
	if _, err := quests.Accept(ctx, admin, h.ID, qNonShareable.ID, holderAct); err != nil {
		t.Fatalf("accept non-shareable: %v", err)
	}
	if _, err := quests.Share(ctx, admin, h.ID, qNonShareable.ID, holderAct, target8Act); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("share non-shareable with out-of-range target: got %v, want ErrNotFound", err)
	}

	// 2. Quest held by someone else -> ErrNotFound (even with out-of-range target).
	qHeldOther := createQuest("Held by other 10-14", &min10, &max14, true)
	if _, err := quests.Accept(ctx, admin, h.ID, qHeldOther.ID, holderAct); err != nil {
		t.Fatalf("accept held other: %v", err)
	}
	if _, err := quests.Share(ctx, admin, h.ID, qHeldOther.ID, target12Act, target8Act); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("share unheld quest with out-of-range target: got %v, want ErrNotFound", err)
	}

	// 3. Wrong target PIN -> ErrWrongTargetPIN (even when target is out of range).
	_, target8WithPINAct := newMemberWithAge(h.ID, "Target8WithPIN", "1234", 8, -30)
	wrongTargetPINAct := app.MemberAction{MemberID: target8WithPINAct.MemberID, PIN: "9999"}
	qWrongPIN := createQuest("Wrong PIN 10-14", &min10, &max14, true)
	if _, err := quests.Accept(ctx, admin, h.ID, qWrongPIN.ID, holderAct); err != nil {
		t.Fatalf("accept wrong pin quest: %v", err)
	}
	if _, err := quests.Share(ctx, admin, h.ID, qWrongPIN.ID, holderAct, wrongTargetPINAct); !errors.Is(err, app.ErrWrongTargetPIN) {
		t.Errorf("share with wrong target pin (out-of-range target): got %v, want ErrWrongTargetPIN", err)
	}

	// 4. A participant whose birthday is then moved out of range, shared again -> ErrAlreadyParticipant and stays participant.
	qMoveBday := createQuest("Move birthday 10-14", &min10, &max14, true)
	if _, err := quests.Accept(ctx, admin, h.ID, qMoveBday.ID, holderAct); err != nil {
		t.Fatalf("accept move bday quest: %v", err)
	}
	moveMemberID, moveMemberAct := newMemberWithAge(h.ID, "MoveMember", "", 12, -30)
	if _, err := quests.Share(ctx, admin, h.ID, qMoveBday.ID, holderAct, moveMemberAct); err != nil {
		t.Fatalf("initial share with MoveMember: %v", err)
	}
	// Move birthday out of range to age 16
	bd16 := birthdayForAge(16, -30)
	if _, err := familySvc.SetBirthday(ctx, admin, h.ID, moveMemberID, &bd16, ""); err != nil {
		t.Fatalf("update birthday to age 16: %v", err)
	}
	// Share again -> ErrAlreadyParticipant
	if _, err := quests.Share(ctx, admin, h.ID, qMoveBday.ID, holderAct, moveMemberAct); !errors.Is(err, app.ErrAlreadyParticipant) {
		t.Errorf("re-share with participant whose birthday moved out of range: got %v, want ErrAlreadyParticipant", err)
	}
	vMove, err := quests.Get(ctx, admin, h.ID, qMoveBday.ID)
	if err != nil {
		t.Fatalf("get qMoveBday: %v", err)
	}
	if !slices.Contains(vMove.Participants, moveMemberID) {
		t.Errorf("MoveMember is not in participants after re-share: %v", vMove.Participants)
	}

	// 5. Holder as target -> ErrAlreadyParticipant.
	qSelf := createQuest("Self share 10-14", &min10, &max14, true)
	if _, err := quests.Accept(ctx, admin, h.ID, qSelf.ID, holderAct); err != nil {
		t.Fatalf("accept self share: %v", err)
	}
	if _, err := quests.Share(ctx, admin, h.ID, qSelf.ID, holderAct, holderAct); !errors.Is(err, app.ErrAlreadyParticipant) {
		t.Errorf("share with holder as target: got %v, want ErrAlreadyParticipant", err)
	}
}

// sqlAgeArithmetic matches SQL age arithmetic inside a Go string literal.
var sqlAgeArithmetic = regexp.MustCompile(`(?i)\b(age|date_part|extract)\s*\(`)

// goDateMethods are the time methods that would restate the age rule in Go
// when called on (or with) a birthday.
var goDateMethods = map[string]bool{"Year": true, "YearDay": true, "AddDate": true, "Sub": true}

// ageArithmeticInCode parses one Go file (from src when non-nil, else from
// path) without its comments and reports code that restates the age rule:
// a string literal holding SQL age(/date_part(/extract(, a .Year(),
// .YearDay(), .AddDate() or .Sub() whose receiver or argument mentions a
// birthday, and time.Since/time.Until over a birthday. Comments are never
// read, so prose such as a doc comment mentioning an age is not code.
func ageArithmeticInCode(path string, src any) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	mentionsBirthday := func(e ast.Expr) bool {
		return strings.Contains(strings.ToLower(types.ExprString(e)), "birthday")
	}
	var found []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(n.Value)
			if err != nil {
				s = n.Value
			}
			if m := sqlAgeArithmetic.FindString(s); m != "" {
				found = append(found, fmt.Sprintf("%s: SQL %q in a string literal", fset.Position(n.Pos()), m))
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "time" && (sel.Sel.Name == "Since" || sel.Sel.Name == "Until") {
				for _, a := range n.Args {
					if mentionsBirthday(a) {
						found = append(found, fmt.Sprintf("%s: %s over a birthday", fset.Position(n.Pos()), types.ExprString(n)))
					}
				}
				return true
			}
			if !goDateMethods[sel.Sel.Name] {
				return true
			}
			hit := mentionsBirthday(sel.X)
			for _, a := range n.Args {
				hit = hit || mentionsBirthday(a)
			}
			if hit {
				found = append(found, fmt.Sprintf("%s: %s on a birthday", fset.Position(n.Pos()), types.ExprString(n)))
			}
		}
		return true
	})
	return found, nil
}

// checkAgeArithmeticChecker proves the guard can fail: it flags each kind of
// violation in an in-memory snippet, and passes comments and date code that
// does not touch a birthday.
func checkAgeArithmeticChecker(t *testing.T) {
	t.Helper()
	repl := strings.NewReplacer(
		"@AGE@", "ag"+"e(",
		"@DATEPART@", "date_"+"part(",
		"@AGE_SPACE@", "ag"+"e (",
		"@EXTRACT@", "EXT"+"RACT(",
		"@BDAY_YEAR@", "Birth"+"day.Year(",
		"@BDAY_ADDDATE@", "Birth"+"day.AddDate(",
		"@B@", "`",
	)
	violating := repl.Replace(`package p

import "time"

type member struct{ Birthday time.Time }

const q = "SELECT @AGE@birthday) FROM family_members"
const q2 = @B@SELECT @DATEPART@'year', now())@B@

func f(b member, now time.Time) {
	_ = b.@BDAY_YEAR@)
	_ = b.@BDAY_ADDDATE@10, 0, 0)
	_ = now.Sub(b.Birthday)
	_ = time.Since(b.Birthday)
}
`)
	found, err := ageArithmeticInCode("violating.go", violating)
	if err != nil {
		t.Fatalf("self-check parse violating snippet: %v", err)
	}
	if len(found) != 6 {
		t.Errorf("self-check: violating snippet flagged %d times, want 6: %v", len(found), found)
	}
	clean := repl.Replace(`package p

import "time"

// and @AGE_SPACE@nil for both when unset)
/* @EXTRACT@year FROM birthday) in a block comment */
func g(now time.Time, birthday *string) int {
	_ = "Minimum age can't be above the maximum age."
	return now.Year()
}
`)
	found, err = ageArithmeticInCode("clean.go", clean)
	if err != nil {
		t.Fatalf("self-check parse clean snippet: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("self-check: comment-only and unrelated code flagged: %v", found)
	}
}
