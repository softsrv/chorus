//go:build integration

package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// TestIntegration_HouseholdIsolation is the core authorization guarantee: a
// user who isn't a member of a household can't see or touch anything in it,
// and gets ErrNotFound (not ErrForbidden) so household IDs can't be probed.
func TestIntegration_HouseholdIsolation(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	chores := app.NewChoreService(q)
	events := app.NewEventService(q)
	ctx := context.Background()

	alice := registerVerified(t, pool, authSvc, mailer, "alice").User.ID
	mallory := registerVerified(t, pool, authSvc, mailer, "mallory").User.ID

	h, err := households.Create(ctx, alice, app.HouseholdInput{Name: "Alice Family", Timezone: "America/Chicago"})
	if err != nil {
		t.Fatalf("Create household: %v", err)
	}
	chore, err := chores.Create(ctx, alice, h.ID, app.ChoreInput{Title: "Dishes", AssigneeID: &alice})
	if err != nil {
		t.Fatalf("Create chore: %v", err)
	}
	now := time.Now()
	event, err := events.Create(ctx, alice, h.ID, app.EventInput{Title: "Dinner", StartsAt: now, EndsAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("Create event: %v", err)
	}

	// Mallory has her own household; IDs from it must not unlock Alice's data.
	mh, err := households.Create(ctx, mallory, app.HouseholdInput{Name: "Mallory"})
	if err != nil {
		t.Fatalf("Create mallory household: %v", err)
	}

	checks := map[string]error{}
	_, checks["get household"] = households.Get(ctx, mallory, h.ID)
	_, checks["update household"] = households.Update(ctx, mallory, h.ID, app.HouseholdInput{Name: "pwned"})
	checks["delete household"] = households.Delete(ctx, mallory, h.ID)
	_, checks["members"] = households.Members(ctx, mallory, h.ID)
	_, checks["invite"] = households.CreateInvite(ctx, mallory, h.ID)
	_, _, checks["list chores"] = chores.List(ctx, mallory, h.ID, app.ChoreFilter{Limit: 10})
	_, checks["get chore"] = chores.Get(ctx, mallory, h.ID, chore.ID)
	_, checks["update chore"] = chores.Update(ctx, mallory, h.ID, chore.ID, app.ChoreInput{Title: "x"})
	_, checks["complete chore"] = chores.SetCompleted(ctx, mallory, h.ID, chore.ID, true)
	checks["delete chore"] = chores.Delete(ctx, mallory, h.ID, chore.ID)
	_, checks["list events"] = events.ListRange(ctx, mallory, h.ID, now.Add(-time.Hour), now.Add(time.Hour))
	_, checks["get event"] = events.Get(ctx, mallory, h.ID, event.ID)
	checks["delete event"] = events.Delete(ctx, mallory, h.ID, event.ID)
	// Cross-household ID smuggling: Mallory's household ID + Alice's chore ID.
	_, checks["get chore via own household"] = chores.Get(ctx, mallory, mh.ID, chore.ID)
	checks["delete chore via own household"] = chores.Delete(ctx, mallory, mh.ID, chore.ID)
	_, checks["get event via own household"] = events.Get(ctx, mallory, mh.ID, event.ID)

	for name, err := range checks {
		if !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s: expected ErrNotFound, got %v", name, err)
		}
	}

	// Assigning a chore to a non-member is rejected.
	_, err = chores.Create(ctx, alice, h.ID, app.ChoreInput{Title: "Mow", AssigneeID: &mallory})
	var vErr *app.ValidationError
	if !errors.As(err, &vErr) || vErr.Fields["assignee_id"] == "" {
		t.Errorf("assign non-member: expected assignee_id validation error, got %v", err)
	}

	// Alice's data is intact.
	if got, err := chores.Get(ctx, alice, h.ID, chore.ID); err != nil || got.Title != "Dishes" || got.CompletedAt.Valid {
		t.Errorf("alice's chore was modified: %+v, %v", got, err)
	}
	if _, err := events.Get(ctx, alice, h.ID, event.ID); err != nil {
		t.Errorf("alice's event: %v", err)
	}
}

func TestIntegration_InvitesAndAdminRules(t *testing.T) {
	pool := testDB(t)
	mailer := &captureMailer{}
	authSvc := testAuthService(t, pool, mailer)
	q := db.New(pool)
	households := app.NewHouseholdService(q, pool)
	chores := app.NewChoreService(q)
	ctx := context.Background()

	parent := registerVerified(t, pool, authSvc, mailer, "parent").User.ID
	kid := registerVerified(t, pool, authSvc, mailer, "kid").User.ID

	h, err := households.Create(ctx, parent, app.HouseholdInput{Name: "Home"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	inv, err := households.CreateInvite(ctx, parent, h.ID)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	// Codes are forgiving about case and separators.
	joined, err := households.AcceptInvite(ctx, kid, " "+strings.ToLower(inv.Code)+" ")
	if err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
	if joined.ID != h.ID || joined.Role != app.RoleMember {
		t.Errorf("joined: got %+v", joined)
	}
	if _, err := households.AcceptInvite(ctx, kid, inv.Code); !errors.Is(err, app.ErrInviteInvalid) {
		t.Errorf("reused invite: expected ErrInviteInvalid, got %v", err)
	}

	// Members can manage chores but not the household.
	c, err := chores.Create(ctx, kid, h.ID, app.ChoreInput{Title: "Feed cat", AssigneeID: &kid})
	if err != nil {
		t.Fatalf("member create chore: %v", err)
	}
	if _, err := households.CreateInvite(ctx, kid, h.ID); !errors.Is(err, app.ErrForbidden) {
		t.Errorf("member invite: expected ErrForbidden, got %v", err)
	}
	if err := households.RemoveMember(ctx, kid, h.ID, parent); !errors.Is(err, app.ErrForbidden) {
		t.Errorf("member removes admin: expected ErrForbidden, got %v", err)
	}

	// The only admin can't demote themselves or leave while others remain.
	if err := households.SetMemberRole(ctx, parent, h.ID, parent, app.RoleMember); !errors.Is(err, app.ErrLastAdmin) {
		t.Errorf("demote last admin: expected ErrLastAdmin, got %v", err)
	}
	if err := households.RemoveMember(ctx, parent, h.ID, parent); !errors.Is(err, app.ErrLastAdmin) {
		t.Errorf("last admin leaves: expected ErrLastAdmin, got %v", err)
	}
	// Account deletion is blocked for the same reason.
	var hbErr app.HouseholdBlockingError
	if err := authSvc.DeleteAccount(ctx, parent, "correct-password-123"); !errors.As(err, &hbErr) {
		t.Errorf("delete sole admin account: expected HouseholdBlockingError, got %v", err)
	}

	// When the kid leaves, their chore becomes unassigned.
	if err := households.RemoveMember(ctx, kid, h.ID, kid); err != nil {
		t.Fatalf("kid leaves: %v", err)
	}
	got, err := chores.Get(ctx, parent, h.ID, c.ID)
	if err != nil {
		t.Fatalf("get chore: %v", err)
	}
	if got.AssigneeID != nil {
		t.Errorf("chore should be unassigned after assignee left, got %v", got.AssigneeID)
	}

	// Now the parent is the sole member; deleting the account deletes the household.
	if err := authSvc.DeleteAccount(ctx, parent, "correct-password-123"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM households WHERE id = $1", h.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("household should be deleted with its last member (count %d, err %v)", n, err)
	}
}
