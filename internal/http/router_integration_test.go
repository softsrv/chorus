//go:build integration

package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/auth"
	"github.com/softsrv/chorus/internal/db"
	internalhttp "github.com/softsrv/chorus/internal/http"
)

// These tests drive the full HTTP stack (middleware, routing, handlers,
// services) against a real database, the same way a mobile client would.

type mailbox struct {
	mu   sync.Mutex
	text map[string]string // recipient → latest plain-text body
}

func (m *mailbox) Send(to, _, _, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.text[to] = text
	return nil
}

var tokenRE = regexp.MustCompile(`token=([A-Za-z0-9_\-=]+)`)

func (m *mailbox) token(t *testing.T, to string) string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		m.mu.Lock()
		body := m.text[to]
		m.mu.Unlock()
		if match := tokenRE.FindStringSubmatch(body); match != nil {
			tok, _ := url.QueryUnescape(match[1])
			return tok
		}
	}
	t.Fatalf("no token emailed to %s", to)
	return ""
}

type testServer struct {
	t    *testing.T
	srv  *httptest.Server
	mail *mailbox
	pool *pgxpool.Pool
}

const secret = "router-integration-secret-at-least-32-bytes"

func newTestServer(t *testing.T) *testServer {
	t.Helper()
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
	handler := internalhttp.NewRouter(ctx, internalhttp.RouterConfig{
		Queries: q, Pool: pool, AuthSvc: authSvc,
		UserSvc:      app.NewUserService(q),
		HouseholdSvc: app.NewHouseholdService(q, pool),
		FamilySvc:    app.NewFamilyService(q, pool, 10),
		JWTSecret:    secret,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &testServer{t: t, srv: srv, mail: mail, pool: pool}
}

// do sends a JSON request and decodes a JSON response into out (if non-nil).
func (s *testServer) do(method, path, token string, body any, out any) int {
	return s.doWithHeaders(method, path, token, nil, body, out)
}

func (s *testServer) doWithHeaders(method, path, token string, headers map[string]string, body any, out any) int {
	s.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			s.t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, s.srv.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			s.t.Fatalf("%s %s: decode (status %d): %v", method, path, resp.StatusCode, err)
		}
	}
	return resp.StatusCode
}

func (s *testServer) expect(want int, method, path, token string, body any, out any) {
	s.t.Helper()
	if got := s.do(method, path, token, body, out); got != want {
		s.t.Fatalf("%s %s: status %d, want %d", method, path, got, want)
	}
}

func (s *testServer) expectWithHeaders(want int, method, path, token string, headers map[string]string, body any, out any) {
	s.t.Helper()
	if got := s.doWithHeaders(method, path, token, headers, body, out); got != want {
		s.t.Fatalf("%s %s: status %d, want %d", method, path, got, want)
	}
}

type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	User         struct {
		ID            uuid.UUID `json:"id"`
		EmailVerified bool      `json:"email_verified"`
	} `json:"user"`
}

// signUp registers and verifies a user via the API and returns their tokens.
func (s *testServer) signUp(name string) tokens {
	s.t.Helper()
	email := fmt.Sprintf("%s-%s@example.com", name, uuid.NewString()[:8])
	s.t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), "DELETE FROM users WHERE email = $1", email)
	})
	s.expect(http.StatusAccepted, "POST", "/api/v1/auth/register", "",
		map[string]string{"email": email, "password": "correct-password-123", "display_name": name}, nil)

	var tok tokens
	s.expect(http.StatusOK, "POST", "/api/v1/auth/verify-email", "",
		map[string]string{"token": s.mail.token(s.t, email)}, &tok)
	if !tok.User.EmailVerified || tok.AccessToken == "" {
		s.t.Fatalf("verify-email returned %+v", tok)
	}
	return tok
}

type apiError struct {
	Error struct {
		Code   string            `json:"code"`
		Fields map[string]string `json:"fields"`
	} `json:"error"`
}

func TestAPI_EndToEnd(t *testing.T) {
	s := newTestServer(t)
	parent := s.signUp("parent")

	// Verify auto-provisioning created an admin household on first sign-in.
	var initialHouseholds struct {
		Items []struct {
			ID   uuid.UUID `json:"id"`
			Name string    `json:"name"`
			Role string    `json:"role"`
		} `json:"items"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/households", parent.AccessToken, nil, &initialHouseholds)
	if len(initialHouseholds.Items) != 1 || initialHouseholds.Items[0].Role != "admin" {
		t.Errorf("expected 1 admin household after sign-up, got %+v", initialHouseholds.Items)
	}

	// Household + invite + join.
	var h struct {
		ID   uuid.UUID `json:"id"`
		Role string    `json:"role"`
	}
	s.expect(http.StatusCreated, "POST", "/api/v1/households", parent.AccessToken,
		map[string]string{"name": "The Smiths", "timezone": "America/Chicago"}, &h)
	if h.Role != "admin" {
		t.Errorf("creator role: got %q", h.Role)
	}
	hp := "/api/v1/households/" + h.ID.String()

	// The old chore routes are gone: the API answers its JSON 404.
	var gone apiError
	s.expect(http.StatusNotFound, "GET", hp+"/chores", parent.AccessToken, nil, &gone)
	if gone.Error.Code != "not_found" {
		t.Errorf("GET chores: code %q, want not_found", gone.Error.Code)
	}
	gone = apiError{}
	s.expect(http.StatusNotFound, "PUT", hp+"/chores/"+uuid.NewString()+"/completion", parent.AccessToken, nil, &gone)
	if gone.Error.Code != "not_found" {
		t.Errorf("PUT chore completion: code %q, want not_found", gone.Error.Code)
	}

	// Quest list pagination; the documented bounds are inclusive.
	for i := range 3 {
		s.expect(http.StatusCreated, "POST", hp+"/quests", parent.AccessToken, map[string]any{"name": fmt.Sprintf("quest %d", i)}, nil)
	}
	var page struct {
		Items      []map[string]any `json:"items"`
		NextOffset *int             `json:"next_offset"`
	}
	s.expect(http.StatusOK, "GET", hp+"/quests?limit=2", parent.AccessToken, nil, &page)
	if len(page.Items) != 2 || page.NextOffset == nil || *page.NextOffset != 2 {
		t.Fatalf("page 1: %d items, next %v", len(page.Items), page.NextOffset)
	}
	s.expect(http.StatusOK, "GET", hp+"/quests?limit=2&offset=2", parent.AccessToken, nil, &page)
	if len(page.Items) != 1 || page.NextOffset != nil {
		t.Fatalf("page 2: %d items, next %v", len(page.Items), page.NextOffset)
	}
	s.expect(http.StatusOK, "GET", hp+"/quests?limit=100", parent.AccessToken, nil, nil)
	s.expect(http.StatusUnprocessableEntity, "GET", hp+"/quests?limit=101", parent.AccessToken, nil, nil)
	s.expect(http.StatusUnprocessableEntity, "GET", hp+"/quests?limit=0", parent.AccessToken, nil, nil)
	// Clear them so the CRUD checks below start from an empty list.
	var cleanup struct {
		Items []struct {
			ID uuid.UUID `json:"id"`
		} `json:"items"`
	}
	s.expect(http.StatusOK, "GET", hp+"/quests", parent.AccessToken, nil, &cleanup)
	for _, item := range cleanup.Items {
		s.expect(http.StatusNoContent, "DELETE", hp+"/quests/"+item.ID.String(), parent.AccessToken, nil, nil)
	}

	// Quest CRUD: what is posted is what GET returns.
	type questBody struct {
		ID         uuid.UUID  `json:"id"`
		LibraryID  *uuid.UUID `json:"library_id"`
		Name       string     `json:"name"`
		Points     int        `json:"points"`
		RepeatDays int        `json:"repeat_days"`
		Notes      string     `json:"notes"`
	}
	var quests struct {
		Items      []questBody `json:"items"`
		NextOffset *int        `json:"next_offset"`
	}
	var rawList map[string]json.RawMessage
	s.expect(http.StatusOK, "GET", hp+"/quests", parent.AccessToken, nil, &rawList)
	if string(rawList["items"]) != "[]" {
		t.Errorf("empty quest list: items = %s, want []", rawList["items"])
	}
	// CLM-5: fresh household GET /quests returns 200 with items [] (not null) — and the library still lists.
	var freshLib struct {
		Items []questBody `json:"items"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/quest-library", parent.AccessToken, nil, &freshLib)
	if len(freshLib.Items) == 0 {
		t.Errorf("fresh household cannot list quest library: 0 items")
	}
	var quest questBody
	s.expect(http.StatusCreated, "POST", hp+"/quests", parent.AccessToken, map[string]any{
		"name": "Walk the dog", "points": 15, "repeat_days": 2, "notes": "Around the block",
	}, &quest)
	wantQuest := questBody{ID: quest.ID, Name: "Walk the dog", Points: 15, RepeatDays: 2, Notes: "Around the block"}
	if quest != wantQuest {
		t.Errorf("created quest: %+v", quest)
	}
	qp := hp + "/quests/" + quest.ID.String()
	var gotQuest questBody
	s.expect(http.StatusOK, "GET", qp, parent.AccessToken, nil, &gotQuest)
	if gotQuest != wantQuest {
		t.Errorf("get quest: %+v", gotQuest)
	}
	s.expect(http.StatusOK, "GET", hp+"/quests", parent.AccessToken, nil, &quests)
	if len(quests.Items) != 1 || quests.Items[0] != wantQuest || quests.NextOffset != nil {
		t.Errorf("list quests: %+v", quests)
	}
	s.expect(http.StatusOK, "PUT", qp, parent.AccessToken, map[string]any{
		"name": "Walk the dog twice", "points": 20, "repeat_days": 1, "notes": "",
	}, &gotQuest)
	if gotQuest != (questBody{ID: quest.ID, Name: "Walk the dog twice", Points: 20, RepeatDays: 1}) {
		t.Errorf("updated quest: %+v", gotQuest)
	}
	// One 422 lists every bad field, including a wrong-typed number.
	var qerr apiError
	if got := s.do("POST", hp+"/quests", parent.AccessToken, map[string]any{
		"name": "", "points": "lots", "repeat_days": 0,
	}, &qerr); got != http.StatusUnprocessableEntity ||
		qerr.Error.Fields["name"] == "" || qerr.Error.Fields["points"] == "" || qerr.Error.Fields["repeat_days"] == "" {
		t.Errorf("invalid quest: status %d, %+v", got, qerr)
	}
	// Non-numeric points value (e.g. "abc") is rejected with 422 and saves nothing.
	if got := s.do("POST", hp+"/quests", parent.AccessToken, map[string]any{
		"name": "Bad Points Quest", "points": "abc",
	}, &qerr); got != http.StatusUnprocessableEntity || qerr.Error.Fields["points"] == "" {
		t.Errorf("non-numeric points quest: status %d, %+v", got, qerr)
	}
	// notes > 2000 chars rejected
	if got := s.do("POST", hp+"/quests", parent.AccessToken, map[string]any{
		"name": "Long Notes", "notes": strings.Repeat("x", 2001),
	}, &qerr); got != http.StatusUnprocessableEntity || qerr.Error.Fields["notes"] == "" {
		t.Errorf("too long notes quest: status %d, %+v", got, qerr)
	}
	// Rejected creates saved nothing: list count is still 1.
	s.expect(http.StatusOK, "GET", hp+"/quests", parent.AccessToken, nil, &quests)
	if len(quests.Items) != 1 {
		t.Errorf("rejected creates saved rows: %d quests", len(quests.Items))
	}
	// PUT with invalid fields rejected with 422, row unchanged.
	if got := s.do("PUT", qp, parent.AccessToken, map[string]any{
		"name": "", "points": -1, "repeat_days": 0,
	}, &qerr); got != http.StatusUnprocessableEntity ||
		qerr.Error.Fields["name"] == "" || qerr.Error.Fields["points"] == "" || qerr.Error.Fields["repeat_days"] == "" {
		t.Errorf("invalid quest update: status %d, %+v", got, qerr)
	}
	var unchangedAfterPut questBody
	s.expect(http.StatusOK, "GET", qp, parent.AccessToken, nil, &unchangedAfterPut)
	if unchangedAfterPut != gotQuest {
		t.Errorf("quest row changed after rejected update: %+v vs %+v", unchangedAfterPut, gotQuest)
	}
	// CLM-12: a negative or non-whole streak_bonus is a 422 naming
	// streak_bonus, on create and on update, and saves nothing.
	for _, bad := range []any{-1, 1.5, "five"} {
		qerr = apiError{}
		if got := s.do("POST", hp+"/quests", parent.AccessToken, map[string]any{
			"name": "Streaky", "streak_bonus": bad,
		}, &qerr); got != http.StatusUnprocessableEntity || qerr.Error.Fields["streak_bonus"] == "" {
			t.Errorf("create with streak_bonus %v: status %d, %+v", bad, got, qerr)
		}
		qerr = apiError{}
		if got := s.do("PUT", qp, parent.AccessToken, map[string]any{
			"name": "Walk the dog twice", "points": 20, "repeat_days": 1, "streak_bonus": bad,
		}, &qerr); got != http.StatusUnprocessableEntity || qerr.Error.Fields["streak_bonus"] == "" {
			t.Errorf("update with streak_bonus %v: status %d, %+v", bad, got, qerr)
		}
	}
	readStreakBonus := func() int {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(context.Background(), `SELECT streak_bonus FROM household_quests WHERE id = $1`, quest.ID).Scan(&n); err != nil {
			t.Fatalf("read streak_bonus: %v", err)
		}
		return n
	}
	s.expect(http.StatusOK, "GET", hp+"/quests", parent.AccessToken, nil, &quests)
	if len(quests.Items) != 1 {
		t.Errorf("rejected streak_bonus creates saved rows: %d quests", len(quests.Items))
	}
	s.expect(http.StatusOK, "GET", qp, parent.AccessToken, nil, &unchangedAfterPut)
	if unchangedAfterPut != gotQuest {
		t.Errorf("quest row changed after rejected streak_bonus update: %+v vs %+v", unchangedAfterPut, gotQuest)
	}
	if n := readStreakBonus(); n != 0 {
		t.Errorf("streak_bonus after rejected updates: %d, want 0", n)
	}
	// Changing streak_bonus needs the admin PIN like other quest changes.
	s.expect(http.StatusOK, "PUT", hp+"/admin-pin", parent.AccessToken, map[string]string{"pin": "1357"}, nil)
	qerr = apiError{}
	s.expect(http.StatusForbidden, "PUT", qp, parent.AccessToken, map[string]any{
		"name": "Walk the dog twice", "points": 20, "repeat_days": 1, "streak_bonus": 5,
	}, &qerr)
	if qerr.Error.Code != "admin_pin_required" || readStreakBonus() != 0 {
		t.Errorf("streak_bonus update without admin pin: code %q, streak_bonus %d", qerr.Error.Code, readStreakBonus())
	}
	// A valid streak_bonus is saved with the PIN.
	s.expectWithHeaders(http.StatusOK, "PUT", qp, parent.AccessToken, map[string]string{"X-Admin-PIN": "1357"}, map[string]any{
		"name": "Walk the dog twice", "points": 20, "repeat_days": 1, "streak_bonus": 5,
	}, nil)
	if n := readStreakBonus(); n != 5 {
		t.Errorf("streak_bonus after valid update: %d, want 5", n)
	}
	// A PUT that omits streak_bonus keeps the stored one (an older client or a
	// blank edit field never wipes it).
	s.expectWithHeaders(http.StatusOK, "PUT", qp, parent.AccessToken, map[string]string{"X-Admin-PIN": "1357"}, map[string]any{
		"name": "Walk the dog twice", "points": 20, "repeat_days": 1,
	}, nil)
	if n := readStreakBonus(); n != 5 {
		t.Errorf("omitted streak_bonus on PUT: %d, want 5 kept", n)
	}
	// Back to the state the rest of this test expects: no streak bonus, no PIN.
	s.expectWithHeaders(http.StatusOK, "PUT", qp, parent.AccessToken, map[string]string{"X-Admin-PIN": "1357"}, map[string]any{
		"name": "Walk the dog twice", "points": 20, "repeat_days": 1, "streak_bonus": 0,
	}, nil)
	s.expectWithHeaders(http.StatusNoContent, "DELETE", hp+"/admin-pin", parent.AccessToken, map[string]string{"X-Admin-PIN": "1357"}, nil, nil)
	if n := readStreakBonus(); n != 0 {
		t.Errorf("explicit streak_bonus 0 on PUT: %d, want 0", n)
	}

	// CLM-4: POST/PUT/DELETE on /api/v1/quest-library return 404 or 405.
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		st := s.do(m, "/api/v1/quest-library", parent.AccessToken, map[string]any{"name": "test"}, nil)
		if st != http.StatusNotFound && st != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/v1/quest-library returned %d, want 404 or 405", m, st)
		}
	}

	// Accept: a verified acting member takes an available quest; the 200
	// body carries its notes, claimed_by and available=false, and a second
	// member is told it is taken.
	type acceptedBody struct {
		ID        uuid.UUID  `json:"id"`
		Notes     string     `json:"notes"`
		Available bool       `json:"available"`
		ClaimedBy *uuid.UUID `json:"claimed_by"`
		ClaimedAt *time.Time `json:"claimed_at"`
	}
	var member, member2 struct {
		ID uuid.UUID `json:"id"`
	}
	s.expect(http.StatusCreated, "POST", hp+"/family-members", parent.AccessToken, map[string]string{"name": "Kid", "pin": "4321"}, &member)
	s.expect(http.StatusCreated, "POST", hp+"/family-members", parent.AccessToken, map[string]string{"name": "Kid two"}, &member2)
	var acceptQuest acceptedBody
	s.expect(http.StatusCreated, "POST", hp+"/quests", parent.AccessToken, map[string]any{"name": "Take out trash", "notes": "Blue bin too"}, &acceptQuest)
	if !acceptQuest.Available || acceptQuest.ClaimedBy != nil {
		t.Errorf("new quest: %+v, want available and unclaimed", acceptQuest)
	}
	ap := hp + "/quests/" + acceptQuest.ID.String() + "/accept"
	var wrongPin apiError
	s.expect(http.StatusForbidden, "POST", ap, parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "0000"}, &wrongPin)
	if wrongPin.Error.Code != "wrong_pin" {
		t.Errorf("accept with wrong pin: code %q", wrongPin.Error.Code)
	}
	s.expect(http.StatusUnprocessableEntity, "POST", ap, parent.AccessToken, map[string]any{"pin": "4321"}, nil)
	var accepted acceptedBody
	s.expect(http.StatusOK, "POST", ap, parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, &accepted)
	if accepted.ID != acceptQuest.ID || accepted.Notes != "Blue bin too" || accepted.Available ||
		accepted.ClaimedBy == nil || *accepted.ClaimedBy != member.ID || accepted.ClaimedAt == nil {
		t.Errorf("accepted quest: %+v", accepted)
	}
	var afterAccept struct {
		Items []acceptedBody `json:"items"`
	}
	s.expect(http.StatusOK, "GET", hp+"/quests", parent.AccessToken, nil, &afterAccept)
	for _, it := range afterAccept.Items {
		if it.ID == acceptQuest.ID && (it.Available || it.ClaimedBy == nil || *it.ClaimedBy != member.ID) {
			t.Errorf("listed accepted quest: %+v", it)
		}
	}
	var taken apiError
	s.expect(http.StatusConflict, "POST", ap, parent.AccessToken, map[string]any{"member_id": member2.ID}, &taken)
	if taken.Error.Code != "quest_taken" {
		t.Errorf("second accept: code %q, want quest_taken", taken.Error.Code)
	}

	// Share: the holder of a shareable quest adds another member, each with
	// their own PIN (Kid two has none, so target_pin is empty). The quest
	// is then shared, lists both members and stays held; the share route is
	// a PIN-limited member action like accept.
	type sharedBody struct {
		ID            uuid.UUID   `json:"id"`
		ClaimedBy     *uuid.UUID  `json:"claimed_by"`
		Shareable     bool        `json:"shareable"`
		TeamworkBonus int32       `json:"teamwork_bonus"`
		Shared        bool        `json:"shared"`
		Participants  []uuid.UUID `json:"participants"`
	}
	var shareQuest sharedBody
	s.expect(http.StatusCreated, "POST", hp+"/quests", parent.AccessToken, map[string]any{"name": "Clean the living room", "points": 100}, &shareQuest)
	if shareQuest.Shared || shareQuest.Shareable || shareQuest.TeamworkBonus != 0 || shareQuest.Participants == nil || len(shareQuest.Participants) != 0 {
		t.Errorf("new quest: %+v, want not shareable, bonus 0, not shared, participants []", shareQuest)
	}
	sq := hp + "/quests/" + shareQuest.ID.String()
	// The administrator makes it shareable with a teamwork bonus by editing it;
	// a bad bonus is a 422 on teamwork_bonus and changes nothing.
	for _, bad := range []map[string]any{
		{"name": "Clean the living room", "points": 100, "shareable": true, "teamwork_bonus": -1},
		{"name": "Clean the living room", "points": 100, "shareable": true, "teamwork_bonus": 2.5},
		{"name": "Clean the living room", "points": 100, "shareable": true, "teamwork_bonus": "abc"},
		{"name": "Clean the living room", "points": 100, "shareable": false, "teamwork_bonus": 20},
	} {
		var badBonus apiError
		s.expect(http.StatusUnprocessableEntity, "PUT", sq, parent.AccessToken, bad, &badBonus)
		if badBonus.Error.Fields["teamwork_bonus"] == "" {
			t.Errorf("PUT %v: %+v, want a teamwork_bonus field error", bad, badBonus)
		}
	}
	var badCreate apiError
	s.expect(http.StatusUnprocessableEntity, "POST", hp+"/quests", parent.AccessToken, map[string]any{"name": "Bonus only", "teamwork_bonus": 5}, &badCreate)
	if badCreate.Error.Fields["teamwork_bonus"] == "" {
		t.Errorf("create with bonus but not shareable: %+v, want a teamwork_bonus field error", badCreate)
	}
	var madeShareable sharedBody
	s.expect(http.StatusOK, "PUT", sq, parent.AccessToken, map[string]any{"name": "Clean the living room", "points": 100, "shareable": true, "teamwork_bonus": 20}, &madeShareable)
	if !madeShareable.Shareable || madeShareable.TeamworkBonus != 20 {
		t.Errorf("edited quest: %+v, want shareable, bonus 20", madeShareable)
	}
	var createdShareable sharedBody
	s.expect(http.StatusCreated, "POST", hp+"/quests", parent.AccessToken, map[string]any{"name": "Wash the car", "points": 30, "shareable": true, "teamwork_bonus": 5}, &createdShareable)
	if !createdShareable.Shareable || createdShareable.TeamworkBonus != 5 {
		t.Errorf("created shareable quest: %+v, want shareable, bonus 5", createdShareable)
	}
	var gotShareable sharedBody
	s.expect(http.StatusOK, "GET", hp+"/quests/"+createdShareable.ID.String(), parent.AccessToken, nil, &gotShareable)
	if !gotShareable.Shareable || gotShareable.TeamworkBonus != 5 {
		t.Errorf("get shareable quest: %+v, want shareable, bonus 5", gotShareable)
	}
	s.expect(http.StatusNoContent, "DELETE", hp+"/quests/"+createdShareable.ID.String(), parent.AccessToken, nil, nil)
	s.expect(http.StatusOK, "POST", sq+"/accept", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, nil)
	var shareErr apiError
	s.expect(http.StatusUnprocessableEntity, "POST", sq+"/share", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, &shareErr)
	if shareErr.Error.Fields["target_member_id"] == "" {
		t.Errorf("share without target: %+v, want a target_member_id field error", shareErr)
	}
	shareErr = apiError{}
	s.expect(http.StatusForbidden, "POST", sq+"/share", parent.AccessToken,
		map[string]any{"member_id": member.ID, "pin": "4321", "target_member_id": member2.ID, "target_pin": "0000"}, &shareErr)
	if shareErr.Error.Code != "wrong_pin" || shareErr.Error.Fields["target_pin"] == "" {
		t.Errorf("share with wrong target pin: %+v, want wrong_pin with a target_pin field", shareErr)
	}
	shareErr = apiError{}
	s.expect(http.StatusForbidden, "POST", sq+"/share", parent.AccessToken,
		map[string]any{"member_id": member.ID, "pin": "0000", "target_member_id": member2.ID, "target_pin": ""}, &shareErr)
	if shareErr.Error.Code != "wrong_pin" || len(shareErr.Error.Fields) != 0 {
		t.Errorf("share with wrong sharer pin: %+v, want wrong_pin with no fields", shareErr)
	}
	s.expect(http.StatusNotFound, "POST", sq+"/share", parent.AccessToken,
		map[string]any{"member_id": member.ID, "pin": "4321", "target_member_id": uuid.New()}, nil)
	var sharedQuest sharedBody
	s.expect(http.StatusOK, "POST", sq+"/share", parent.AccessToken,
		map[string]any{"member_id": member.ID, "pin": "4321", "target_member_id": member2.ID, "target_pin": ""}, &sharedQuest)
	wantParticipants := []uuid.UUID{member.ID, member2.ID}
	if !sharedQuest.Shared || !slices.Equal(sharedQuest.Participants, wantParticipants) ||
		sharedQuest.ClaimedBy == nil || *sharedQuest.ClaimedBy != member.ID {
		t.Errorf("shared quest: %+v, want shared, participants %v, held by %v", sharedQuest, wantParticipants, member.ID)
	}
	var gotShared sharedBody
	s.expect(http.StatusOK, "GET", sq, parent.AccessToken, nil, &gotShared)
	if !gotShared.Shared || !slices.Equal(gotShared.Participants, wantParticipants) {
		t.Errorf("get shared quest: %+v", gotShared)
	}
	var sharedList struct {
		Items []sharedBody `json:"items"`
	}
	s.expect(http.StatusOK, "GET", hp+"/quests", parent.AccessToken, nil, &sharedList)
	for _, it := range sharedList.Items {
		if it.ID == shareQuest.ID && (!it.Shared || !slices.Equal(it.Participants, wantParticipants)) {
			t.Errorf("listed shared quest: %+v", it)
		}
		if it.ID != shareQuest.ID && (it.Shared || len(it.Participants) != 0) {
			t.Errorf("listed unshared quest %s: shared=%v participants=%v", it.ID, it.Shared, it.Participants)
		}
	}
	shareErr = apiError{}
	s.expect(http.StatusConflict, "POST", sq+"/share", parent.AccessToken,
		map[string]any{"member_id": member.ID, "pin": "4321", "target_member_id": member2.ID}, &shareErr)
	if shareErr.Error.Code != "already_participant" {
		t.Errorf("share again: code %q, want already_participant", shareErr.Error.Code)
	}
	// Clear it so the quest lists below hold only the trash chore.
	s.expect(http.StatusNoContent, "DELETE", sq, parent.AccessToken, nil, nil)

	// Approval is per completion: nothing waits yet ({items: []}, not null);
	// the holder marks the chore done, which releases it into its cooldown and
	// records a pending completion; the administrator sends that completion
	// back, which reopens the chore; the holder does it again and approval
	// credits the points, visible in the family-member list.
	qbase := hp + "/quests/" + acceptQuest.ID.String()
	var rawPending map[string]json.RawMessage
	s.expect(http.StatusOK, "GET", hp+"/quests/pending", parent.AccessToken, nil, &rawPending)
	if string(rawPending["items"]) != "[]" {
		t.Errorf("empty pending list: items = %s, want []", rawPending["items"])
	}
	s.expect(http.StatusNotFound, "POST", qbase+"/abandon", parent.AccessToken, map[string]any{"member_id": member2.ID}, nil)
	s.expect(http.StatusNotFound, "POST", qbase+"/done", parent.AccessToken, map[string]any{"member_id": member2.ID}, nil)
	var doneBody map[string]json.RawMessage
	s.expect(http.StatusOK, "POST", qbase+"/done", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, &doneBody)
	if string(doneBody["awaiting_approval"]) != "true" || string(doneBody["claimed_by"]) != "null" || string(doneBody["available"]) != "false" ||
		string(doneBody["available_at"]) == "null" || string(doneBody["completed_at"]) == "null" {
		t.Errorf("marked done: %s", doneBody)
	}
	if _, has := doneBody["done_at"]; has {
		t.Errorf("marked done still carries done_at: %s", doneBody)
	}
	// The holder let go at mark-done: it can no longer abandon the chore.
	s.expect(http.StatusNotFound, "POST", qbase+"/abandon", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, nil)
	listedQuest := func() (awaiting, available bool, hasDoneAt bool) {
		t.Helper()
		var list struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		s.expect(http.StatusOK, "GET", hp+"/quests", parent.AccessToken, nil, &list)
		for _, it := range list.Items {
			if string(it["id"]) == `"`+acceptQuest.ID.String()+`"` {
				_, has := it["done_at"]
				return string(it["awaiting_approval"]) == "true", string(it["available"]) == "true", has
			}
		}
		t.Fatalf("quest %s not listed", acceptQuest.ID)
		return false, false, false
	}
	if awaiting, available, hasDoneAt := listedQuest(); !awaiting || available || hasDoneAt {
		t.Errorf("listed after done: awaiting_approval=%v available=%v done_at present=%v, want true false false", awaiting, available, hasDoneAt)
	}
	type pendingRow struct {
		CompletionID uuid.UUID  `json:"completion_id"`
		QuestID      uuid.UUID  `json:"quest_id"`
		Name         string     `json:"name"`
		Points       int        `json:"points"`
		MemberName   string     `json:"member_name"`
		DoneAt       *time.Time `json:"done_at"`
	}
	var pendingList struct {
		Items []pendingRow `json:"items"`
	}
	s.expect(http.StatusOK, "GET", hp+"/quests/pending", parent.AccessToken, nil, &pendingList)
	if len(pendingList.Items) != 1 || pendingList.Items[0].QuestID != acceptQuest.ID || pendingList.Items[0].MemberName != "Kid" ||
		pendingList.Items[0].Name != "Take out trash" || pendingList.Items[0].Points != 0 ||
		pendingList.Items[0].CompletionID == uuid.Nil || pendingList.Items[0].DoneAt == nil {
		t.Errorf("pending list: %+v", pendingList.Items)
	}
	firstCompletion := hp + "/quest-completions/" + pendingList.Items[0].CompletionID.String()
	var rejectedBody struct {
		Completion struct {
			ID     uuid.UUID `json:"id"`
			Status string    `json:"status"`
		} `json:"completion"`
	}
	s.expect(http.StatusOK, "POST", firstCompletion+"/reject", parent.AccessToken, map[string]any{}, &rejectedBody)
	if rejectedBody.Completion.ID != pendingList.Items[0].CompletionID || rejectedBody.Completion.Status != "rejected" {
		t.Errorf("rejected: %+v", rejectedBody)
	}
	s.expect(http.StatusNotFound, "POST", firstCompletion+"/reject", parent.AccessToken, map[string]any{}, nil)
	s.expect(http.StatusNotFound, "POST", firstCompletion+"/approve", parent.AccessToken, map[string]any{}, nil)
	s.expect(http.StatusBadRequest, "POST", firstCompletion+"/approve", parent.AccessToken, map[string]any{"admin_pin": "1234"}, nil)
	if awaiting, available, _ := listedQuest(); awaiting || !available {
		t.Errorf("listed after reject: awaiting_approval=%v available=%v, want false true", awaiting, available)
	}
	// The old per-quest approve and reject routes are gone.
	s.expect(http.StatusNotFound, "POST", qbase+"/approve", parent.AccessToken, map[string]any{}, nil)
	s.expect(http.StatusNotFound, "POST", qbase+"/reject", parent.AccessToken, map[string]any{}, nil)
	// Give the quest points so the credit is visible, then do it again.
	s.expect(http.StatusOK, "PUT", qbase, parent.AccessToken, map[string]any{"name": "Take out trash", "points": 9, "repeat_days": 2, "notes": "Blue bin too"}, nil)
	s.expect(http.StatusOK, "POST", qbase+"/accept", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, nil)
	s.expect(http.StatusOK, "POST", qbase+"/done", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, nil)
	pendingList.Items = nil
	s.expect(http.StatusOK, "GET", hp+"/quests/pending", parent.AccessToken, nil, &pendingList)
	if len(pendingList.Items) != 1 || pendingList.Items[0].Points != 9 {
		t.Fatalf("pending after second mark-done: %+v", pendingList.Items)
	}
	cbase := hp + "/quest-completions/" + pendingList.Items[0].CompletionID.String()

	// With an admin PIN set: a missing or wrong header is a 403, never a
	// sign-out 401, and changes nothing.
	s.expect(http.StatusOK, "PUT", hp+"/admin-pin", parent.AccessToken, map[string]string{"pin": "8642"}, nil)
	var pinErr apiError
	s.expect(http.StatusForbidden, "POST", cbase+"/approve", parent.AccessToken, map[string]any{}, &pinErr)
	if pinErr.Error.Code != "admin_pin_required" {
		t.Errorf("approve without admin pin: code %q", pinErr.Error.Code)
	}
	pinErr = apiError{}
	s.expectWithHeaders(http.StatusForbidden, "POST", cbase+"/approve", parent.AccessToken, map[string]string{"X-Admin-PIN": "0000"}, map[string]any{}, &pinErr)
	if pinErr.Error.Code != "wrong_pin" {
		t.Errorf("approve with wrong admin pin: code %q", pinErr.Error.Code)
	}
	pinErr = apiError{}
	s.expectWithHeaders(http.StatusForbidden, "GET", hp+"/quests/pending", parent.AccessToken, map[string]string{"X-Admin-PIN": "0000"}, nil, &pinErr)
	if pinErr.Error.Code != "wrong_pin" {
		t.Errorf("pending with wrong admin pin: code %q", pinErr.Error.Code)
	}
	s.expectWithHeaders(http.StatusForbidden, "POST", cbase+"/reject", parent.AccessToken, map[string]string{"X-Admin-PIN": "0000"}, map[string]any{}, nil)
	adminHdr := map[string]string{"X-Admin-PIN": "8642"}
	pendingList.Items = nil
	s.expectWithHeaders(http.StatusOK, "GET", hp+"/quests/pending", parent.AccessToken, adminHdr, nil, &pendingList)
	if len(pendingList.Items) != 1 || pendingList.Items[0].Points != 9 {
		t.Errorf("pending after refused approvals: %+v", pendingList.Items)
	}

	var approvedBody struct {
		Completion struct {
			ID         uuid.UUID   `json:"id"`
			QuestID    uuid.UUID   `json:"quest_id"`
			DoneAt     *time.Time  `json:"done_at"`
			ApprovedAt *time.Time  `json:"approved_at"`
			Status     string      `json:"status"`
			MemberIDs  []uuid.UUID `json:"member_ids"`
		} `json:"completion"`
		Member struct {
			ID     uuid.UUID `json:"id"`
			Name   string    `json:"name"`
			HasPIN bool      `json:"has_pin"`
			Points int       `json:"points"`
		} `json:"member"`
		ComboBonuses []json.RawMessage `json:"combo_bonuses"`
		StreakBonus  *int              `json:"streak_bonus"`
		StreakCount  *int              `json:"streak_count"`
		Members      []struct {
			Member struct {
				ID     uuid.UUID `json:"id"`
				Points int       `json:"points"`
			} `json:"member"`
			PointsAwarded int `json:"points_awarded"`
			TeamworkBonus int `json:"teamwork_bonus"`
		} `json:"members"`
	}
	s.expectWithHeaders(http.StatusOK, "POST", cbase+"/approve", parent.AccessToken, adminHdr, map[string]any{}, &approvedBody)
	// CLM-13 / CLM-16: the first mark-done was sent back, which undid its
	// streak step, so this approval is the 1st in a row: count 1, no bonus.
	if approvedBody.StreakBonus == nil || *approvedBody.StreakBonus != 0 ||
		approvedBody.StreakCount == nil || *approvedBody.StreakCount != 1 {
		t.Errorf("approve streak fields: streak_bonus=%v streak_count=%v, want 0 and 1", approvedBody.StreakBonus, approvedBody.StreakCount)
	}
	if approvedBody.Completion.ID != pendingList.Items[0].CompletionID || approvedBody.Completion.QuestID != acceptQuest.ID ||
		approvedBody.Completion.Status != "approved" || approvedBody.Completion.ApprovedAt == nil ||
		approvedBody.Completion.DoneAt == nil || !approvedBody.Completion.DoneAt.Equal(*pendingList.Items[0].DoneAt) ||
		approvedBody.Member.ID != member.ID || approvedBody.Member.Name != "Kid" || !approvedBody.Member.HasPIN || approvedBody.Member.Points != 9 ||
		approvedBody.ComboBonuses == nil || len(approvedBody.ComboBonuses) != 0 {
		t.Errorf("approved: %+v", approvedBody)
	}
	// A quest that is not shared credits its one member, with no teamwork
	// bonus, in members as well as member.
	if len(approvedBody.Members) != 1 || approvedBody.Members[0].Member.ID != member.ID ||
		approvedBody.Members[0].Member.Points != 9 || approvedBody.Members[0].TeamworkBonus != 0 ||
		len(approvedBody.Completion.MemberIDs) != 1 || approvedBody.Completion.MemberIDs[0] != member.ID {
		t.Errorf("approved members: %+v, completion member_ids %v; want only Kid, no bonus", approvedBody.Members, approvedBody.Completion.MemberIDs)
	}
	s.expectWithHeaders(http.StatusNotFound, "POST", cbase+"/approve", parent.AccessToken, adminHdr, map[string]any{}, nil)
	if awaiting, _, _ := listedQuest(); awaiting {
		t.Errorf("listed after approval: awaiting_approval = true, want false")
	}
	var familyList struct {
		Items []struct {
			ID     uuid.UUID `json:"id"`
			Points int       `json:"points"`
		} `json:"items"`
	}
	s.expect(http.StatusOK, "GET", hp+"/family-members", parent.AccessToken, nil, &familyList)
	for _, m := range familyList.Items {
		want := 0
		if m.ID == member.ID {
			want = 9
		}
		if m.Points != want {
			t.Errorf("family member %s points = %d, want %d", m.ID, m.Points, want)
		}
	}
	rawPending = nil
	s.expectWithHeaders(http.StatusOK, "GET", hp+"/quests/pending", parent.AccessToken, adminHdr, nil, &rawPending)
	if string(rawPending["items"]) != "[]" {
		t.Errorf("pending after approval: items = %s, want []", rawPending["items"])
	}

	// Points history (CLM-6):
	// A second quest approval for member so two awards are present.
	var quest2 acceptedBody
	s.expectWithHeaders(http.StatusCreated, "POST", hp+"/quests", parent.AccessToken, adminHdr, map[string]any{"name": "Dishes", "points": 5}, &quest2)
	q2base := hp + "/quests/" + quest2.ID.String()
	s.expect(http.StatusOK, "POST", q2base+"/accept", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, nil)
	s.expect(http.StatusOK, "POST", q2base+"/done", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, nil)
	pendingList.Items = nil
	s.expectWithHeaders(http.StatusOK, "GET", hp+"/quests/pending", parent.AccessToken, adminHdr, nil, &pendingList)
	if len(pendingList.Items) != 1 {
		t.Fatalf("pending after quest2 mark-done: %+v", pendingList.Items)
	}
	c2base := hp + "/quest-completions/" + pendingList.Items[0].CompletionID.String()
	s.expectWithHeaders(http.StatusOK, "POST", c2base+"/approve", parent.AccessToken, adminHdr, map[string]any{}, nil)
	s.expectWithHeaders(http.StatusNoContent, "DELETE", q2base, parent.AccessToken, adminHdr, nil, nil)

	histPath := hp + "/family-members/" + member.ID.String() + "/points-history"

	// Wrong X-Member-PIN returns 403 wrong_pin.
	var histPinErr apiError
	s.expectWithHeaders(http.StatusForbidden, "GET", histPath, parent.AccessToken, map[string]string{"X-Member-PIN": "0000"}, nil, &histPinErr)
	if histPinErr.Error.Code != "wrong_pin" {
		t.Errorf("history with wrong pin: code %q", histPinErr.Error.Code)
	}

	// GET after two approvals returns 200 with items newest first and all award fields.
	var hist struct {
		Items []struct {
			ID       uuid.UUID `json:"id"`
			Kind     string    `json:"kind"`
			Reason   string    `json:"reason"`
			Points   int       `json:"points"`
			EarnedAt time.Time `json:"earned_at"`
		} `json:"items"`
	}
	s.expectWithHeaders(http.StatusOK, "GET", histPath, parent.AccessToken, map[string]string{"X-Member-PIN": "4321"}, nil, &hist)
	if len(hist.Items) != 2 {
		t.Fatalf("points history count: got %d, want 2", len(hist.Items))
	}
	if hist.Items[0].Reason != "Dishes" || hist.Items[0].Points != 5 || hist.Items[0].Kind != "quest" {
		t.Errorf("newest award mismatch: %+v", hist.Items[0])
	}
	if hist.Items[1].Reason != "Take out trash" || hist.Items[1].Points != 9 || hist.Items[1].Kind != "quest" {
		t.Errorf("older award mismatch: %+v", hist.Items[1])
	}
	if hist.Items[0].ID == uuid.Nil || hist.Items[0].EarnedAt.IsZero() {
		t.Errorf("expected populated award fields: %+v", hist.Items[0])
	}

	// GET for a member with no awards returns {items: []}.
	var emptyHist struct {
		Items []map[string]any `json:"items"`
	}
	s.expect(http.StatusOK, "GET", hp+"/family-members/"+member2.ID.String()+"/points-history", parent.AccessToken, nil, &emptyHist)
	if emptyHist.Items == nil || len(emptyHist.Items) != 0 {
		t.Errorf("empty member history: %+v, want []", emptyHist.Items)
	}

	// GET for unknown member in household returns 404 (identical to random uuid).
	var randHistErr apiError
	s.expect(http.StatusNotFound, "GET", hp+"/family-members/"+uuid.NewString()+"/points-history", parent.AccessToken, nil, &randHistErr)
	if randHistErr.Error.Code != "not_found" {
		t.Errorf("random member points-history: code %q, want not_found", randHistErr.Error.Code)
	}

	// A shared quest completes for everyone on it: Kid accepts a shareable
	// 100-point quest with a teamwork bonus of 20 and shares it with Kid two,
	// who marks it done. One item waits, naming the marker in the old
	// member_id/member_name and both members with 50 each in members;
	// approving pays each 100/2 + 20 = 70 once, keeping completion, member
	// (the marker) and combo_bonuses beside the new members and member_ids.
	var teamQuest sharedBody
	s.expectWithHeaders(http.StatusCreated, "POST", hp+"/quests", parent.AccessToken, adminHdr,
		map[string]any{"name": "Clean the living room", "points": 100, "shareable": true, "teamwork_bonus": 20}, &teamQuest)
	tq := hp + "/quests/" + teamQuest.ID.String()
	s.expect(http.StatusOK, "POST", tq+"/accept", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, nil)
	s.expect(http.StatusOK, "POST", tq+"/share", parent.AccessToken,
		map[string]any{"member_id": member.ID, "pin": "4321", "target_member_id": member2.ID, "target_pin": ""}, nil)
	s.expect(http.StatusOK, "POST", tq+"/done", parent.AccessToken, map[string]any{"member_id": member2.ID}, nil)
	var teamPending struct {
		Items []struct {
			CompletionID  uuid.UUID `json:"completion_id"`
			QuestID       uuid.UUID `json:"quest_id"`
			Name          string    `json:"name"`
			Points        int       `json:"points"`
			TeamworkBonus int       `json:"teamwork_bonus"`
			MemberID      uuid.UUID `json:"member_id"`
			MemberName    string    `json:"member_name"`
			Members       []struct {
				MemberID   uuid.UUID `json:"member_id"`
				MemberName string    `json:"member_name"`
				Points     int       `json:"points"`
			} `json:"members"`
		} `json:"items"`
	}
	s.expectWithHeaders(http.StatusOK, "GET", hp+"/quests/pending", parent.AccessToken, adminHdr, nil, &teamPending)
	if len(teamPending.Items) != 1 {
		t.Fatalf("pending after shared mark-done: %+v, want one item", teamPending.Items)
	}
	tp := teamPending.Items[0]
	if tp.QuestID != teamQuest.ID || tp.Name != "Clean the living room" || tp.Points != 100 || tp.TeamworkBonus != 20 ||
		tp.MemberID != member2.ID || tp.MemberName != "Kid two" || len(tp.Members) != 2 {
		t.Errorf("shared pending item: %+v, want marker Kid two, bonus 20, two members", tp)
	}
	wantNames := map[uuid.UUID]string{member.ID: "Kid", member2.ID: "Kid two"}
	for _, m := range tp.Members {
		if wantNames[m.MemberID] != m.MemberName || m.Points != 50 {
			t.Errorf("shared pending member: %+v, want one of %v with 50 points", m, wantNames)
		}
	}
	if len(tp.Members) == 2 && tp.Members[0].MemberID == tp.Members[1].MemberID {
		t.Errorf("shared pending members repeat one member: %+v", tp.Members)
	}
	tc := hp + "/quest-completions/" + tp.CompletionID.String()
	var teamApproved struct {
		Completion struct {
			ID        uuid.UUID   `json:"id"`
			QuestID   uuid.UUID   `json:"quest_id"`
			MemberID  *uuid.UUID  `json:"member_id"`
			MemberIDs []uuid.UUID `json:"member_ids"`
			Status    string      `json:"status"`
		} `json:"completion"`
		Member struct {
			ID     uuid.UUID `json:"id"`
			Name   string    `json:"name"`
			Points int       `json:"points"`
		} `json:"member"`
		ComboBonuses []json.RawMessage `json:"combo_bonuses"`
		Members      []struct {
			Member struct {
				ID     uuid.UUID `json:"id"`
				Points int       `json:"points"`
			} `json:"member"`
			PointsAwarded int `json:"points_awarded"`
			TeamworkBonus int `json:"teamwork_bonus"`
		} `json:"members"`
	}
	s.expectWithHeaders(http.StatusOK, "POST", tc+"/approve", parent.AccessToken, adminHdr, map[string]any{}, &teamApproved)
	// Kid already had 9 from the trash chore and 5 from Dishes; Kid two had nothing.
	wantTotals := map[uuid.UUID]int{member.ID: 84, member2.ID: 70}
	if teamApproved.Completion.ID != tp.CompletionID || teamApproved.Completion.QuestID != teamQuest.ID ||
		teamApproved.Completion.Status != "approved" || teamApproved.Completion.MemberID == nil || *teamApproved.Completion.MemberID != member2.ID ||
		len(teamApproved.Completion.MemberIDs) != 2 ||
		teamApproved.Member.ID != member2.ID || teamApproved.Member.Name != "Kid two" || teamApproved.Member.Points != 70 ||
		teamApproved.ComboBonuses == nil || len(teamApproved.ComboBonuses) != 0 || len(teamApproved.Members) != 2 {
		t.Errorf("shared approve: %+v", teamApproved)
	}
	for _, id := range teamApproved.Completion.MemberIDs {
		if _, ok := wantTotals[id]; !ok {
			t.Errorf("shared approve member_ids: %v, want Kid and Kid two", teamApproved.Completion.MemberIDs)
		}
	}
	for _, m := range teamApproved.Members {
		if want, ok := wantTotals[m.Member.ID]; !ok || m.Member.Points != want || m.PointsAwarded != 50 || m.TeamworkBonus != 20 {
			t.Errorf("shared approve member: %+v, want 50 + 20 bonus, total %d", m, want)
		}
	}
	s.expectWithHeaders(http.StatusNotFound, "POST", tc+"/approve", parent.AccessToken, adminHdr, map[string]any{}, nil)
	familyList.Items = nil
	s.expect(http.StatusOK, "GET", hp+"/family-members", parent.AccessToken, nil, &familyList)
	for _, m := range familyList.Items {
		if want := wantTotals[m.ID]; m.Points != want {
			t.Errorf("after shared approve and repeat: member %s points = %d, want %d", m.ID, m.Points, want)
		}
	}
	// CLM-1: every member of the shared completion gets a history entry for
	// their split and one for the teamwork bonus, kind quest, reason the
	// quest name, earned at the approval.
	var teamHist struct {
		Items []struct {
			Kind     string    `json:"kind"`
			Reason   string    `json:"reason"`
			Points   int       `json:"points"`
			EarnedAt time.Time `json:"earned_at"`
		} `json:"items"`
	}
	s.expect(http.StatusOK, "GET", hp+"/family-members/"+member2.ID.String()+"/points-history", parent.AccessToken, nil, &teamHist)
	if len(teamHist.Items) != 2 {
		t.Fatalf("shared quest history for Kid two: %+v, want split and teamwork bonus entries", teamHist.Items)
	}
	gotPts := map[int]bool{}
	for _, it := range teamHist.Items {
		if it.Kind != "quest" || it.Reason != "Clean the living room" || it.EarnedAt.IsZero() {
			t.Errorf("shared quest history entry: %+v", it)
		}
		gotPts[it.Points] = true
	}
	if !gotPts[50] || !gotPts[20] {
		t.Errorf("shared quest history points: %+v, want 50 and 20", teamHist.Items)
	}
	teamHist.Items = nil
	s.expectWithHeaders(http.StatusOK, "GET", histPath, parent.AccessToken, map[string]string{"X-Member-PIN": "4321"}, nil, &teamHist)
	if len(teamHist.Items) != 4 || teamHist.Items[0].Reason != "Clean the living room" || teamHist.Items[1].Reason != "Clean the living room" ||
		teamHist.Items[0].Points+teamHist.Items[1].Points != 70 {
		t.Errorf("shared quest history for Kid: %+v, want two newest entries of 50 and 20", teamHist.Items)
	}
	s.expectWithHeaders(http.StatusNoContent, "DELETE", tq, parent.AccessToken, adminHdr, nil, nil)
	// No self-complete route exists.
	s.expect(http.StatusNotFound, "POST", qbase+"/complete", parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, nil)
	s.expectWithHeaders(http.StatusNoContent, "DELETE", hp+"/admin-pin", parent.AccessToken, adminHdr, nil, nil)
	s.expect(http.StatusNoContent, "DELETE", hp+"/quests/"+acceptQuest.ID.String(), parent.AccessToken, nil, nil)

	// Library copy keeps the library's values and its id.
	var library struct {
		Items []questBody `json:"items"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/quest-library", parent.AccessToken, nil, &library)
	if len(library.Items) == 0 {
		t.Fatal("quest library is empty")
	}
	var copied questBody
	s.expect(http.StatusCreated, "POST", hp+"/quests/from-library", parent.AccessToken,
		map[string]any{"library_id": library.Items[0].ID}, &copied)
	if copied.LibraryID == nil || *copied.LibraryID != library.Items[0].ID || copied.Name != library.Items[0].Name ||
		copied.Points != library.Items[0].Points || copied.RepeatDays != library.Items[0].RepeatDays {
		t.Errorf("copied quest %+v vs library %+v", copied, library.Items[0])
	}

	s.expect(http.StatusNoContent, "DELETE", qp, parent.AccessToken, nil, nil)
	s.expect(http.StatusNotFound, "GET", qp, parent.AccessToken, nil, nil)

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
	autoHP := "/api/v1/households/" + initialHouseholds.Items[0].ID.String()
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

	// No redeem, spend or buy route exists, and no reward action moved points.
	for _, action := range []string{"redeem", "spend", "buy"} {
		if got := s.do("POST", rp+"/"+action, parent.AccessToken, map[string]any{"member_id": member.ID, "pin": "4321"}, nil); got != http.StatusNotFound && got != http.StatusMethodNotAllowed {
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
	s.expect(http.StatusNotFound, "GET", hp+"/quests/"+copied.ID.String(), outsider.AccessToken, nil, nil)
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

func TestAPI_SessionsAndLogout(t *testing.T) {
	s := newTestServer(t)
	u := s.signUp("sessions")

	s.expect(http.StatusOK, "GET", "/api/v1/me", u.AccessToken, nil, nil)

	// Refresh issues a working access token.
	var refreshed tokens
	s.expect(http.StatusOK, "POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": u.RefreshToken}, &refreshed)
	s.expect(http.StatusOK, "GET", "/api/v1/me", refreshed.AccessToken, nil, nil)

	// Logout kills both tokens immediately — not when the access token expires.
	s.expect(http.StatusNoContent, "POST", "/api/v1/auth/logout", u.AccessToken, nil, nil)
	var e apiError
	s.expect(http.StatusUnauthorized, "GET", "/api/v1/me", u.AccessToken, nil, &e)
	if e.Error.Code != "unauthenticated" {
		t.Errorf("after logout: code %q", e.Error.Code)
	}
	s.expect(http.StatusUnauthorized, "POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": u.RefreshToken}, nil)
}

func TestAPI_RequestHardening(t *testing.T) {
	s := newTestServer(t)
	u := s.signUp("hardening")

	var e apiError
	// Unknown fields are rejected rather than silently ignored.
	s.expect(http.StatusBadRequest, "POST", "/api/v1/households", u.AccessToken,
		map[string]any{"name": "x", "is_admin": true}, &e)
	if e.Error.Code != "invalid_json" {
		t.Errorf("unknown field: code %q", e.Error.Code)
	}

	// Non-JSON content types are rejected.
	req, _ := http.NewRequest("POST", s.srv.URL+"/api/v1/households", strings.NewReader("name=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+u.AccessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("form post: status %d, want 415", resp.StatusCode)
	}

	// Oversized bodies are rejected.
	s.expect(http.StatusRequestEntityTooLarge, "POST", "/api/v1/households", u.AccessToken,
		map[string]string{"name": strings.Repeat("x", 100<<10)}, nil)

	// Unknown API routes return JSON 404s, not the web client.
	s.expect(http.StatusNotFound, "GET", "/api/v1/nope", u.AccessToken, nil, &e)

	// Unverified users can see their account but nothing else.
	email := fmt.Sprintf("unverified-%s@example.com", uuid.NewString()[:8])
	t.Cleanup(func() { _, _ = s.pool.Exec(context.Background(), "DELETE FROM users WHERE email = $1", email) })
	s.expect(http.StatusAccepted, "POST", "/api/v1/auth/register", "",
		map[string]string{"email": email, "password": "correct-password-123", "display_name": "U"}, nil)
	var unv tokens
	s.expect(http.StatusOK, "POST", "/api/v1/auth/login", "",
		map[string]string{"email": email, "password": "correct-password-123"}, &unv)
	s.expect(http.StatusOK, "GET", "/api/v1/me", unv.AccessToken, nil, nil)
	s.expect(http.StatusForbidden, "GET", "/api/v1/households", unv.AccessToken, nil, &e)
	if e.Error.Code != "email_not_verified" {
		t.Errorf("unverified: code %q", e.Error.Code)
	}

	// Responses carry no-store and security headers.
	resp, err = http.Get(s.srv.URL + "/api/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for h, want := range map[string]string{"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff"} {
		if got := resp.Header.Get(h); got != want {
			t.Errorf("%s: got %q, want %q", h, got, want)
		}
	}
}

// passwordless is a Google-only account (NULL password_hash) signed in on one
// session. It is built straight from the queries the Google sign-in flow uses,
// so these tests don't depend on talking to Google.
type passwordless struct {
	ID          uuid.UUID
	Email       string
	SessionID   uuid.UUID
	AccessToken string
}

func (s *testServer) signUpPasswordless(name string) passwordless {
	s.t.Helper()
	ctx := context.Background()
	q := db.New(s.pool)
	id := uuid.Must(uuid.NewV7())
	email := fmt.Sprintf("%s-%s@example.com", name, uuid.NewString()[:8])
	user, err := q.CreateGoogleUser(ctx, db.CreateGoogleUserParams{
		ID: id, Email: email, DisplayName: name,
		GoogleSubject: pgtype.Text{String: "google-sub-" + id.String(), Valid: true},
	})
	if err != nil {
		s.t.Fatalf("CreateGoogleUser: %v", err)
	}
	s.t.Cleanup(func() { _, _ = s.pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", user.ID) })

	_, hashed, err := auth.GenerateRefreshToken()
	if err != nil {
		s.t.Fatalf("GenerateRefreshToken: %v", err)
	}
	sessionID := uuid.Must(uuid.NewV7())
	if _, err := q.InsertRefreshToken(ctx, db.InsertRefreshTokenParams{
		ID: sessionID, UserID: user.ID, TokenHash: hashed,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		s.t.Fatalf("InsertRefreshToken: %v", err)
	}
	tp, err := auth.IssueAccessToken(user.ID, sessionID, secret, 15*time.Minute)
	if err != nil {
		s.t.Fatalf("IssueAccessToken: %v", err)
	}
	return passwordless{ID: user.ID, Email: email, SessionID: sessionID, AccessToken: tp.AccessToken}
}

func (s *testServer) userExists(id uuid.UUID) bool {
	s.t.Helper()
	var found bool
	if err := s.pool.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)", id).Scan(&found); err != nil {
		s.t.Fatalf("user exists: %v", err)
	}
	return found
}

// CLM-11: /me reports has_password, and a passwordless user asking to change
// a password gets a field validation error rather than invalid credentials.
func TestAPI_PasswordlessAccountBasics(t *testing.T) {
	s := newTestServer(t)
	pwUser := s.signUp("haspw")
	gUser := s.signUpPasswordless("nopw")

	var me struct {
		HasPassword *bool `json:"has_password"`
	}
	s.expect(http.StatusOK, "GET", "/api/v1/me", pwUser.AccessToken, nil, &me)
	if me.HasPassword == nil || !*me.HasPassword {
		t.Errorf("password user: has_password = %v, want true", me.HasPassword)
	}
	me.HasPassword = nil
	s.expect(http.StatusOK, "GET", "/api/v1/me", gUser.AccessToken, nil, &me)
	if me.HasPassword == nil || *me.HasPassword {
		t.Errorf("passwordless user: has_password = %v, want false", me.HasPassword)
	}

	var before, after db.User
	q := db.New(s.pool)
	before, _ = q.GetUserByID(context.Background(), gUser.ID)
	var e apiError
	s.expect(http.StatusUnprocessableEntity, "POST", "/api/v1/me/password", gUser.AccessToken,
		map[string]string{"current_password": "anything", "new_password": "brand-new-password"}, &e)
	if e.Error.Code == "invalid_credentials" || e.Error.Fields["current_password"] == "" || e.Error.Fields["current_password"] == "is incorrect" {
		t.Errorf("passwordless change password: got %+v, want a current_password field error that isn't invalid credentials", e)
	}
	after, _ = q.GetUserByID(context.Background(), gUser.ID)
	if after.PasswordHash != before.PasswordHash || after.PasswordHash.Valid || !after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) {
		t.Errorf("passwordless user row changed: before %+v after %+v", before.PasswordHash, after.PasswordHash)
	}

	// CLM-13: password sign-in to a passwordless account is invalid credentials.
	s.expect(http.StatusUnauthorized, "POST", "/api/v1/auth/login", "",
		map[string]string{"email": gUser.Email, "password": "correct-password-123"}, &e)
	if e.Error.Code != "invalid_credentials" {
		t.Errorf("passwordless login: code %q, want invalid_credentials", e.Error.Code)
	}
}

// CLM-10: a passwordless user deletes their account by typing their email
// from a recently created session; password users still need their password.
func TestAPI_DeleteAccountPasswordless(t *testing.T) {
	s := newTestServer(t)

	t.Run("matching email from a fresh session deletes", func(t *testing.T) {
		u := s.signUpPasswordless("delok")
		s.expect(http.StatusNoContent, "DELETE", "/api/v1/me", u.AccessToken,
			map[string]string{"email": "  " + strings.ToUpper(u.Email) + " "}, nil)
		if s.userExists(u.ID) {
			t.Error("user still exists after delete")
		}
	})

	t.Run("wrong email is refused", func(t *testing.T) {
		u := s.signUpPasswordless("delwrong")
		var e apiError
		s.expect(http.StatusUnprocessableEntity, "DELETE", "/api/v1/me", u.AccessToken,
			map[string]string{"email": "someone-else@example.com"}, &e)
		if e.Error.Fields["email"] == "" {
			t.Errorf("wrong email: expected email field error, got %+v", e)
		}
		s.expect(http.StatusUnprocessableEntity, "DELETE", "/api/v1/me", u.AccessToken,
			map[string]string{"password": "correct-password-123"}, nil)
		if !s.userExists(u.ID) {
			t.Error("user deleted despite wrong email")
		}
	})

	t.Run("stale session is refused with re-sign-in message", func(t *testing.T) {
		u := s.signUpPasswordless("delstale")
		if _, err := s.pool.Exec(context.Background(),
			"UPDATE refresh_tokens SET created_at = NOW() - INTERVAL '11 minutes' WHERE id = $1", u.SessionID); err != nil {
			t.Fatalf("backdate session: %v", err)
		}
		var e apiError
		s.expect(http.StatusUnprocessableEntity, "DELETE", "/api/v1/me", u.AccessToken,
			map[string]string{"email": u.Email}, &e)
		if !strings.Contains(e.Error.Fields["email"], "sign in with Google again") {
			t.Errorf("stale session: expected re-sign-in message, got %+v", e)
		}
		if !s.userExists(u.ID) {
			t.Error("user deleted from a stale session")
		}
	})

	t.Run("deletes the user's household", func(t *testing.T) {
		u := s.signUpPasswordless("deladmin")
		var h struct{ ID uuid.UUID }
		s.expect(http.StatusCreated, "POST", "/api/v1/households", u.AccessToken,
			map[string]string{"name": "Google Home", "timezone": "UTC"}, &h)
		memberID, err := uuid.NewV7()
		if err != nil {
			t.Fatalf("uuid: %v", err)
		}
		if _, err := s.pool.Exec(context.Background(),
			"INSERT INTO family_members (id, household_id, name) VALUES ($1, $2, $3)", memberID, h.ID, "Kid"); err != nil {
			t.Fatalf("insert family member: %v", err)
		}

		s.expect(http.StatusNoContent, "DELETE", "/api/v1/me", u.AccessToken,
			map[string]string{"email": u.Email}, nil)

		if s.userExists(u.ID) {
			t.Error("user still exists after delete")
		}

		var householdCount int
		if err := s.pool.QueryRow(context.Background(),
			"SELECT COUNT(*) FROM households WHERE id = $1", h.ID).Scan(&householdCount); err != nil {
			t.Fatalf("count households: %v", err)
		}
		if householdCount != 0 {
			t.Errorf("household still exists: count = %d, want 0", householdCount)
		}

		var memberCount int
		if err := s.pool.QueryRow(context.Background(),
			"SELECT COUNT(*) FROM family_members WHERE household_id = $1", h.ID).Scan(&memberCount); err != nil {
			t.Fatalf("count family members: %v", err)
		}
		if memberCount != 0 {
			t.Errorf("family members still exist: count = %d, want 0", memberCount)
		}
	})

	t.Run("password user still needs the correct password", func(t *testing.T) {
		u := s.signUp("delpw")
		for _, body := range []map[string]string{
			{"password": "wrong-password"},
			{"password": ""},
			{"email": "whatever@example.com"},
		} {
			var e apiError
			s.expect(http.StatusUnprocessableEntity, "DELETE", "/api/v1/me", u.AccessToken, body, &e)
			if e.Error.Fields["password"] != "is incorrect" {
				t.Errorf("password user %v: got %+v, want password 'is incorrect'", body, e)
			}
		}
		if !s.userExists(u.User.ID) {
			t.Error("password user deleted without the right password")
		}
	})
}

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

	// PIN budget: member create + quest create + one accept + one wrong
	// history PIN. History budget: three correct reads + the wrong one.
	const pinLimit = 4
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
