package testnet_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/softsrv/chorus/internal/testnet"
)

func TestRefuseExternal(t *testing.T) {
	origTransport := http.DefaultTransport

	restore := testnet.RefuseExternal()
	defer func() {
		restore()
		if http.DefaultTransport != origTransport {
			t.Errorf("cleanup: expected http.DefaultTransport to be restored to original")
		}
	}()

	if http.DefaultTransport == origTransport {
		t.Fatal("expected http.DefaultTransport to be replaced by a clone")
	}

	client := &http.Client{
		Timeout: 2 * time.Second,
	}

	// 1. Dials to non-loopback addresses must fail immediately with an error naming the host.
	testCases := []struct {
		url          string
		expectedHost string
	}{
		{url: "http://192.0.2.1:8080/test", expectedHost: "192.0.2.1"},
		{url: "http://example.com/api", expectedHost: "example.com"},
		{url: "http://10.0.0.1:9000", expectedHost: "10.0.0.1"},
	}

	for _, tc := range testCases {
		start := time.Now()
		resp, err := client.Get(tc.url)
		elapsed := time.Since(start)

		if err == nil {
			resp.Body.Close()
			t.Fatalf("expected error dialing non-loopback URL %s, got response %d", tc.url, resp.StatusCode)
		}

		if elapsed > 500*time.Millisecond {
			t.Errorf("dial to %s took %v, expected immediate refusal", tc.url, elapsed)
		}

		if !strings.Contains(err.Error(), tc.expectedHost) {
			t.Errorf("expected error to contain host %q, got: %v", tc.expectedHost, err)
		}

		if !errors.Is(err, testnet.ErrExternalDial) {
			t.Errorf("expected error to wrap ErrExternalDial, got: %v", err)
		}
	}

	// 2. Loopback server requests must succeed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("expected loopback request to succeed, got error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK from loopback server, got %d", resp.StatusCode)
	}

	// 3. Restore reinstates the original transport.
	restore()
	if http.DefaultTransport != origTransport {
		t.Fatalf("expected http.DefaultTransport to be restored to original")
	}
}
