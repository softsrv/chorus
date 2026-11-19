//go:build integration

package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
	internalhttp "github.com/softsrv/chorus/internal/http"
)

func TestAPI_FamilyMembersAndAdminPin(t *testing.T) {
	s := newTestServer(t)
	u := s.signUp("famadmin")

	// Fresh auto household
	var h struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households", u.AccessToken,
		map[string]string{"name": "Admin Household", "timezone": "America/New_York"}, &h)

	// CLM-6: GET family-members returns {"items":[]} (non-nil empty array)
	var listResp struct {
		Items []struct {
			ID     uuid.UUID `json:"id"`
			Name   string    `json:"name"`
			HasPIN bool      `json:"has_pin"`
			Points int32     `json:"points"`
		} `json:"items"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/households/"+h.ID.String()+"/family-members", u.AccessToken, nil, &listResp)
	if listResp.Items == nil || len(listResp.Items) != 0 {
		t.Fatalf("expected empty non-nil slice, got %+v", listResp.Items)
	}

	// GET admin-pin status: initially false
	var adminPinStatus struct {
		Set bool `json:"set"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/households/"+h.ID.String()+"/admin-pin", u.AccessToken, nil, &adminPinStatus)
	if adminPinStatus.Set {
		t.Errorf("admin-pin status initially set = true, want false")
	}

	// Create member without PIN
	var m1 struct {
		ID     uuid.UUID `json:"id"`
		Name   string    `json:"name"`
		HasPIN bool      `json:"has_pin"`
		Points int32     `json:"points"`
	}
	s.expect(http.StatusCreated, "POST", "/api/v1/households/"+h.ID.String()+"/family-members", u.AccessToken,
		map[string]string{"name": "Sam"}, &m1)
	if m1.Name != "Sam" || m1.HasPIN || m1.Points != 0 {
		t.Errorf("created m1: got %+v", m1)
	}

	// Create member with PIN
	var m2 struct {
		ID     uuid.UUID `json:"id"`
		Name   string    `json:"name"`
		HasPIN bool      `json:"has_pin"`
		Points int32     `json:"points"`
	}
	s.expect(http.StatusCreated, "POST", "/api/v1/households/"+h.ID.String()+"/family-members", u.AccessToken,
		map[string]string{"name": "Alex", "pin": "1234"}, &m2)
	if m2.Name != "Alex" || !m2.HasPIN || m2.Points != 0 {
		t.Errorf("created m2: got %+v", m2)
	}

	// List shows both
	s.expect(http.StatusOK, "GET", "/api/v1/households/"+h.ID.String()+"/family-members", u.AccessToken, nil, &listResp)
	if len(listResp.Items) != 2 {
		t.Fatalf("list count: got %d, want 2", len(listResp.Items))
	}

	// Rename member
	var m1Renamed struct {
		ID   uuid.UUID `json:"id"`
		Name string    `json:"name"`
	}
	s.expect(http.StatusOK, "PUT", "/api/v1/households/"+h.ID.String()+"/family-members/"+m1.ID.String(), u.AccessToken,
		map[string]string{"name": "Samantha"}, &m1Renamed)
	if m1Renamed.Name != "Samantha" {
		t.Errorf("renamed m1: got %q, want Samantha", m1Renamed.Name)
	}

	// CLM-7: Verify route
	// Verify m2 with correct PIN returns 200 with member JSON
	var vResp struct {
		ID     uuid.UUID `json:"id"`
		Name   string    `json:"name"`
		HasPIN bool      `json:"has_pin"`
		Points int32     `json:"points"`
	}
	req, _ := http.NewRequest("POST", s.srv.URL+"/api/v1/households/"+h.ID.String()+"/family-members/"+m2.ID.String()+"/verify", strings.NewReader(`{"pin":"1234"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+u.AccessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("verify request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("verify correct pin status %d, want 200", resp.StatusCode)
	}
	if cookie := resp.Header.Get("Set-Cookie"); cookie != "" {
		t.Errorf("verify response set cookie: %q", cookie)
	}
	if err := json.NewDecoder(resp.Body).Decode(&vResp); err != nil {
		t.Fatalf("decode verify: %v", err)
	}
	if vResp.ID != m2.ID || vResp.Name != "Alex" || !vResp.HasPIN {
		t.Errorf("verify resp: got %+v", vResp)
	}

	// Verify m1 (no PIN) with empty PIN succeeds
	s.expect(http.StatusOK, "POST", "/api/v1/households/"+h.ID.String()+"/family-members/"+m1.ID.String()+"/verify", u.AccessToken,
		map[string]string{"pin": ""}, &vResp)
	if vResp.ID != m1.ID || vResp.Name != "Samantha" || vResp.HasPIN {
		t.Errorf("verify m1 resp: got %+v", vResp)
	}

	// Verify m2 with wrong PIN gives 403 wrong_pin
	var errResp apiError
	s.expect(http.StatusForbidden, "POST", "/api/v1/households/"+h.ID.String()+"/family-members/"+m2.ID.String()+"/verify", u.AccessToken,
		map[string]string{"pin": "9999"}, &errResp)
	if errResp.Error.Code != "wrong_pin" {
		t.Errorf("wrong pin code: got %q, want wrong_pin", errResp.Error.Code)
	}

	// Session token still works afterwards
	s.expect(http.StatusOK, "GET", "/api/v1/me", u.AccessToken, nil, nil)

	// Points history: with no admin PIN set, the household admin reads the
	// PIN'd member's history with no PIN header at all; the member PIN still
	// reads it too.
	awardID := uuid.New()
	if _, err := db.New(s.pool).InsertPointsAward(context.Background(), db.InsertPointsAwardParams{
		ID: awardID, HouseholdID: h.ID, MemberID: m2.ID, Kind: "quest", Reason: "Feed the cat", Points: 5,
		EarnedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("insert points award: %v", err)
	}
	m2HistPath := "/api/v1/households/" + h.ID.String() + "/family-members/" + m2.ID.String() + "/points-history"
	type histItems struct {
		Items []struct {
			ID     uuid.UUID `json:"id"`
			Reason string    `json:"reason"`
			Points int       `json:"points"`
		} `json:"items"`
	}
	var adminNoPinHist histItems
	s.expect(http.StatusOK, "GET", m2HistPath, u.AccessToken, nil, &adminNoPinHist)
	if len(adminNoPinHist.Items) != 1 || adminNoPinHist.Items[0].ID != awardID || adminNoPinHist.Items[0].Reason != "Feed the cat" {
		t.Errorf("admin history with no admin PIN set: got %+v, want award %v", adminNoPinHist.Items, awardID)
	}
	var memberPinHist histItems
	s.expectWithHeaders(http.StatusOK, "GET", m2HistPath, u.AccessToken, map[string]string{"X-Member-PIN": "1234"}, nil, &memberPinHist)
	if len(memberPinHist.Items) != 1 || memberPinHist.Items[0].ID != awardID {
		t.Errorf("member PIN history: got %+v, want award %v", memberPinHist.Items, awardID)
	}

	// Set invalid PINs: 123, 12345, abcd -> 422 field pin
	for _, badPin := range []string{"123", "12345", "abcd"} {
		var vErr apiError
		s.expect(http.StatusUnprocessableEntity, "PUT", "/api/v1/households/"+h.ID.String()+"/family-members/"+m1.ID.String()+"/pin", u.AccessToken,
			map[string]string{"pin": badPin}, &vErr)
		if vErr.Error.Fields["pin"] == "" {
			t.Errorf("bad pin %q: expected validation error on pin, got %+v", badPin, vErr)
		}
	}

	// Check response JSON bodies never leak "pin" or "pin_hash"
	rawListResp, _ := http.NewRequest("GET", s.srv.URL+"/api/v1/households/"+h.ID.String()+"/family-members", nil)
	rawListResp.Header.Set("Authorization", "Bearer "+u.AccessToken)
	rResp, err := http.DefaultClient.Do(rawListResp)
	if err != nil {
		t.Fatalf("get list: %v", err)
	}
	defer rResp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(rResp.Body)
	rawBody := buf.String()
	if strings.Contains(rawBody, "pin_hash") || strings.Contains(rawBody, "1234") {
		t.Errorf("family members response leaked pin/pin_hash: %s", rawBody)
	}

	// Direct database check: stored hashes are bcrypt and != raw PIN
	var storedMemberHash, storedAdminHash string
	if err := s.pool.QueryRow(context.Background(), "SELECT pin_hash FROM family_members WHERE id = $1", m2.ID).Scan(&storedMemberHash); err != nil {
		t.Fatalf("query member pin hash: %v", err)
	}
	if storedMemberHash == "1234" || !strings.HasPrefix(storedMemberHash, "$2a$") {
		t.Errorf("stored member hash is not bcrypt: %q", storedMemberHash)
	}

	// CLM-10: Admin PIN lifecycle and management write gates
	// Set admin PIN "4321"
	s.expect(http.StatusOK, "PUT", "/api/v1/households/"+h.ID.String()+"/admin-pin", u.AccessToken,
		map[string]string{"pin": "4321"}, nil)

	// Admin pin status is now true
	s.expect(http.StatusOK, "GET", "/api/v1/households/"+h.ID.String()+"/admin-pin", u.AccessToken, nil, &adminPinStatus)
	if !adminPinStatus.Set {
		t.Errorf("admin-pin status want true, got false")
	}

	// Verify admin PIN
	s.expect(http.StatusOK, "POST", "/api/v1/households/"+h.ID.String()+"/admin-pin/verify", u.AccessToken,
		map[string]string{"pin": "4321"}, nil)
	s.expect(http.StatusForbidden, "POST", "/api/v1/households/"+h.ID.String()+"/admin-pin/verify", u.AccessToken,
		map[string]string{"pin": "0000"}, nil)

	// Check stored admin PIN hash
	if err := s.pool.QueryRow(context.Background(), "SELECT pin_hash FROM household_admin_pins WHERE household_id = $1", h.ID).Scan(&storedAdminHash); err != nil {
		t.Fatalf("query admin pin hash: %v", err)
	}
	if storedAdminHash == "4321" || !strings.HasPrefix(storedAdminHash, "$2a$") {
		t.Errorf("stored admin pin hash is not bcrypt: %q", storedAdminHash)
	}

	// With admin PIN set:
	// Points history with no PIN header, or a wrong X-Admin-PIN, is refused
	// with 403 wrong_pin and no items; the right X-Admin-PIN reads it.
	for name, hdr := range map[string]map[string]string{
		"missing admin PIN": nil,
		"wrong admin PIN":   {"X-Admin-PIN": "0000"},
	} {
		var histErr struct {
			apiError
			Items json.RawMessage `json:"items"`
		}
		s.expectWithHeaders(http.StatusForbidden, "GET", m2HistPath, u.AccessToken, hdr, nil, &histErr)
		if histErr.Error.Code != "wrong_pin" {
			t.Errorf("history with %s: code %q, want wrong_pin", name, histErr.Error.Code)
		}
		if histErr.Items != nil {
			t.Errorf("history with %s returned items: %s", name, histErr.Items)
		}
	}
	var adminPinHist histItems
	s.expectWithHeaders(http.StatusOK, "GET", m2HistPath, u.AccessToken, map[string]string{"X-Admin-PIN": "4321"}, nil, &adminPinHist)
	if len(adminPinHist.Items) != 1 || adminPinHist.Items[0].ID != awardID {
		t.Errorf("history with right admin PIN: got %+v, want award %v", adminPinHist.Items, awardID)
	}

	// Points adjustments with the admin PIN set: a missing or wrong
	// X-Admin-PIN is refused and changes nothing; with the right one, +10
	// lands as the member's new total and as the top admin history row.
	m1Path := "/api/v1/households/" + h.ID.String() + "/family-members/" + m1.ID.String()
	adjPath := m1Path + "/points-adjustments"
	s.expect(http.StatusForbidden, "POST", adjPath, u.AccessToken,
		map[string]any{"points": 10, "reason": "helped grandma"}, &errResp)
	if errResp.Error.Code != "admin_pin_required" {
		t.Errorf("adjust without header: got %q, want admin_pin_required", errResp.Error.Code)
	}
	s.expectWithHeaders(http.StatusForbidden, "POST", adjPath, u.AccessToken,
		map[string]string{"X-Admin-PIN": "0000"}, map[string]any{"points": 10, "reason": "helped grandma"}, &errResp)
	if errResp.Error.Code != "wrong_pin" {
		t.Errorf("adjust with wrong header: got %q, want wrong_pin", errResp.Error.Code)
	}
	adminHdr := map[string]string{"X-Admin-PIN": "4321"}
	var adjusted struct {
		ID     uuid.UUID `json:"id"`
		Points int32     `json:"points"`
	}
	s.expectWithHeaders(http.StatusOK, "POST", adjPath, u.AccessToken, adminHdr,
		map[string]any{"points": 10, "reason": "helped grandma"}, &adjusted)
	if adjusted.ID != m1.ID || adjusted.Points != 10 {
		t.Errorf("adjust +10: got %+v, want member %v at 10 points", adjusted, m1.ID)
	}
	s.expectWithHeaders(http.StatusOK, "POST", adjPath, u.AccessToken, adminHdr,
		map[string]any{"points": -4, "reason": ""}, &adjusted)
	if adjusted.Points != 6 {
		t.Errorf("adjust -4 with blank reason: points %d, want 6", adjusted.Points)
	}
	// Refusals: removing more than the total, a non-whole, zero or oversized
	// amount, and a reason that is too long, each a 422 on its field.
	var adjErr apiError
	s.expectWithHeaders(http.StatusUnprocessableEntity, "POST", adjPath, u.AccessToken, adminHdr,
		map[string]any{"points": -7}, &adjErr)
	if got := adjErr.Error.Fields["points"]; got != "Samantha only has 6 points — you can't remove 7." {
		t.Errorf("over-removal points message = %q", got)
	}
	for name, body := range map[string]map[string]any{
		"non-whole": {"points": 1.5},
		"string":    {"points": "5"},
		"zero":      {"points": 0},
		"oversized": {"points": 1_000_001},
		"absent":    {"reason": "x"},
		"long":      {"points": 1, "reason": strings.Repeat("r", 201)},
	} {
		field := "points"
		if name == "long" {
			field = "reason"
		}
		var vErr apiError
		s.expectWithHeaders(http.StatusUnprocessableEntity, "POST", adjPath, u.AccessToken, adminHdr, body, &vErr)
		if vErr.Error.Fields[field] == "" {
			t.Errorf("adjust %s: want a field error on %s, got %+v", name, field, vErr)
		}
	}
	var m1Hist struct {
		Items []struct {
			Kind   string `json:"kind"`
			Reason string `json:"reason"`
			Points int32  `json:"points"`
		} `json:"items"`
	}
	s.expectWithHeaders(http.StatusOK, "GET", m1Path+"/points-history", u.AccessToken, adminHdr, nil, &m1Hist)
	if len(m1Hist.Items) != 2 ||
		m1Hist.Items[0].Kind != "admin" || m1Hist.Items[0].Points != -4 || m1Hist.Items[0].Reason != "Adjusted by admin" ||
		m1Hist.Items[1].Kind != "admin" || m1Hist.Items[1].Points != 10 || m1Hist.Items[1].Reason != "helped grandma" {
		t.Errorf("history after adjustments: got %+v", m1Hist.Items)
	}
	var m1After struct {
		Points int32 `json:"points"`
	}
	s.expect(http.StatusOK, "GET", m1Path, u.AccessToken, nil, &m1After)
	if m1After.Points != 6 || (len(m1Hist.Items) == 2 && m1After.Points != m1Hist.Items[0].Points+m1Hist.Items[1].Points) {
		t.Errorf("member total %d, want 6 and the sum of its history", m1After.Points)
	}

	// Create without header -> 403 admin_pin_required
	s.expect(http.StatusForbidden, "POST", "/api/v1/households/"+h.ID.String()+"/family-members", u.AccessToken,
		map[string]string{"name": "GatedKid"}, &errResp)
	if errResp.Error.Code != "admin_pin_required" {
		t.Errorf("create without header: got %q, want admin_pin_required", errResp.Error.Code)
	}

	// Create with wrong header -> 403 wrong_pin
	s.expectWithHeaders(http.StatusForbidden, "POST", "/api/v1/households/"+h.ID.String()+"/family-members", u.AccessToken,
		map[string]string{"X-Admin-PIN": "9999"}, map[string]string{"name": "GatedKid"}, &errResp)
	if errResp.Error.Code != "wrong_pin" {
		t.Errorf("create with wrong header: got %q, want wrong_pin", errResp.Error.Code)
	}

	// Create with right header -> 201 Created
	var gatedKid struct{ ID uuid.UUID }
	s.expectWithHeaders(http.StatusCreated, "POST", "/api/v1/households/"+h.ID.String()+"/family-members", u.AccessToken,
		map[string]string{"X-Admin-PIN": "4321"}, map[string]string{"name": "GatedKid"}, &gatedKid)

	// Rename without header -> 403 admin_pin_required
	s.expect(http.StatusForbidden, "PUT", "/api/v1/households/"+h.ID.String()+"/family-members/"+gatedKid.ID.String(), u.AccessToken,
		map[string]string{"name": "RenamedGated"}, &errResp)

	// Rename with wrong header -> 403 wrong_pin
	s.expectWithHeaders(http.StatusForbidden, "PUT", "/api/v1/households/"+h.ID.String()+"/family-members/"+gatedKid.ID.String(), u.AccessToken,
		map[string]string{"X-Admin-PIN": "0000"}, map[string]string{"name": "RenamedGated"}, &errResp)

	// Rename with right header -> 200 OK
	s.expectWithHeaders(http.StatusOK, "PUT", "/api/v1/households/"+h.ID.String()+"/family-members/"+gatedKid.ID.String(), u.AccessToken,
		map[string]string{"X-Admin-PIN": "4321"}, map[string]string{"name": "RenamedGated"}, nil)

	// Set member PIN without header -> 403 admin_pin_required
	s.expect(http.StatusForbidden, "PUT", "/api/v1/households/"+h.ID.String()+"/family-members/"+gatedKid.ID.String()+"/pin", u.AccessToken,
		map[string]string{"pin": "5555"}, &errResp)

	// Set member PIN with right header -> 200 OK
	s.expectWithHeaders(http.StatusOK, "PUT", "/api/v1/households/"+h.ID.String()+"/family-members/"+gatedKid.ID.String()+"/pin", u.AccessToken,
		map[string]string{"X-Admin-PIN": "4321"}, map[string]string{"pin": "5555"}, nil)

	// Clear admin PIN with right current admin PIN -> 204
	s.expectWithHeaders(http.StatusNoContent, "DELETE", "/api/v1/households/"+h.ID.String()+"/admin-pin", u.AccessToken,
		map[string]string{"X-Admin-PIN": "4321"}, nil, nil)

	// Admin pin status is false again
	s.expect(http.StatusOK, "GET", "/api/v1/households/"+h.ID.String()+"/admin-pin", u.AccessToken, nil, &adminPinStatus)
	if adminPinStatus.Set {
		t.Errorf("admin-pin status want false after clear, got true")
	}

	// Clearing reopened gate: Delete family member without X-Admin-PIN header succeeds
	s.expect(http.StatusNoContent, "DELETE", "/api/v1/households/"+h.ID.String()+"/family-members/"+gatedKid.ID.String(), u.AccessToken, nil, nil)
}

func TestAPI_PINRateLimit(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	q := db.New(pool)
	mail := &mailbox{text: map[string]string{}}
	authSvc := app.NewAuthService(q, pool, mail, app.AuthServiceConfig{
		JWTSecret: secret, AccessExpiry: 15 * time.Minute, RefreshExpiry: 24 * time.Hour,
		BCryptCost: 10, PasswordMinLen: 8, AppBaseURL: "http://chorus.test", AppName: "Chorus",
	})
	t.Cleanup(authSvc.Shutdown)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Small PIN limit: 1 setup call (member create, also PIN-limited) + 3 wrong guesses.
	const setupCalls, wrongGuesses = 1, 3
	handler := internalhttp.NewRouter(ctx, internalhttp.RouterConfig{
		Queries: q, Pool: pool, AuthSvc: authSvc,
		UserSvc:      app.NewUserService(q),
		HouseholdSvc: app.NewHouseholdService(q, pool),
		FamilySvc:    app.NewFamilyService(q, pool, 10),
		JWTSecret:    secret,
		PINRateLimit: setupCalls + wrongGuesses,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ts := &testServer{t: t, srv: srv, mail: mail, pool: pool}

	u := ts.signUp("rluser")
	var h struct{ ID uuid.UUID }
	ts.expect(http.StatusCreated, "POST", "/api/v1/households", u.AccessToken,
		map[string]string{"name": "RL Home", "timezone": "UTC"}, &h)

	var m struct{ ID uuid.UUID }
	ts.expect(http.StatusCreated, "POST", "/api/v1/households/"+h.ID.String()+"/family-members", u.AccessToken,
		map[string]string{"name": "Kid", "pin": "1234"}, &m)

	// Wrong guesses within the limit are evaluated: 403 wrong_pin.
	for i := 0; i < wrongGuesses; i++ {
		ts.expect(http.StatusForbidden, "POST", "/api/v1/households/"+h.ID.String()+"/family-members/"+m.ID.String()+"/verify", u.AccessToken,
			map[string]string{"pin": "0000"}, nil)
	}

	// Past the limit: 429 Too Many Requests, even with the correct PIN.
	var rlErr apiError
	ts.expect(http.StatusTooManyRequests, "POST", "/api/v1/households/"+h.ID.String()+"/family-members/"+m.ID.String()+"/verify", u.AccessToken,
		map[string]string{"pin": "1234"}, &rlErr)
	if rlErr.Error.Code != "rate_limited" {
		t.Errorf("expected rate_limited, got %q", rlErr.Error.Code)
	}
}

func TestAPI_QuestPINRateLimit(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	q := db.New(pool)
	mail := &mailbox{text: map[string]string{}}
	authSvc := app.NewAuthService(q, pool, mail, app.AuthServiceConfig{
		JWTSecret: secret, AccessExpiry: 15 * time.Minute, RefreshExpiry: 24 * time.Hour,
		BCryptCost: 10, PasswordMinLen: 8, AppBaseURL: "http://chorus.test", AppName: "Chorus",
	})
	t.Cleanup(authSvc.Shutdown)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Small PIN limit: 2 setup calls (member create + quest create, both PIN-limited) + 3 wrong guesses.
	const setupCalls, wrongGuesses = 2, 3
	handler := internalhttp.NewRouter(ctx, internalhttp.RouterConfig{
		Queries: q, Pool: pool, AuthSvc: authSvc,
		UserSvc:      app.NewUserService(q),
		HouseholdSvc: app.NewHouseholdService(q, pool),
		FamilySvc:    app.NewFamilyService(q, pool, 10),
		QuestSvc:     app.NewQuestService(q, pool),
		JWTSecret:    secret,
		PINRateLimit: setupCalls + wrongGuesses,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ts := &testServer{t: t, srv: srv, mail: mail, pool: pool}

	u := ts.signUp("questrluser")
	var h struct{ ID uuid.UUID }
	ts.expect(http.StatusCreated, "POST", "/api/v1/households", u.AccessToken,
		map[string]string{"name": "Quest RL Home", "timezone": "UTC"}, &h)

	var m struct{ ID uuid.UUID }
	ts.expect(http.StatusCreated, "POST", "/api/v1/households/"+h.ID.String()+"/family-members", u.AccessToken,
		map[string]string{"name": "Kid", "pin": "1234"}, &m)

	var quest struct{ ID uuid.UUID }
	ts.expect(http.StatusCreated, "POST", "/api/v1/households/"+h.ID.String()+"/quests", u.AccessToken,
		map[string]string{"name": "Mow Lawn"}, &quest)

	ap := "/api/v1/households/" + h.ID.String() + "/quests/" + quest.ID.String() + "/accept"

	// Wrong guesses within the limit are evaluated: 403 wrong_pin.
	for i := 0; i < wrongGuesses; i++ {
		var wrongPin apiError
		ts.expect(http.StatusForbidden, "POST", ap, u.AccessToken,
			map[string]any{"member_id": m.ID, "pin": "0000"}, &wrongPin)
		if wrongPin.Error.Code != "wrong_pin" {
			t.Errorf("expected wrong_pin, got %q", wrongPin.Error.Code)
		}
	}

	// Past the limit: 429 Too Many Requests, even with the correct PIN.
	var rlErr apiError
	ts.expect(http.StatusTooManyRequests, "POST", ap, u.AccessToken,
		map[string]any{"member_id": m.ID, "pin": "1234"}, &rlErr)
	if rlErr.Error.Code != "rate_limited" {
		t.Errorf("expected rate_limited, got %q", rlErr.Error.Code)
	}
}

// TestAPI_PointsHistoryHasItsOwnPINBudget proves the points-history read is
// rate limited on a budget of its own for correct reads (reading it never
// spends the PIN budget member actions need, and it is still limited
// itself), while every wrong PIN on it is charged to the shared PIN budget,
// so it gives no second budget of guesses.
func TestAPI_PointsHistoryHasItsOwnPINBudget(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	q := db.New(pool)
	mail := &mailbox{text: map[string]string{}}
	authSvc := app.NewAuthService(q, pool, mail, app.AuthServiceConfig{
		JWTSecret: secret, AccessExpiry: 15 * time.Minute, RefreshExpiry: 24 * time.Hour,
		BCryptCost: 10, PasswordMinLen: 8, AppBaseURL: "http://chorus.test", AppName: "Chorus",
	})
	t.Cleanup(authSvc.Shutdown)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// PIN budget: member create + quest create + admin PIN set + one accept +
	// one wrong history PIN. History budget: four correct reads + the wrong one.
	const pinLimit = 5
	handler := internalhttp.NewRouter(ctx, internalhttp.RouterConfig{
		Queries: q, Pool: pool, AuthSvc: authSvc,
		UserSvc:      app.NewUserService(q),
		HouseholdSvc: app.NewHouseholdService(q, pool),
		FamilySvc:    app.NewFamilyService(q, pool, 10),
		QuestSvc:     app.NewQuestService(q, pool),
		JWTSecret:    secret,
		PINRateLimit: pinLimit,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ts := &testServer{t: t, srv: srv, mail: mail, pool: pool}

	u := ts.signUp("histrluser")
	var h struct{ ID uuid.UUID }
	ts.expect(http.StatusCreated, "POST", "/api/v1/households", u.AccessToken,
		map[string]string{"name": "History RL Home", "timezone": "UTC"}, &h)
	hp := "/api/v1/households/" + h.ID.String()

	var m struct{ ID uuid.UUID }
	ts.expect(http.StatusCreated, "POST", hp+"/family-members", u.AccessToken,
		map[string]string{"name": "Kid", "pin": "1234"}, &m)
	var quest struct{ ID uuid.UUID }
	ts.expect(http.StatusCreated, "POST", hp+"/quests", u.AccessToken,
		map[string]string{"name": "Mow Lawn"}, &quest)
	// An admin PIN is set so the caller (the household admin) cannot read
	// the history without one: with none set, the admin needs no PIN at all.
	ts.expect(http.StatusOK, "PUT", hp+"/admin-pin", u.AccessToken,
		map[string]string{"pin": "4321"}, nil)

	histPath := hp + "/family-members/" + m.ID.String() + "/points-history"
	pinHdr := map[string]string{"X-Member-PIN": "1234"}
	for i := 0; i < pinLimit-1; i++ {
		ts.expectWithHeaders(http.StatusOK, "GET", histPath, u.AccessToken, pinHdr, nil, nil)
	}

	// The history reads spent none of the action budget: accept still runs.
	acceptPath := hp + "/quests/" + quest.ID.String() + "/accept"
	ts.expect(http.StatusOK, "POST", acceptPath, u.AccessToken,
		map[string]any{"member_id": m.ID, "pin": "1234"}, nil)

	// A wrong PIN on the history read is refused and charged to the shared
	// PIN budget, which it now fills: the next member action is limited.
	var wrongPin apiError
	ts.expectWithHeaders(http.StatusForbidden, "GET", histPath, u.AccessToken,
		map[string]string{"X-Member-PIN": "0000"}, nil, &wrongPin)
	if wrongPin.Error.Code != "wrong_pin" {
		t.Errorf("expected wrong_pin, got %q", wrongPin.Error.Code)
	}
	var actionRL apiError
	ts.expect(http.StatusTooManyRequests, "POST", acceptPath, u.AccessToken,
		map[string]any{"member_id": m.ID, "pin": "1234"}, &actionRL)
	if actionRL.Error.Code != "rate_limited" {
		t.Errorf("accept after wrong history PIN: expected rate_limited, got %q", actionRL.Error.Code)
	}

	// The history read is still limited on its own budget.
	var rlErr apiError
	ts.expectWithHeaders(http.StatusTooManyRequests, "GET", histPath, u.AccessToken, pinHdr, nil, &rlErr)
	if rlErr.Error.Code != "rate_limited" {
		t.Errorf("expected rate_limited, got %q", rlErr.Error.Code)
	}
}

func TestAPI_RemovedInviteAndMemberRoutes404(t *testing.T) {
	s := newTestServer(t)
	u := s.signUp("removedroutes")

	var h struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households", u.AccessToken,
		map[string]string{"name": "Home", "timezone": "UTC"}, &h)
	hp := "/api/v1/households/" + h.ID.String()

	var e apiError
	// POST /households/{id}/invites -> 404
	s.expect(http.StatusNotFound, "POST", hp+"/invites", u.AccessToken, nil, &e)
	if e.Error.Code != "not_found" {
		t.Errorf("POST /invites: code %q, want not_found", e.Error.Code)
	}

	// POST /invites/accept -> 404
	s.expect(http.StatusNotFound, "POST", "/api/v1/invites/accept", u.AccessToken, map[string]string{"code": "ABCDEFGH"}, &e)
	if e.Error.Code != "not_found" {
		t.Errorf("POST /invites/accept: code %q, want not_found", e.Error.Code)
	}

	// GET /households/{id}/members -> 404
	s.expect(http.StatusNotFound, "GET", hp+"/members", u.AccessToken, nil, &e)
	if e.Error.Code != "not_found" {
		t.Errorf("GET /members: code %q, want not_found", e.Error.Code)
	}

	// PUT /households/{id}/members/{uid} -> 404
	s.expect(http.StatusNotFound, "PUT", hp+"/members/"+u.User.ID.String(), u.AccessToken, map[string]string{"role": "member"}, &e)
	if e.Error.Code != "not_found" {
		t.Errorf("PUT /members/{uid}: code %q, want not_found", e.Error.Code)
	}

	// DELETE /households/{id}/members/{uid} -> 404
	s.expect(http.StatusNotFound, "DELETE", hp+"/members/"+u.User.ID.String(), u.AccessToken, nil, &e)
	if e.Error.Code != "not_found" {
		t.Errorf("DELETE /members/{uid}: code %q, want not_found", e.Error.Code)
	}
}

// CLM-2: POST share for an out-of-range target answers 409 age_restricted
// with "This quest is outside that member's age range." and leaves the quest,
// its holder and its participants unchanged.
func TestAPI_QuestShare_AgeRange(t *testing.T) {
	s := newTestServer(t)
	parent := s.signUp("shareparent")
	ctx := context.Background()

	var h struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households", parent.AccessToken,
		map[string]string{"name": "Share Ages", "timezone": "America/Chicago"}, &h)
	hp := "/api/v1/households/" + h.ID.String()

	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	newMemberWithPin := func(name, pin string, years int) uuid.UUID {
		t.Helper()
		var m struct{ ID uuid.UUID }
		s.expect(http.StatusCreated, "POST", hp+"/family-members", parent.AccessToken,
			map[string]string{"name": name, "pin": pin}, &m)
		if years >= 0 {
			bd := today.AddDate(-years, 0, -30).Format("2006-01-02")
			if _, err := s.pool.Exec(ctx, `UPDATE family_members SET birthday = $1::date WHERE id = $2`, bd, m.ID); err != nil {
				t.Fatalf("set birthday: %v", err)
			}
		}
		return m.ID
	}

	eligible := newMemberWithPin("Eligible", "1111", 12)
	outOfRange := newMemberWithPin("TooYoung", "2222", 8)

	type shareQuest struct {
		ID                  uuid.UUID   `json:"id"`
		Name                string      `json:"name"`
		Shareable           bool        `json:"shareable"`
		Shared              bool        `json:"shared"`
		ClaimedBy           *uuid.UUID  `json:"claimed_by"`
		Participants        []uuid.UUID `json:"participants"`
		MinAge              *int32      `json:"min_age"`
		MaxAge              *int32      `json:"max_age"`
		IneligibleMemberIDs []uuid.UUID `json:"ineligible_member_ids"`
	}

	var q shareQuest
	s.expect(http.StatusCreated, "POST", hp+"/quests", parent.AccessToken, map[string]any{
		"name": "Mow lawn", "points": 50, "shareable": true, "teamwork_bonus": 10,
		"min_age": 10, "max_age": 14,
	}, &q)

	qp := hp + "/quests/" + q.ID.String()
	// Eligible member accepts the quest.
	s.expect(http.StatusOK, "POST", qp+"/accept", parent.AccessToken, map[string]any{
		"member_id": eligible, "pin": "1111",
	}, nil)

	// POST share to an out-of-range target returns 409 age_restricted.
	var refused ageErr
	s.expect(http.StatusConflict, "POST", qp+"/share", parent.AccessToken, map[string]any{
		"member_id":        eligible,
		"pin":              "1111",
		"target_member_id": outOfRange,
		"target_pin":       "2222",
	}, &refused)

	if refused.Error.Code != "age_restricted" {
		t.Errorf("share refusal code: got %q, want age_restricted", refused.Error.Code)
	}
	const wantMsg = "This quest is outside that member's age range."
	if refused.Error.Message != wantMsg {
		t.Errorf("share refusal message: got %q, want %q", refused.Error.Message, wantMsg)
	}

	// GET quest confirms the quest, its holder and its participants are unchanged afterwards.
	var got shareQuest
	s.expect(http.StatusOK, "GET", qp, parent.AccessToken, nil, &got)
	if got.Shared {
		t.Errorf("quest after refused share: shared = true, want false")
	}
	if len(got.Participants) != 0 {
		t.Errorf("quest after refused share: participants = %v, want empty", got.Participants)
	}
	if got.ClaimedBy == nil || *got.ClaimedBy != eligible {
		t.Errorf("quest after refused share: claimed_by = %v, want %v", got.ClaimedBy, eligible)
	}
}

// CLM-5: Share refuses with ErrAgeRestricted exactly the set in ineligible_member_ids;
// members not on the quest are never missing from the family-members list because of age,
// and GET family-members returns no birthday or age keys.
func TestAPI_QuestShare_IneligibleConsistency(t *testing.T) {
	s := newTestServer(t)
	parent := s.signUp("ineligibleparent")
	ctx := context.Background()

	var h struct{ ID uuid.UUID }
	s.expect(http.StatusCreated, "POST", "/api/v1/households", parent.AccessToken,
		map[string]string{"name": "Ineligible Consistency", "timezone": "America/Chicago"}, &h)
	hp := "/api/v1/households/" + h.ID.String()

	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	newMember := func(name string, years int) uuid.UUID {
		t.Helper()
		var m struct{ ID uuid.UUID }
		s.expect(http.StatusCreated, "POST", hp+"/family-members", parent.AccessToken,
			map[string]string{"name": name}, &m)
		if years >= 0 {
			bd := today.AddDate(-years, 0, -30).Format("2006-01-02")
			if _, err := s.pool.Exec(ctx, `UPDATE family_members SET birthday = $1::date WHERE id = $2`, bd, m.ID); err != nil {
				t.Fatalf("set birthday: %v", err)
			}
		}
		return m.ID
	}

	m8 := newMember("Eight", 8)
	m10 := newMember("Ten", 10)
	m12 := newMember("Twelve", 12)
	m14 := newMember("Fourteen", 14)
	m15 := newMember("Fifteen", 15)
	mNoBday := newMember("NoBirthday", -1)

	allMembers := []uuid.UUID{m8, m10, m12, m14, m15, mNoBday}

	type shareQuest struct {
		ID                  uuid.UUID   `json:"id"`
		Name                string      `json:"name"`
		Shareable           bool        `json:"shareable"`
		Shared              bool        `json:"shared"`
		ClaimedBy           *uuid.UUID  `json:"claimed_by"`
		Participants        []uuid.UUID `json:"participants"`
		IneligibleMemberIDs []uuid.UUID `json:"ineligible_member_ids"`
	}

	createAndAcceptQuest := func(name string) shareQuest {
		var q shareQuest
		s.expect(http.StatusCreated, "POST", hp+"/quests", parent.AccessToken, map[string]any{
			"name": name, "points": 25, "shareable": true, "teamwork_bonus": 5,
			"min_age": 10, "max_age": 14,
		}, &q)
		s.expect(http.StatusOK, "POST", hp+"/quests/"+q.ID.String()+"/accept", parent.AccessToken, map[string]any{
			"member_id": m12,
		}, nil)
		s.expect(http.StatusOK, "GET", hp+"/quests/"+q.ID.String(), parent.AccessToken, nil, &q)
		return q
	}

	initialQuest := createAndAcceptQuest("Check Ineligible")
	ineligible := initialQuest.IneligibleMemberIDs

	// Out of the members, exactly m8 and m15 should be in ineligible_member_ids.
	wantIneligible := []uuid.UUID{m8, m15}
	if len(ineligible) != len(wantIneligible) {
		t.Fatalf("ineligible_member_ids: got %v, want %v", ineligible, wantIneligible)
	}
	for _, id := range wantIneligible {
		if !slices.Contains(ineligible, id) {
			t.Errorf("ineligible_member_ids missing %v", id)
		}
	}

	// For each other member, compare ineligible_member_ids with POST share outcome.
	// Each success (and each test) on a fresh quest so no already_participant interference.
	otherMembers := []uuid.UUID{m8, m10, m14, m15, mNoBday}
	for _, target := range otherMembers {
		quest := createAndAcceptQuest(fmt.Sprintf("Quest for %s", target))
		sharePath := hp + "/quests/" + quest.ID.String() + "/share"
		shareBody := map[string]any{
			"member_id":        m12,
			"target_member_id": target,
		}

		if slices.Contains(ineligible, target) {
			var errResp ageErr
			s.expect(http.StatusConflict, "POST", sharePath, parent.AccessToken, shareBody, &errResp)
			if errResp.Error.Code != "age_restricted" {
				t.Errorf("member %v: share refusal code got %q, want age_restricted", target, errResp.Error.Code)
			}
			const wantMsg = "This quest is outside that member's age range."
			if errResp.Error.Message != wantMsg {
				t.Errorf("member %v: share refusal message got %q, want %q", target, errResp.Error.Message, wantMsg)
			}
		} else {
			var successQuest shareQuest
			s.expect(http.StatusOK, "POST", sharePath, parent.AccessToken, shareBody, &successQuest)
			if !successQuest.Shared {
				t.Errorf("member %v: successful share did not set shared = true", target)
			}
			if !slices.Contains(successQuest.Participants, target) {
				t.Errorf("member %v: not found in participants %v", target, successQuest.Participants)
			}
		}
	}

	// GET .../family-members returns every member with no birthday or age key.
	var rawMembers any
	s.expect(http.StatusOK, "GET", hp+"/family-members", parent.AccessToken, nil, &rawMembers)
	noBirthdayKey(t, "family-members", rawMembers)

	var memberList struct {
		Items []struct {
			ID   uuid.UUID `json:"id"`
			Name string    `json:"name"`
		} `json:"items"`
	}
	s.expect(http.StatusOK, "GET", hp+"/family-members", parent.AccessToken, nil, &memberList)
	if len(memberList.Items) != len(allMembers) {
		t.Fatalf("family-members count: got %d, want %d", len(memberList.Items), len(allMembers))
	}
	for _, expectedID := range allMembers {
		found := false
		for _, m := range memberList.Items {
			if m.ID == expectedID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("family-members missing member %v", expectedID)
		}
	}
}
