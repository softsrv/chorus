//go:build integration

package http_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

type comboWire struct {
	ID          uuid.UUID   `json:"id"`
	Name        string      `json:"name"`
	BonusPoints int32       `json:"bonus_points"`
	QuestIDs    []uuid.UUID `json:"quest_ids"`
}

type comboListWire struct {
	Items []comboWire `json:"items"`
}

func TestAPI_Combos(t *testing.T) {
	s := newTestServer(t)
	user := s.signUp("combouser")

	var hh struct {
		Items []struct {
			ID uuid.UUID `json:"id"`
		} `json:"items"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/households", user.AccessToken, nil, &hh)
	if len(hh.Items) != 1 {
		t.Fatalf("expected 1 household, got %d", len(hh.Items))
	}
	hid := hh.Items[0].ID.String()
	basePath := "/api/v1/households/" + hid + "/combos"

	// 1. Initial list is empty: {"items": []}
	var listResp comboListWire
	s.expect(http.StatusOK, "GET", basePath, user.AccessToken, nil, &listResp)
	if listResp.Items == nil || len(listResp.Items) != 0 {
		t.Fatalf("expected empty items array, got %+v", listResp.Items)
	}

	// Create quests for combos
	var q1, q2, q3 struct {
		ID uuid.UUID `json:"id"`
	}
	s.expect(http.StatusCreated, "POST", "/api/v1/households/"+hid+"/quests", user.AccessToken,
		map[string]any{"name": "Chore One", "points": 10}, &q1)
	s.expect(http.StatusCreated, "POST", "/api/v1/households/"+hid+"/quests", user.AccessToken,
		map[string]any{"name": "Chore Two", "points": 15}, &q2)
	s.expect(http.StatusCreated, "POST", "/api/v1/households/"+hid+"/quests", user.AccessToken,
		map[string]any{"name": "Chore Three", "points": 20}, &q3)

	// 2. 422 on invalid create (empty name, negative bonus, only 1 chore)
	var errResp apiError
	st := s.do("POST", basePath, user.AccessToken, map[string]any{
		"name":         "   ",
		"bonus_points": -5,
		"quest_ids":    []uuid.UUID{q1.ID},
	}, &errResp)
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("invalid combo create status: got %d, want 422", st)
	}
	if errResp.Error.Fields["name"] == "" || errResp.Error.Fields["bonus_points"] == "" || errResp.Error.Fields["quest_ids"] == "" {
		t.Fatalf("expected 422 fields name, bonus_points, quest_ids; got %+v", errResp.Error.Fields)
	}

	// 3. POST 201: create valid combo
	var created comboWire
	s.expect(http.StatusCreated, "POST", basePath, user.AccessToken, map[string]any{
		"name":         "Morning Duo",
		"bonus_points": 25,
		"quest_ids":    []uuid.UUID{q1.ID, q2.ID},
	}, &created)
	if created.ID == uuid.Nil || created.Name != "Morning Duo" || created.BonusPoints != 25 || len(created.QuestIDs) != 2 {
		t.Fatalf("unexpected created combo: %+v", created)
	}
	comboPath := basePath + "/" + created.ID.String()

	// 4. GET list 200: {items: [...]}
	s.expect(http.StatusOK, "GET", basePath, user.AccessToken, nil, &listResp)
	if len(listResp.Items) != 1 || listResp.Items[0].ID != created.ID {
		t.Fatalf("expected 1 combo in list, got %+v", listResp)
	}

	// 5. GET one 200
	var got comboWire
	s.expect(http.StatusOK, "GET", comboPath, user.AccessToken, nil, &got)
	if got.ID != created.ID || got.Name != "Morning Duo" || got.BonusPoints != 25 {
		t.Fatalf("unexpected get combo: %+v", got)
	}

	// 6. PUT 200: full replacement
	var updated comboWire
	s.expect(http.StatusOK, "PUT", comboPath, user.AccessToken, map[string]any{
		"name":         "Evening Trio",
		"bonus_points": 50,
		"quest_ids":    []uuid.UUID{q2.ID, q3.ID},
	}, &updated)
	if updated.Name != "Evening Trio" || updated.BonusPoints != 50 || len(updated.QuestIDs) != 2 {
		t.Fatalf("unexpected updated combo: %+v", updated)
	}

	// 7. Admin PIN protection (CLM-9)
	s.expect(http.StatusOK, "PUT", "/api/v1/households/"+hid+"/admin-pin", user.AccessToken,
		map[string]string{"pin": "4321"}, nil)

	// Create without PIN -> 403 admin_pin_required
	var pinErr apiError
	s.expect(http.StatusForbidden, "POST", basePath, user.AccessToken, map[string]any{
		"name": "New Combo", "bonus_points": 10, "quest_ids": []uuid.UUID{q1.ID, q2.ID},
	}, &pinErr)
	if pinErr.Error.Code != "admin_pin_required" {
		t.Fatalf("missing pin on create: got code %q, want admin_pin_required", pinErr.Error.Code)
	}

	// Create with wrong PIN -> 403 wrong_pin
	s.expectWithHeaders(http.StatusForbidden, "POST", basePath, user.AccessToken,
		map[string]string{"X-Admin-PIN": "9999"},
		map[string]any{"name": "New Combo", "bonus_points": 10, "quest_ids": []uuid.UUID{q1.ID, q2.ID}},
		&pinErr)
	if pinErr.Error.Code != "wrong_pin" {
		t.Fatalf("wrong pin on create: got code %q, want wrong_pin", pinErr.Error.Code)
	}

	// Update without PIN -> 403 admin_pin_required
	s.expect(http.StatusForbidden, "PUT", comboPath, user.AccessToken, map[string]any{
		"name": "Updated", "bonus_points": 10, "quest_ids": []uuid.UUID{q1.ID, q2.ID},
	}, &pinErr)
	if pinErr.Error.Code != "admin_pin_required" {
		t.Fatalf("missing pin on update: got code %q, want admin_pin_required", pinErr.Error.Code)
	}

	// Update with wrong PIN -> 403 wrong_pin
	s.expectWithHeaders(http.StatusForbidden, "PUT", comboPath, user.AccessToken,
		map[string]string{"X-Admin-PIN": "9999"},
		map[string]any{"name": "Updated", "bonus_points": 10, "quest_ids": []uuid.UUID{q1.ID, q2.ID}},
		&pinErr)
	if pinErr.Error.Code != "wrong_pin" {
		t.Fatalf("wrong pin on update: got code %q, want wrong_pin", pinErr.Error.Code)
	}

	// Delete without PIN -> 403 admin_pin_required
	s.expect(http.StatusForbidden, "DELETE", comboPath, user.AccessToken, nil, &pinErr)
	if pinErr.Error.Code != "admin_pin_required" {
		t.Fatalf("missing pin on delete: got code %q, want admin_pin_required", pinErr.Error.Code)
	}

	// Delete with wrong PIN -> 403 wrong_pin
	s.expectWithHeaders(http.StatusForbidden, "DELETE", comboPath, user.AccessToken,
		map[string]string{"X-Admin-PIN": "9999"}, nil, &pinErr)
	if pinErr.Error.Code != "wrong_pin" {
		t.Fatalf("wrong pin on delete: got code %q, want wrong_pin", pinErr.Error.Code)
	}

	// 8. DELETE 204 with correct PIN
	s.expectWithHeaders(http.StatusNoContent, "DELETE", comboPath, user.AccessToken,
		map[string]string{"X-Admin-PIN": "4321"}, nil, nil)

	// 9. GET one now 404
	s.expect(http.StatusNotFound, "GET", comboPath, user.AccessToken, nil, nil)

	// 10. GET list now empty {items: []}
	s.expect(http.StatusOK, "GET", basePath, user.AccessToken, nil, &listResp)
	if listResp.Items == nil || len(listResp.Items) != 0 {
		t.Fatalf("expected empty items after delete, got %+v", listResp.Items)
	}
}
