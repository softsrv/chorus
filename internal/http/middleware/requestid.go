package middleware

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// requestIDKeyType is an unexported struct used as the context key for request IDs.
// Using a distinct struct type (rather than a typed string) makes collisions with
// other packages impossible, even if they use the same underlying string value.
type requestIDKeyType struct{}

var requestIDKey = requestIDKeyType{}

// RequestID attaches a unique request ID to the context and response header.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.NewV7()
		if err != nil {
			id = uuid.New() // fallback to v4
		}
		requestID := id.String()

		ctx := context.WithValue(r.Context(), requestIDKey, requestID)
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetRequestID retrieves the request ID from context.
func GetRequestID(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}
