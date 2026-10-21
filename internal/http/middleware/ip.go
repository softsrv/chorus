package middleware

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP returns the client's IP address as a string.
//
// When trustedProxies > 0 and an X-Forwarded-For header is present, it trusts
// the leftmost (client) entry. Otherwise it falls back to the connection's
// RemoteAddr with the port stripped. Pass trustedProxies = 0 when the service
// is exposed directly to the internet so spoofed X-Forwarded-For headers from
// clients are ignored.
//
// This is the single source of truth for client-IP extraction, used by both the
// rate limiters and request device-metadata capture.
func ClientIP(r *http.Request, trustedProxies int) string {
	if trustedProxies > 0 {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			return firstForwardedIP(fwd)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// firstForwardedIP returns the leftmost (client) IP from an X-Forwarded-For
// value, which may be a comma-separated list: "client, proxy1, proxy2".
func firstForwardedIP(xff string) string {
	first, _, _ := strings.Cut(xff, ",")
	return strings.TrimSpace(first)
}
