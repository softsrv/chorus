//go:build systest

package systest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSystem_FailsSafely tests that the running app handles failure modes safely:
// 1. Unreachable database: reports /health 200 {status:ok} and /ready 503 {status:unavailable}.
// 2. Bad configuration: exits non-zero within bounded deadline, logs "configuration error",
//    and never accepts connections on its port.
// 3. Graceful shutdown: in-flight request finishes, exits 0, and logs "shutdown complete".
func TestSystem_FailsSafely(t *testing.T) {
	t.Run("unreachable database", func(t *testing.T) {
		// Pick an unused port by listening on loopback and immediately closing it.
		closedPort := freePort(t)
		badDBURL := "postgres://chorus:chorus@127.0.0.1:" + closedPort + "/chorus_test?sslmode=disable"

		// Start app overriding DATABASE_URL to unreachable host/port.
		// waitHealthy polls /health which answers 200 ok even without DB.
		app := startApp(t, appOpts{
			Env: map[string]string{
				"DATABASE_URL": badDBURL,
			},
		})

		var health struct {
			Status string `json:"status"`
		}
		app.do("GET", "/health", "", nil).expect(t, "GET /health with unreachable db", http.StatusOK).decode(t, &health)
		if health.Status != "ok" {
			t.Fatalf("GET /health status = %q, want ok", health.Status)
		}

		var ready struct {
			Status string `json:"status"`
		}
		app.do("GET", "/ready", "", nil).expect(t, "GET /ready with unreachable db", http.StatusServiceUnavailable).decode(t, &ready)
		if ready.Status != "unavailable" {
			t.Fatalf("GET /ready status = %q, want unavailable", ready.Status)
		}
	})

	t.Run("bad configuration", func(t *testing.T) {
		tests := []struct {
			name string
			env  map[string]string
		}{
			{
				name: "missing DATABASE_URL",
				env:  map[string]string{"DATABASE_URL": ""},
			},
			{
				name: "short JWT_SECRET 31 bytes",
				env:  map[string]string{"JWT_SECRET": "1234567890123456789012345678901"},
			},
			{
				name: "missing SMTP_HOST",
				env:  map[string]string{"SMTP_HOST": ""},
			},
			{
				name: "missing SMTP_PORT",
				env:  map[string]string{"SMTP_PORT": ""},
			},
			{
				name: "missing SMTP_FROM_EMAIL",
				env:  map[string]string{"SMTP_FROM_EMAIL": ""},
			},
			{
				name: "missing APP_BASE_URL",
				env:  map[string]string{"APP_BASE_URL": ""},
			},
			{
				name: "invalid GOOGLE_TOKEN_KEY",
				env:  map[string]string{"GOOGLE_TOKEN_KEY": "not-valid-base64-or-length"},
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				app := startApp(t, appOpts{
					Env:    tc.env,
					NoWait: true,
				})

				code, err := app.Wait(5 * time.Second)
				if err != nil {
					t.Fatalf("app did not exit within timeout: %v", err)
				}
				if code == 0 {
					t.Fatalf("expected non-zero exit code, got 0")
				}

				out := app.Output()
				if !strings.Contains(out, "configuration error") {
					t.Fatalf("expected output to contain %q, got:\n%s", "configuration error", out)
				}

				// Verify port never accepted connections
				_, dialErr := net.DialTimeout("tcp", strings.TrimPrefix(app.BaseURL, "http://"), 200*time.Millisecond)
				if dialErr == nil {
					t.Fatalf("expected connection to %s to be refused, but connected successfully", app.BaseURL)
				}
			})
		}
	})

	t.Run("SIGTERM drain in-flight request", func(t *testing.T) {
		app := startApp(t, appOpts{})

		// Start a delayed-body POST request to keep a request demonstrably in flight.
		// /api/v1/auth/register parses JSON body with decodeJSON.
		// By feeding body chunks with a delay, the handler is actively reading when SIGTERM arrives.
		pipeR, pipeW := io.Pipe()

		type delayedResult struct {
			resp response
			err  error
		}
		respCh := make(chan delayedResult, 1)

		go func() {
			req, err := http.NewRequest("POST", app.BaseURL+"/api/v1/auth/register", pipeR)
			if err != nil {
				respCh <- delayedResult{err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			// Use an HTTP client with longer timeout than the drain delay
			resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
			if err != nil {
				respCh <- delayedResult{err: err}
				return
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				respCh <- delayedResult{err: err}
				return
			}
			respCh <- delayedResult{resp: response{Status: resp.StatusCode, Header: resp.Header, Body: b}}
		}()

		// Send initial JSON chunk
		email := uniqueEmail(t)
		bodyPrefix := `{"email":"` + email + `","password":"` + fakePassword + `",`
		if _, err := pipeW.Write([]byte(bodyPrefix)); err != nil {
			t.Fatalf("write prefix to pipe: %v", err)
		}

		// Wait briefly so server starts reading the request
		time.Sleep(100 * time.Millisecond)

		// Send SIGTERM to the app process
		if err := app.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("signal SIGTERM: %v", err)
		}

		// Sleep briefly while request is still in flight under shutdown
		time.Sleep(200 * time.Millisecond)

		// Finish sending the body
		bodySuffix := `"display_name":"Systest User"}`
		if _, err := pipeW.Write([]byte(bodySuffix)); err != nil {
			t.Fatalf("write suffix to pipe: %v", err)
		}
		_ = pipeW.Close()

		// Wait for response with timeout
		var res delayedResult
		select {
		case res = <-respCh:
		case <-time.After(15 * time.Second):
			t.Fatal("timed out waiting for in-flight request to finish")
		}

		if res.err != nil {
			t.Fatalf("in-flight request failed: %v", res.err)
		}
		res.resp.expect(t, "in-flight register request", http.StatusAccepted)

		// Wait for process to exit
		code, err := app.Wait(35 * time.Second)
		if err != nil {
			t.Fatalf("process did not exit after SIGTERM: %v", err)
		}
		if code != 0 {
			t.Fatalf("process exited with code %d, want 0", code)
		}

		out := app.Output()
		if !strings.Contains(out, "shutdown complete") {
			t.Fatalf("expected output to contain %q, got:\n%s", "shutdown complete", out)
		}
	})
}
