package testnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// ErrExternalDial is returned when a dial to a non-loopback host is attempted while RefuseExternal is active.
var ErrExternalDial = errors.New("testnet: external network dial refused")

// RefuseExternal replaces http.DefaultTransport with a clone whose DialContext returns
// an error naming the host for any non-loopback address, before any network traffic,
// and still dials loopback addresses normally.
// It returns a restore func that reinstates the original transport.
func RefuseExternal() func() {
	orig := http.DefaultTransport

	var clone *http.Transport
	if t, ok := orig.(*http.Transport); ok {
		clone = t.Clone()
	} else {
		clone = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}

	origDialContext := clone.DialContext
	if origDialContext == nil {
		origDialContext = (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
	}

	clone.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")

		if !isLoopback(host) {
			return nil, fmt.Errorf("%w to %s (only loopback dials permitted)", ErrExternalDial, host)
		}

		return origDialContext(ctx, network, addr)
	}

	if clone.DialTLSContext != nil {
		origDialTLSContext := clone.DialTLSContext
		clone.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}
			host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")

			if !isLoopback(host) {
				return nil, fmt.Errorf("%w to %s (only loopback dials permitted)", ErrExternalDial, host)
			}

			return origDialTLSContext(ctx, network, addr)
		}
	}

	http.DefaultTransport = clone

	return func() {
		http.DefaultTransport = orig
	}
}

// isLoopback reports whether host is a loopback IP or loopback hostname.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}
