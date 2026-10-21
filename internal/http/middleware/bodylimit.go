package middleware

import "net/http"

// DefaultMaxBodyBytes is the default request body ceiling (1 MiB). The auth
// flows only ever read small form posts, so this is generous while still
// preventing trivial memory-pressure DoS from oversized bodies.
const DefaultMaxBodyBytes int64 = 1 << 20

// BodyLimit caps the number of bytes that can be read from each request body.
// Reads past the limit fail with http.MaxBytesError, which ParseForm surfaces as
// an error — so handlers that already check ParseForm's error reject oversized
// bodies for free. GET/HEAD requests carry no body and are unaffected.
func BodyLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}
