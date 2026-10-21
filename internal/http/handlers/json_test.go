package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/softsrv/chorus/internal/http/middleware"
)

func TestDecodeJSON(t *testing.T) {
	t.Parallel()

	type payload struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}

	cases := []struct {
		name        string
		contentType string
		body        string
		wantOK      bool
		wantStatus  int
	}{
		{"valid", "application/json", `{"name":"a","count":1}`, true, 0},
		{"charset param allowed", "application/json; charset=utf-8", `{"name":"a"}`, true, 0},
		{"wrong content type", "text/plain", `{"name":"a"}`, false, http.StatusUnsupportedMediaType},
		{"missing content type", "", `{"name":"a"}`, false, http.StatusUnsupportedMediaType},
		{"unknown field", "application/json", `{"name":"a","admin":true}`, false, http.StatusBadRequest},
		{"trailing data", "application/json", `{"name":"a"}{"name":"b"}`, false, http.StatusBadRequest},
		{"malformed", "application/json", `{"name":`, false, http.StatusBadRequest},
		{"empty body", "application/json", ``, false, http.StatusBadRequest},
		{"wrong type", "application/json", `{"count":"seven"}`, false, http.StatusUnprocessableEntity},
		{"too large", "application/json", `{"name":"` + strings.Repeat("x", 2048) + `"}`, false, http.StatusRequestEntityTooLarge},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rr := httptest.NewRecorder()
			var ok bool
			middleware.BodyLimit(1024)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var p payload
				ok = decodeJSON(w, r, &p)
			})).ServeHTTP(rr, req)

			if ok != tc.wantOK {
				t.Fatalf("ok: got %v, want %v (status %d, body %s)", ok, tc.wantOK, rr.Code, rr.Body)
			}
			if !tc.wantOK && rr.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d (body %s)", rr.Code, tc.wantStatus, rr.Body)
			}
		})
	}
}
