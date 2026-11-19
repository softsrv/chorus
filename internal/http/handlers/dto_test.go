package handlers

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

func TestFamilyMemberJSON(t *testing.T) {
	t.Parallel()

	memberID := uuid.MustParse("018f25b3-8b77-7000-8000-000000000001")
	householdID := uuid.MustParse("018f25b3-8b77-7000-8000-000000000002")

	testCases := []struct {
		name       string
		dbMember   db.FamilyMember
		wantHasPIN bool
	}{
		{
			name: "with pin hash",
			dbMember: db.FamilyMember{
				ID:          memberID,
				HouseholdID: householdID,
				Name:        "Alice",
				PinHash:     pgtype.Text{String: "$2a$12$dummyhashforpin", Valid: true},
				Points:      15,
			},
			wantHasPIN: true,
		},
		{
			name: "without pin hash (null)",
			dbMember: db.FamilyMember{
				ID:          memberID,
				HouseholdID: householdID,
				Name:        "Bob",
				PinHash:     pgtype.Text{String: "", Valid: false},
				Points:      0,
			},
			wantHasPIN: false,
		},
		{
			name: "with empty valid pin hash string",
			dbMember: db.FamilyMember{
				ID:          memberID,
				HouseholdID: householdID,
				Name:        "Charlie",
				PinHash:     pgtype.Text{String: "", Valid: true},
				Points:      5,
			},
			wantHasPIN: false,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dto := toFamilyMember(tc.dbMember)

			if dto.ID != tc.dbMember.ID {
				t.Errorf("ID mismatch: got %v, want %v", dto.ID, tc.dbMember.ID)
			}
			if dto.Name != tc.dbMember.Name {
				t.Errorf("Name mismatch: got %q, want %q", dto.Name, tc.dbMember.Name)
			}
			if dto.HasPIN != tc.wantHasPIN {
				t.Errorf("HasPIN mismatch: got %v, want %v", dto.HasPIN, tc.wantHasPIN)
			}
			if dto.Points != tc.dbMember.Points {
				t.Errorf("Points mismatch: got %d, want %d", dto.Points, tc.dbMember.Points)
			}

			data, err := json.Marshal(dto)
			if err != nil {
				t.Fatalf("json.Marshal failed: %v", err)
			}

			var raw map[string]any
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatalf("json.Unmarshal failed: %v", err)
			}

			expectedKeys := []string{"id", "name", "has_pin", "points"}
			if len(raw) != len(expectedKeys) {
				t.Errorf("expected exactly %d keys in JSON, got %d: %+v", len(expectedKeys), len(raw), raw)
			}
			for _, k := range expectedKeys {
				if _, ok := raw[k]; !ok {
					t.Errorf("missing expected key %q in JSON: %+v", k, raw)
				}
			}

			// Must not contain pin_hash, pin, or the raw hash value
			if _, ok := raw["pin_hash"]; ok {
				t.Errorf("forbidden key 'pin_hash' present in marshalled JSON")
			}
			if _, ok := raw["pin"]; ok {
				t.Errorf("forbidden key 'pin' present in marshalled JSON")
			}
			if strings.Contains(string(data), "dummyhashforpin") {
				t.Errorf("marshalled JSON leaked pin hash value: %s", string(data))
			}

			// Ensure familyMemberJSON struct has no forbidden fields via reflection
			valType := reflect.TypeOf(dto)
			for i := 0; i < valType.NumField(); i++ {
				field := valType.Field(i)
				tag := field.Tag.Get("json")
				if strings.Contains(tag, "pin_hash") || strings.EqualFold(field.Name, "PinHash") || strings.EqualFold(field.Name, "Pin") {
					t.Errorf("forbidden field %s with tag %q found in familyMemberJSON struct", field.Name, tag)
				}
			}
		})
	}
}

func TestPointsAwardJSON(t *testing.T) {
	t.Parallel()

	awardID := uuid.MustParse("018f25b3-8b77-7000-8000-000000000003")
	earnedAt := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

	appAward := app.PointsAward{
		ID:        awardID,
		Kind:      "quest",
		Reason:    "Wash the car",
		Points:    40,
		EarnedAt:  earnedAt,
		CreatedAt: earnedAt.Add(time.Second),
	}

	dto := toPointsAward(appAward)

	if dto.ID != appAward.ID {
		t.Errorf("ID mismatch: got %v, want %v", dto.ID, appAward.ID)
	}
	if dto.Kind != appAward.Kind {
		t.Errorf("Kind mismatch: got %q, want %q", dto.Kind, appAward.Kind)
	}
	if dto.Reason != appAward.Reason {
		t.Errorf("Reason mismatch: got %q, want %q", dto.Reason, appAward.Reason)
	}
	if dto.Points != appAward.Points {
		t.Errorf("Points mismatch: got %d, want %d", dto.Points, appAward.Points)
	}
	if !dto.EarnedAt.Equal(appAward.EarnedAt) {
		t.Errorf("EarnedAt mismatch: got %v, want %v", dto.EarnedAt, appAward.EarnedAt)
	}

	data, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	expectedKeys := []string{"id", "kind", "reason", "points", "earned_at"}
	if len(raw) != len(expectedKeys) {
		t.Errorf("expected exactly %d keys in JSON, got %d: %+v", len(expectedKeys), len(raw), raw)
	}
	for _, k := range expectedKeys {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing expected key %q in JSON: %+v", k, raw)
		}
	}
}

// TestQuestStreakBonusJSON pins that a household quest and a library quest
// both carry their streak bonus to the client as streak_bonus.
func TestQuestStreakBonusJSON(t *testing.T) {
	t.Parallel()

	streakBonus := func(v any) any {
		t.Helper()
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal failed: %v", err)
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("json.Unmarshal failed: %v", err)
		}
		got, ok := raw["streak_bonus"]
		if !ok {
			t.Fatalf("streak_bonus missing from %s", data)
		}
		return got
	}

	quest := toQuest(app.QuestView{HouseholdQuest: db.HouseholdQuest{
		ID: uuid.New(), HouseholdID: uuid.New(), Name: "Feed the cat", Points: 3, RepeatDays: 1, StreakBonus: 7,
	}})
	if got := streakBonus(quest); got != float64(7) {
		t.Errorf("quest streak_bonus = %v, want 7", got)
	}
	none := toQuest(app.QuestView{HouseholdQuest: db.HouseholdQuest{ID: uuid.New(), Name: "Dishes"}})
	if got := streakBonus(none); got != float64(0) {
		t.Errorf("quest without bonus streak_bonus = %v, want 0", got)
	}
	lib := toLibraryQuest(db.QuestLibrary{ID: uuid.New(), Name: "Clean the garage", Points: 25, RepeatDays: 30, StreakBonus: 10})
	if got := streakBonus(lib); got != float64(10) {
		t.Errorf("library streak_bonus = %v, want 10", got)
	}
}

// TestQuestAgeRangeJSON pins the age-range keys of quest JSON: min_age and
// max_age are null for no limit and the number otherwise, ineligible_member_ids
// is [] when nobody is left out, and no birthday or age key is ever present.
func TestQuestAgeRangeJSON(t *testing.T) {
	t.Parallel()

	decode := func(v any) map[string]any {
		t.Helper()
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal failed: %v", err)
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("json.Unmarshal failed: %v", err)
		}
		return raw
	}

	left := uuid.MustParse("018f25b3-8b77-7000-8000-0000000000aa")
	ranged := decode(toQuest(app.QuestView{
		HouseholdQuest: db.HouseholdQuest{
			ID: uuid.New(), Name: "Mow the lawn",
			MinAge: pgtype.Int4{Int32: 10, Valid: true}, MaxAge: pgtype.Int4{Int32: 14, Valid: true},
		},
		IneligibleMemberIDs: []uuid.UUID{left},
	}))
	if ranged["min_age"] != float64(10) || ranged["max_age"] != float64(14) {
		t.Errorf("ranged min_age/max_age = %v/%v, want 10/14", ranged["min_age"], ranged["max_age"])
	}
	if got, ok := ranged["ineligible_member_ids"].([]any); !ok || len(got) != 1 || got[0] != left.String() {
		t.Errorf("ineligible_member_ids = %v, want [%s]", ranged["ineligible_member_ids"], left)
	}

	open := decode(toQuest(app.QuestView{HouseholdQuest: db.HouseholdQuest{
		ID: uuid.New(), Name: "Read a chapter", MinAge: pgtype.Int4{Int32: 6, Valid: true},
	}}))
	for _, key := range []string{"min_age", "max_age", "ineligible_member_ids"} {
		if _, ok := open[key]; !ok {
			t.Errorf("%s missing from quest JSON", key)
		}
	}
	if open["min_age"] != float64(6) || open["max_age"] != nil {
		t.Errorf("one-sided min_age/max_age = %v/%v, want 6/null", open["min_age"], open["max_age"])
	}
	if got, ok := open["ineligible_member_ids"].([]any); !ok || len(got) != 0 {
		t.Errorf("ineligible_member_ids = %#v, want []", open["ineligible_member_ids"])
	}
	for _, raw := range []map[string]any{ranged, open} {
		for key := range raw {
			if strings.Contains(key, "birthday") || key == "age" {
				t.Errorf("quest JSON carries %q", key)
			}
		}
	}
}

// TestQuestClaimExpiresAtJSON pins claim_expires_at in quest JSON: null for an
// unheld quest, the RFC3339 UTC instant for a held one.
func TestQuestClaimExpiresAtJSON(t *testing.T) {
	t.Parallel()

	decode := func(v any) map[string]any {
		t.Helper()
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal failed: %v", err)
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("json.Unmarshal failed: %v", err)
		}
		return raw
	}

	unheld := decode(toQuest(app.QuestView{
		HouseholdQuest: db.HouseholdQuest{
			ID:   uuid.New(),
			Name: "Unheld Quest",
		},
	}))
	val, ok := unheld["claim_expires_at"]
	if !ok {
		t.Errorf("claim_expires_at key missing from unheld quest JSON")
	} else if val != nil {
		t.Errorf("unheld claim_expires_at = %v, want null", val)
	}

	instant := time.Date(2026, 11, 20, 5, 0, 0, 0, time.UTC)
	heldMember := uuid.New()
	held := decode(toQuest(app.QuestView{
		HouseholdQuest: db.HouseholdQuest{
			ID:             uuid.New(),
			Name:           "Held Quest",
			ClaimedBy:      &heldMember,
			ClaimedAt:      pgtype.Timestamptz{Time: instant.Add(-1 * time.Hour), Valid: true},
			ClaimExpiresAt: pgtype.Timestamptz{Time: instant, Valid: true},
		},
	}))
	val, ok = held["claim_expires_at"]
	if !ok {
		t.Errorf("claim_expires_at key missing from held quest JSON")
	} else if got, ok := val.(string); !ok || got != instant.Format(time.RFC3339) {
		t.Errorf("held claim_expires_at = %v, want %s", val, instant.Format(time.RFC3339))
	}
}

// TestQuestScheduleJSON pins the schedule wire shape: the ids, the quest and
// member names, start_date as YYYY-MM-DD and created_at in UTC, and no
// household_id or other model field.
func TestQuestScheduleJSON(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 11, 19, 3, 0, 0, 0, time.FixedZone("CST", -6*3600))
	s := app.QuestSchedule{
		ID:          uuid.MustParse("018f25b3-8b77-7000-8000-0000000000a1"),
		HouseholdID: uuid.MustParse("018f25b3-8b77-7000-8000-0000000000a2"),
		QuestID:     uuid.MustParse("018f25b3-8b77-7000-8000-0000000000a3"),
		QuestName:   "Feed the cat",
		MemberID:    uuid.MustParse("018f25b3-8b77-7000-8000-0000000000a4"),
		MemberName:  "Sam",
		StartDate:   "2026-11-21",
		CreatedAt:   created,
	}
	data, err := json.Marshal(toQuestSchedule(s))
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	want := map[string]any{
		"id":          s.ID.String(),
		"quest_id":    s.QuestID.String(),
		"quest_name":  "Feed the cat",
		"member_id":   s.MemberID.String(),
		"member_name": "Sam",
		"start_date":  "2026-11-21",
		"created_at":  "2026-11-19T09:00:00Z",
	}
	if !reflect.DeepEqual(raw, want) {
		t.Errorf("schedule JSON = %v, want %v", raw, want)
	}
}
