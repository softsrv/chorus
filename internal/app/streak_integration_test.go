//go:build integration

package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// TestIntegration_PointsHistory verifies CLM-4:
// - A new member with no awards gets an empty, non-nil slice (len 0), not an error.
// - Three sequential quest approvals produce history entries ordered newest first (created_at DESC, id DESC).
// - Each entry carries matching earned_at (date), points, and reason.
// - Limit and offset paging is honoured.
func TestIntegration_PointsHistory(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	quests := app.NewQuestService(q, pool)
	familySvc := app.NewFamilyService(q, pool, 10)
	ctx := context.Background()

	parent := registerVerified(t, pool, authSvc, mailer, "pointshistory").User.ID
	h, err := households.Create(ctx, parent, app.HouseholdInput{Name: "Points History Family"})
	if err != nil {
		t.Fatalf("create household: %v", err)
	}

	member, err := familySvc.Create(ctx, parent, h.ID, app.FamilyMemberInput{Name: "Sam", PIN: "5678"}, "")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}

	// 1. Member with no awards gets an empty non-nil slice, not an error.
	history, err := familySvc.PointsHistory(ctx, parent, h.ID, member.ID, "5678", "", 50, 0)
	if err != nil {
		t.Fatalf("PointsHistory for member with no awards returned error: %v", err)
	}
	if history == nil {
		t.Fatal("PointsHistory returned nil slice, want non-nil empty slice")
	}
	if len(history) != 0 {
		t.Fatalf("PointsHistory returned %d awards, want 0", len(history))
	}

	// 2. Approve three quests sequentially.
	var questNames = []string{"Tidy room", "Walk dog", "Do homework"}
	var questPoints = []int32{5, 10, 15}
	asSam := app.MemberAction{MemberID: member.ID, PIN: "5678"}

	var approvalTimes []time.Time
	for i := range 3 {
		questRow, err := quests.Create(ctx, parent, h.ID, app.QuestInput{
			Name:       questNames[i],
			Points:     num(int64(questPoints[i])),
			RepeatDays: num(1),
		}, "")
		if err != nil {
			t.Fatalf("create quest %d: %v", i, err)
		}

		if _, err := quests.Accept(ctx, parent, h.ID, questRow.ID, asSam); err != nil {
			t.Fatalf("accept quest %d: %v", i, err)
		}
		if _, err := quests.MarkDone(ctx, parent, h.ID, questRow.ID, asSam); err != nil {
			t.Fatalf("mark done quest %d: %v", i, err)
		}

		appr, _, err := quests.Approve(ctx, parent, h.ID, questRow.ID, "")
		if err != nil {
			t.Fatalf("approve quest %d: %v", i, err)
		}
		if !appr.CompletedAt.Valid {
			t.Fatalf("quest %d completed_at is not valid", i)
		}
		approvalTimes = append(approvalTimes, appr.CompletedAt.Time)
		// Small pause to guarantee distinct created_at / completed_at timestamps if required
		time.Sleep(10 * time.Millisecond)
	}

	// 3. Query all awards (default limit/offset)
	allAwards, err := familySvc.PointsHistory(ctx, parent, h.ID, member.ID, "5678", "", 50, 0)
	if err != nil {
		t.Fatalf("PointsHistory query failed: %v", err)
	}
	if len(allAwards) != 3 {
		t.Fatalf("expected 3 awards, got %d: %+v", len(allAwards), allAwards)
	}

	// Assert newest first: index 0 should be quest 2 ("Do homework"), index 1 quest 1 ("Walk dog"), index 2 quest 0 ("Tidy room")
	for i := range 3 {
		expectedIdx := 2 - i
		got := allAwards[i]
		wantName := questNames[expectedIdx]
		wantPts := questPoints[expectedIdx]
		wantTime := approvalTimes[expectedIdx]

		if got.Reason != wantName {
			t.Errorf("award %d Reason mismatch: got %q, want %q", i, got.Reason, wantName)
		}
		if got.Points != wantPts {
			t.Errorf("award %d Points mismatch: got %d, want %d", i, got.Points, wantPts)
		}
		if got.Kind != "quest" {
			t.Errorf("award %d Kind mismatch: got %q, want 'quest'", i, got.Kind)
		}
		if !got.EarnedAt.Equal(wantTime) {
			t.Errorf("award %d EarnedAt mismatch: got %v, want %v", i, got.EarnedAt, wantTime)
		}
	}

	// Verify ordering by CreatedAt DESC
	if allAwards[0].CreatedAt.Before(allAwards[1].CreatedAt) || allAwards[1].CreatedAt.Before(allAwards[2].CreatedAt) {
		t.Errorf("expected awards ordered newest first: %v, %v, %v",
			allAwards[0].CreatedAt, allAwards[1].CreatedAt, allAwards[2].CreatedAt)
	}

	// 4. Test paging (limit / offset)
	// Page 1: limit 2, offset 0 -> returns first 2 awards (indices 0 and 1)
	page1, err := familySvc.PointsHistory(ctx, parent, h.ID, member.ID, "5678", "", 2, 0)
	if err != nil {
		t.Fatalf("page 1 failed: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page 1 len got %d, want 2", len(page1))
	}
	if page1[0].ID != allAwards[0].ID || page1[1].ID != allAwards[1].ID {
		t.Errorf("page 1 items do not match first 2 awards: %+v vs %+v", page1, allAwards[:2])
	}

	// Page 2: limit 2, offset 2 -> returns remaining 1 award (index 2)
	page2, err := familySvc.PointsHistory(ctx, parent, h.ID, member.ID, "5678", "", 2, 2)
	if err != nil {
		t.Fatalf("page 2 failed: %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("page 2 len got %d, want 1", len(page2))
	}
	if page2[0].ID != allAwards[2].ID {
		t.Errorf("page 2 item does not match third award: %+v vs %+v", page2[0], allAwards[2])
	}

	// Page 3: limit 2, offset 4 -> returns empty non-nil slice
	page3, err := familySvc.PointsHistory(ctx, parent, h.ID, member.ID, "5678", "", 2, 4)
	if err != nil {
		t.Fatalf("page 3 failed: %v", err)
	}
	if page3 == nil || len(page3) != 0 {
		t.Errorf("page 3 expected empty non-nil slice, got %+v", page3)
	}
}
