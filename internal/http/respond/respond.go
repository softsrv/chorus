// Package respond writes JSON API responses. Every error the API returns has
// the same envelope so clients can handle failures uniformly:
//
//	{"error": {"code": "validation_failed", "message": "...", "fields": {"title": "is required"}}}
//
// code is a stable machine-readable identifier; message is for humans and may
// change; fields is present only for validation errors.
package respond

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ErrorBody is the JSON error envelope.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail describes a single API error.
type ErrorDetail struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// JSON writes v as a JSON response with the given status code.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Header is already written; nothing more we can tell the client.
		slog.Error("encode json response", "error", err)
	}
}

// NoContent writes a 204 with no body.
func NoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// Error writes a JSON error envelope.
func Error(w http.ResponseWriter, status int, code, message string) {
	JSON(w, status, ErrorBody{Error: ErrorDetail{Code: code, Message: message}})
}

// ValidationError writes a 422 with per-field messages.
func ValidationError(w http.ResponseWriter, fields map[string]string) {
	JSON(w, http.StatusUnprocessableEntity, ErrorBody{Error: ErrorDetail{
		Code:    "validation_failed",
		Message: "One or more fields are invalid.",
		Fields:  fields,
	}})
}
