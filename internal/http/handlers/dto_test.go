package handlers

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
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

