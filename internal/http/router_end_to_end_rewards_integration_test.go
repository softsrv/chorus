//go:build integration

package http_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// endToEndFixture is what TestAPI_EndToEnd builds before its rewards
// section: the server, the parent's tokens, the household path and the ids
// the later sections read.
type endToEndFixture struct {
	s               *testServer
	parent          tokens
	hp              string // the second household's API path
	autoHouseholdID uuid.UUID
	memberID        uuid.UUID // the kid member the quest sections created
	copiedID        uuid.UUID // the quest copied from the library
	histPath        string    // the kid member's points-history path
}

// endToEndRewards is TestAPI_EndToEnd's rewards and outsider sections,
// run after the quest sections on the state they left.
func endToEndRewards(t *testing.T, f endToEndFixture) {
	s, parent, hp, histPath := f.s, f.parent, f.hp, f.histPath
	// ── Rewards ─────────────────────────────────────────────────────────────
	type rewardBody struct {
		ID          uuid.UUID  `json:"id"`
		LibraryID   *uuid.UUID `json:"library_id"`
		Description string     `json:"description"`
		Cost        int        `json:"cost"`
	}
	type rewardList struct {
		Items []rewardBody `json:"items"`
	}
	rewardPoints := func() map[uuid.UUID]int {
		var fl struct {
			Items []struct {
				ID     uuid.UUID `json:"id"`
				Points int       `json:"points"`
			} `json:"items"`
		}
		s.expect(http.StatusOK, "GET", hp+"/family-members", parent.AccessToken, nil, &fl)
		out := map[uuid.UUID]int{}
		for _, m := range fl.Items {
			out[m.ID] = m.Points
		}
		return out
	}
	pointsBefore := rewardPoints()

	// A household with no rewards answers {"items": []}, not null.
	autoHP := "/api/v1/households/" + f.autoHouseholdID.String()
	var rawRewards map[string]json.RawMessage
	s.expect(http.StatusOK, "GET", autoHP+"/rewards", parent.AccessToken, nil, &rawRewards)
	if string(rawRewards["items"]) != "[]" {
		t.Errorf("empty rewards: items = %s, want []", rawRewards["items"])
	}
	rawRewards = nil
	s.expect(http.StatusOK, "GET", hp+"/rewards", parent.AccessToken, nil, &rawRewards)
	if string(rawRewards["items"]) != "[]" {
		t.Errorf("empty rewards (second household): items = %s, want []", rawRewards["items"])
	}

	// Create, list, get, edit, delete.
	var reward rewardBody
	s.expect(http.StatusCreated, "POST", hp+"/rewards", parent.AccessToken,
		map[string]any{"description": "Ice cream trip", "cost": 250}, &reward)
	if reward.ID == uuid.Nil || reward.Description != "Ice cream trip" || reward.Cost != 250 || reward.LibraryID != nil {
		t.Errorf("created reward: %+v", reward)
	}
	rp := hp + "/rewards/" + reward.ID.String()
	var rl rewardList
	s.expect(http.StatusOK, "GET", hp+"/rewards", parent.AccessToken, nil, &rl)
	if len(rl.Items) != 1 || rl.Items[0] != reward {
		t.Errorf("reward list after create: %+v", rl.Items)
	}
	var gotReward rewardBody
	s.expect(http.StatusOK, "GET", rp, parent.AccessToken, nil, &gotReward)
	if gotReward != reward {
		t.Errorf("get reward: %+v, want %+v", gotReward, reward)
	}
	s.expect(http.StatusOK, "PUT", rp, parent.AccessToken, map[string]any{"description": "Ice cream sundae", "cost": 300}, nil)
	s.expect(http.StatusOK, "GET", rp, parent.AccessToken, nil, &gotReward)
	if gotReward.Description != "Ice cream sundae" || gotReward.Cost != 300 {
		t.Errorf("reward after edit: %+v", gotReward)
	}

	// Validation: each bad field gets its own error and nothing is saved.
	for _, tc := range []struct {
		name   string
		body   map[string]any
		fields []string
	}{
		{"empty description and negative cost", map[string]any{"description": "", "cost": -5}, []string{"description", "cost"}},
		{"whitespace description", map[string]any{"description": "   ", "cost": 5}, []string{"description"}},
		{"long description", map[string]any{"description": strings.Repeat("x", 201), "cost": 5}, []string{"description"}},
		{"non-numeric cost", map[string]any{"description": "Ok", "cost": "abc"}, []string{"cost"}},
		{"fractional cost", map[string]any{"description": "Ok", "cost": 1.5}, []string{"cost"}},
		{"missing cost", map[string]any{"description": "Ok"}, []string{"cost"}},
		{"cost too large", map[string]any{"description": "Ok", "cost": int64(1) << 40}, []string{"cost"}},
	} {
		for _, target := range []struct{ method, path string }{{"POST", hp + "/rewards"}, {"PUT", rp}} {
			var rerr apiError
			got := s.do(target.method, target.path, parent.AccessToken, tc.body, &rerr)
			if got != http.StatusUnprocessableEntity {
				t.Errorf("%s %s (%s): status %d, want 422", target.method, target.path, tc.name, got)
				continue
			}
			for _, f := range tc.fields {
				if rerr.Error.Fields[f] == "" {
					t.Errorf("%s (%s): no %q field error in %+v", target.method, tc.name, f, rerr.Error.Fields)
				}
			}
			if len(rerr.Error.Fields) != len(tc.fields) {
				t.Errorf("%s (%s): field errors %+v, want exactly %v", target.method, tc.name, rerr.Error.Fields, tc.fields)
			}
		}
	}
	// Unknown fields are still rejected outright.
	s.expect(http.StatusBadRequest, "POST", hp+"/rewards", parent.AccessToken, map[string]any{"description": "Ok", "cost": 1, "extra": 1}, nil)
	rl = rewardList{}
	s.expect(http.StatusOK, "GET", hp+"/rewards", parent.AccessToken, nil, &rl)
	if len(rl.Items) != 1 || rl.Items[0].Description != "Ice cream sundae" || rl.Items[0].Cost != 300 {
		t.Errorf("rewards after rejected writes: %+v; want only the edited reward", rl.Items)
	}

	// Library: global, read-only, seeded with 6 rewards ordered by cost.
	var rewardLib struct {
		Items []rewardBody `json:"items"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/reward-library", parent.AccessToken, nil, &rewardLib)
	var ten *rewardBody
	for i := range rewardLib.Items {
		if rewardLib.Items[i].Description == "$10" {
			ten = &rewardLib.Items[i]
		}
	}
	if len(rewardLib.Items) != 6 || ten == nil || ten.Cost != 1000 {
		t.Fatalf("reward library: %+v", rewardLib.Items)
	}
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		for _, p := range []string{"/api/v1/reward-library", "/api/v1/reward-library/" + ten.ID.String()} {
			if got := s.do(m, p, parent.AccessToken, map[string]any{"description": "pwned", "cost": 0}, nil); got != http.StatusNotFound && got != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: status %d, want 404 or 405", m, p, got)
			}
		}
	}

	// Adopt from the library, then edit the copy: the library is unchanged.
	var adopted rewardBody
	s.expect(http.StatusCreated, "POST", hp+"/rewards/from-library", parent.AccessToken, map[string]any{"library_id": ten.ID}, &adopted)
	if adopted.LibraryID == nil || *adopted.LibraryID != ten.ID || adopted.Description != "$10" || adopted.Cost != 1000 {
		t.Errorf("adopted reward: %+v", adopted)
	}
	s.expect(http.StatusOK, "PUT", hp+"/rewards/"+adopted.ID.String(), parent.AccessToken,
		map[string]any{"description": "Twenty dollars", "cost": 2000}, nil)
	var rewardLibAfter struct {
		Items []rewardBody `json:"items"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/reward-library", parent.AccessToken, nil, &rewardLibAfter)
	if len(rewardLibAfter.Items) != len(rewardLib.Items) {
		t.Fatalf("reward library size changed: %d -> %d", len(rewardLib.Items), len(rewardLibAfter.Items))
	}
	for i := range rewardLib.Items {
		if rewardLib.Items[i] != rewardLibAfter.Items[i] {
			t.Errorf("reward library row %d changed: %+v -> %+v", i, rewardLib.Items[i], rewardLibAfter.Items[i])
		}
	}
	var adoptErr apiError
	s.expect(http.StatusUnprocessableEntity, "POST", hp+"/rewards/from-library", parent.AccessToken, map[string]any{}, &adoptErr)
	if adoptErr.Error.Fields["library_id"] == "" {
		t.Errorf("adopt without library_id: %+v", adoptErr)
	}
	s.expect(http.StatusNotFound, "POST", hp+"/rewards/from-library", parent.AccessToken, map[string]any{"library_id": uuid.New()}, nil)

	// No spend or buy route exists, and no reward action moved points.
	// (Redeem exists; TestAPI_RewardRedemption covers it.)
	for _, action := range []string{"spend", "buy"} {
		if got := s.do("POST", rp+"/"+action, parent.AccessToken, map[string]any{"member_id": f.memberID, "pin": "4321"}, nil); got != http.StatusNotFound && got != http.StatusMethodNotAllowed {
			t.Errorf("POST rewards/{id}/%s: status %d, want 404 or 405", action, got)
		}
	}
	s.expect(http.StatusNoContent, "DELETE", hp+"/rewards/"+adopted.ID.String(), parent.AccessToken, nil, nil)
	s.expect(http.StatusNoContent, "DELETE", rp, parent.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "GET", rp, parent.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "DELETE", rp, parent.AccessToken, nil, nil)
	rl = rewardList{}
	s.expect(http.StatusOK, "GET", hp+"/rewards", parent.AccessToken, nil, &rl)
	if len(rl.Items) != 0 {
		t.Errorf("rewards after delete: %+v", rl.Items)
	}
	if after := rewardPoints(); len(after) != len(pointsBefore) {
		t.Errorf("family members changed during the reward flow: %v -> %v", pointsBefore, after)
	} else {
		for id, p := range pointsBefore {
			if after[id] != p {
				t.Errorf("member %s points changed by a reward action: %d -> %d", id, p, after[id])
			}
		}
	}
	// A reward left in place for the outsider checks below.
	var keptReward rewardBody
	s.expect(http.StatusCreated, "POST", hp+"/rewards", parent.AccessToken, map[string]any{"description": "Kept", "cost": 10}, &keptReward)
	krp := hp + "/rewards/" + keptReward.ID.String()

	// Outsiders see nothing.
	outsider := s.signUp("outsider")
	s.expect(http.StatusNotFound, "GET", hp, outsider.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "GET", hp+"/calendar", outsider.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "GET", hp+"/quests", outsider.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "GET", hp+"/quests/"+f.copiedID.String(), outsider.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "GET", histPath, outsider.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "GET", hp+"/rewards", outsider.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "GET", krp, outsider.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "POST", hp+"/rewards", outsider.AccessToken, map[string]any{"description": "Evil", "cost": 1}, nil)
	s.expect(http.StatusNotFound, "PUT", krp, outsider.AccessToken, map[string]any{"description": "Pwned", "cost": 0}, nil)
	s.expect(http.StatusNotFound, "DELETE", krp, outsider.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "POST", hp+"/rewards/from-library", outsider.AccessToken, map[string]any{"library_id": ten.ID}, nil)
	// The outsider's own household plus the parent's reward id finds nothing.
	var outsiderHouseholds struct {
		Items []struct {
			ID uuid.UUID `json:"id"`
		} `json:"items"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/households", outsider.AccessToken, nil, &outsiderHouseholds)
	if len(outsiderHouseholds.Items) != 1 {
		t.Fatalf("outsider households: %+v", outsiderHouseholds.Items)
	}
	ohp := "/api/v1/households/" + outsiderHouseholds.Items[0].ID.String()
	s.expect(http.StatusNotFound, "GET", ohp+"/rewards/"+keptReward.ID.String(), outsider.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "PUT", ohp+"/rewards/"+keptReward.ID.String(), outsider.AccessToken, map[string]any{"description": "Smuggled", "cost": 0}, nil)
	s.expect(http.StatusNotFound, "DELETE", ohp+"/rewards/"+keptReward.ID.String(), outsider.AccessToken, nil, nil)
	s.expect(http.StatusOK, "GET", krp, parent.AccessToken, nil, &gotReward)
	if gotReward.Description != "Kept" || gotReward.Cost != 10 {
		t.Errorf("parent's reward changed by the outsider: %+v", gotReward)
	}
	// Every verified user reads the same library bytes.
	libBytes := func(token string) string {
		req, _ := http.NewRequest("GET", s.srv.URL+"/api/v1/reward-library", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET reward-library: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if a, b := libBytes(parent.AccessToken), libBytes(outsider.AccessToken); a != b {
		t.Errorf("reward library differs between households:\n%s\n%s", a, b)
	}
}
