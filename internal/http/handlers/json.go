package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/http/middleware"
	"github.com/softsrv/chorus/internal/http/respond"
)

// decodeJSON strictly decodes the request body into dst. It requires an
// application/json content type, rejects unknown fields and trailing data, and
// writes the appropriate error response itself. It returns false if the
// handler should stop.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		respond.Error(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json.")
		return false
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeDecodeError(w, err)
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		respond.Error(w, http.StatusBadRequest, "invalid_json", "Request body must contain a single JSON object.")
		return false
	}
	return true
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var (
		syntaxErr  *json.SyntaxError
		typeErr    *json.UnmarshalTypeError
		maxErr     *http.MaxBytesError
		timeErr    *time.ParseError
		invalidErr *json.InvalidUnmarshalError
	)
	switch {
	case errors.As(err, &maxErr):
		respond.Error(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large.")
	case errors.As(err, &typeErr) && typeErr.Field != "":
		respond.ValidationError(w, map[string]string{typeErr.Field: "has the wrong type"})
	case errors.As(err, &timeErr):
		respond.Error(w, http.StatusBadRequest, "invalid_json", "Timestamps must be RFC 3339, e.g. 2026-01-02T15:04:05Z.")
	case errors.As(err, &invalidErr):
		// Programming error, not a client error.
		slog.Error("decode json: invalid target", "error", err)
		respond.Error(w, http.StatusInternalServerError, "internal_error", "Something went wrong.")
	case errors.As(err, &syntaxErr), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		respond.Error(w, http.StatusBadRequest, "invalid_json", "Request body is not valid JSON.")
	default:
		// Covers DisallowUnknownFields ("json: unknown field \"x\"") and other
		// decoder errors. The message names only the field, which the client sent.
		respond.Error(w, http.StatusBadRequest, "invalid_json", err.Error())
	}
}

// writeError maps a service-layer error to an HTTP response. Unexpected errors
// are logged and reported as a generic 500 so internals never leak to clients.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		vErr  *app.ValidationError
		rlErr app.RateLimitedError
		hbErr app.HouseholdBlockingError
	)
	switch {
	case errors.As(err, &vErr):
		respond.ValidationError(w, vErr.Fields)
	case errors.As(err, &rlErr):
		w.Header().Set("Retry-After", strconv.Itoa(int(time.Until(rlErr.RetryAt).Seconds())+1))
		respond.Error(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Try again later.")
	case errors.As(err, &hbErr):
		respond.Error(w, http.StatusConflict, "sole_admin", hbErr.Error()+". Promote another admin first.")
	case errors.Is(err, app.ErrNotFound), errors.Is(err, app.ErrUserNotFound):
		respond.Error(w, http.StatusNotFound, "not_found", "Not found.")
	case errors.Is(err, app.ErrForbidden):
		respond.Error(w, http.StatusForbidden, "forbidden", "You don't have permission to do that.")
	case errors.Is(err, app.ErrInvalidCredentials):
		respond.Error(w, http.StatusUnauthorized, "invalid_credentials", "Invalid email or password.")
	case errors.Is(err, app.ErrAccountLocked):
		respond.Error(w, http.StatusLocked, "account_locked", "Account is temporarily locked. Try again later.")
	case errors.Is(err, app.ErrTokenNotFound), errors.Is(err, app.ErrTokenExpired),
		errors.Is(err, app.ErrTokenUsed), errors.Is(err, app.ErrTokenRevoked), errors.Is(err, app.ErrTokenInvalid):
		respond.Error(w, http.StatusBadRequest, "invalid_token", "This link or token is invalid or has expired.")
	case errors.Is(err, app.ErrEmailAlreadyVerified):
		respond.Error(w, http.StatusConflict, "already_verified", "Email is already verified.")
	case errors.Is(err, app.ErrLastAdmin):
		respond.Error(w, http.StatusConflict, "last_admin", "A household with members must keep at least one admin.")
	case errors.Is(err, app.ErrAlreadyMember):
		respond.Error(w, http.StatusConflict, "already_member", "You're already a member of this household.")
	case errors.Is(err, app.ErrGoogleUnavailable):
		w.Header().Set("Retry-After", "30")
		respond.Error(w, http.StatusServiceUnavailable, "google_unavailable", "Google Calendar is temporarily unavailable. Try again shortly.")
	case errors.Is(err, app.ErrInviteInvalid):
		respond.Error(w, http.StatusBadRequest, "invalid_invite", "Invite code is invalid or has expired.")
	default:
		slog.ErrorContext(r.Context(), "unhandled error",
			"request_id", middleware.GetRequestID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"error", err,
		)
		respond.Error(w, http.StatusInternalServerError, "internal_error", "Something went wrong.")
	}
}

// pathUUID parses a UUID path parameter. Malformed IDs get a 404, the same as
// a well-formed ID that doesn't exist.
func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		respond.Error(w, http.StatusNotFound, "not_found", "Not found.")
		return uuid.UUID{}, false
	}
	return id, true
}

// queryInt parses an optional integer query parameter, falling back to def.
func queryInt(r *http.Request, name string, def int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, &app.ValidationError{Fields: map[string]string{name: "must be an integer"}}
	}
	return n, nil
}

// queryTime parses a required RFC 3339 query parameter.
func queryTime(r *http.Request, name string) (time.Time, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}, &app.ValidationError{Fields: map[string]string{name: "is required"}}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, &app.ValidationError{Fields: map[string]string{name: "must be an RFC 3339 timestamp"}}
	}
	return t, nil
}

// currentUser returns the authenticated user. Routes using it must be wrapped
// in middleware.Authenticate; reaching it without a user is a wiring bug.
func currentUser(r *http.Request) uuid.UUID {
	u, ok := middleware.UserFromContext(r.Context())
	if !ok {
		panic(fmt.Sprintf("handler for %s %s reached without authentication", r.Method, r.URL.Path))
	}
	return u.ID
}
