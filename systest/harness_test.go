//go:build systest

package systest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Obviously fake fixture values (see .gitleaks.toml and CLAUDE.md).
const (
	fakeJWTSecret = "systest-fake-jwt-secret-not-a-real-key-0000"
	fakePassword  = "systest-fake-password"

	healthDeadline = 30 * time.Second
	mailDeadline   = 15 * time.Second
	stopDeadline   = 40 * time.Second // the app drains for up to 30s
	dockerRmLimit  = 20 * time.Second
	requestTimeout = 15 * time.Second
)

// appOpts configures one app process.
type appOpts struct {
	// Env overrides the base environment. A key mapped to "" is removed, so
	// a test can start the app with a required setting missing.
	Env map[string]string
	// NoWait returns as soon as the process starts, without polling /health
	// (for starts that are expected to fail).
	NoWait bool
}

// appProc is one running app: the binary TestMain built, or a `docker run`
// of SYSTEST_IMAGE. Either way it is one *exec.Cmd whose stdout and stderr
// are captured together in out.
type appProc struct {
	t       *testing.T
	cmd     *exec.Cmd
	BaseURL string
	Mail    *smtpCapture
	out     *syncBuffer
	done    chan struct{} // closed when cmd.Wait returns
	waitErr error
	name    string // container name in image mode
}

// startApp starts the app for this test function only; no process is shared
// at package level, so each test gets the app's fresh in-memory rate limiters.
// It runs with an environment built explicitly here: the listed settings, a
// capture SMTP server of its own, and no GOOGLE_* variable.
func startApp(t *testing.T, opts appOpts) *appProc {
	t.Helper()
	if skipReason != "" {
		t.Skip(skipReason)
	}

	mail := newSMTPCapture(t)
	port := freePort(t)
	base := "http://127.0.0.1:" + port

	env := map[string]string{
		"PORT":            port,
		"APP_BASE_URL":    base,
		"DATABASE_URL":    os.Getenv("TEST_DATABASE_URL"),
		"JWT_SECRET":      fakeJWTSecret,
		"BCRYPT_COST":     "10",
		"SMTP_HOST":       "127.0.0.1",
		"SMTP_PORT":       mail.Port,
		"SMTP_FROM_EMAIL": "noreply@example.test",
	}
	for k, v := range opts.Env {
		if v == "" {
			delete(env, k)
		} else {
			env[k] = v
		}
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	p := &appProc{t: t, BaseURL: base, Mail: mail, out: &syncBuffer{}, done: make(chan struct{})}

	if image := os.Getenv("SYSTEST_IMAGE"); image != "" {
		p.name = "chorus-systest-" + randHex(t, 6)
		// Values travel in cmd.Env and are passed by name only, never argv.
		args := []string{"run", "--rm", "--network", "host", "--name", p.name}
		for _, k := range keys {
			args = append(args, "-e", k)
		}
		args = append(args, image)
		p.cmd = exec.Command("docker", args...)
		p.cmd.Env = dockerClientEnv(env)
	} else {
		if appBinary == "" {
			t.Fatal("startApp: TestMain built no app binary")
		}
		p.cmd = exec.Command(appBinary)
		// An empty working directory: web assets must come from the embed,
		// never from a web/static tree on disk.
		p.cmd.Dir = t.TempDir()
		p.cmd.Env = make([]string, 0, len(keys))
		for _, k := range keys {
			p.cmd.Env = append(p.cmd.Env, k+"="+env[k])
		}
	}
	p.cmd.Stdout = p.out
	p.cmd.Stderr = p.out

	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start app: %v", err)
	}
	go func() {
		p.waitErr = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(p.stop)

	if !opts.NoWait {
		p.waitHealthy()
	}
	return p
}

// dockerClientEnv is the docker CLI's own environment (PATH, DOCKER_HOST,
// HOME) without any app setting, plus exactly the app settings for this run.
func dockerClientEnv(app map[string]string) []string {
	appKey := func(k string) bool {
		_, ok := app[k]
		return ok || strings.HasPrefix(k, "GOOGLE_")
	}
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !appKey(k) {
			out = append(out, kv)
		}
	}
	for k, v := range app {
		out = append(out, k+"="+v)
	}
	return out
}

// waitHealthy polls GET /health until it answers 200 {status:ok}, failing the
// test if the process exits first or the deadline passes.
func (p *appProc) waitHealthy() {
	p.t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(healthDeadline)
	var last string
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			p.t.Fatalf("app exited before becoming healthy (%v); output:\n%s", p.waitErr, p.Output())
		default:
		}
		resp, err := client.Get(p.BaseURL + "/health")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var v struct {
				Status string `json:"status"`
			}
			if resp.StatusCode == http.StatusOK && json.Unmarshal(body, &v) == nil && v.Status == "ok" {
				return
			}
			last = fmt.Sprintf("status %d body %s", resp.StatusCode, body)
		} else {
			last = err.Error()
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.t.Fatalf("app not healthy within %s (last: %s); output:\n%s", healthDeadline, last, p.Output())
}

// Output is everything the app wrote to stdout and stderr so far.
func (p *appProc) Output() string { return p.out.String() }

// Signal sends sig to the process (in image mode the docker CLI forwards it
// to the container).
func (p *appProc) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }

// Wait waits up to timeout for the process to exit and returns its exit code.
func (p *appProc) Wait(timeout time.Duration) (int, error) {
	select {
	case <-p.done:
		return p.cmd.ProcessState.ExitCode(), nil
	case <-time.After(timeout):
		return -1, fmt.Errorf("app did not exit within %s", timeout)
	}
}

// stop ends the process at cleanup: SIGTERM, a bounded wait, then a kill;
// in image mode the container is removed with a bounded `docker rm -f`.
func (p *appProc) stop() {
	select {
	case <-p.done:
	default:
		_ = p.Signal(syscall.SIGTERM)
		if _, err := p.Wait(stopDeadline); err != nil {
			p.t.Errorf("stop app: %v; killing it", err)
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	}
	if p.name != "" {
		ctx, cancel := context.WithTimeout(context.Background(), dockerRmLimit)
		defer cancel()
		// The container is normally gone already (--rm); "No such container"
		// is expected, so only a timeout is reported.
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", p.name).Run()
		if ctx.Err() != nil {
			p.t.Errorf("docker rm -f %s: %v", p.name, ctx.Err())
		}
	}
	if p.t.Failed() {
		p.t.Logf("app output:\n%s", p.Output())
	}
}

// ── HTTP ──────────────────────────────────────────────────────────────────

// response is one HTTP exchange's result.
type response struct {
	Status int
	Header http.Header
	Body   []byte
}

// do sends a request to the app. body, when non-nil, is sent as JSON; token,
// when set, as a bearer token.
func (p *appProc) do(method, path, token string, body any) response {
	p.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			p.t.Fatalf("marshal %s %s body: %v", method, path, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, p.BaseURL+path, rd)
	if err != nil {
		p.t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: requestTimeout}).Do(req)
	if err != nil {
		p.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		p.t.Fatalf("read %s %s: %v", method, path, err)
	}
	return response{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

// expect fails the test unless r has the wanted status.
func (r response) expect(t *testing.T, what string, want int) response {
	t.Helper()
	if r.Status != want {
		t.Fatalf("%s: status %d, want %d; body: %s", what, r.Status, want, r.Body)
	}
	return r
}

// decode unmarshals r's JSON body into v.
func (r response) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("decode %s: %v", r.Body, err)
	}
}

// tokens is the token pair register-verify and login answer with.
type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	User         struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

// signUp registers email, takes the verification token from the captured
// mail and verifies it, returning the verified user's token pair. Each call
// spends one of the app's 5 sign-ups per hour; reuse the user it returns.
func (p *appProc) signUp(t *testing.T, email string) tokens {
	t.Helper()
	p.do("POST", "/api/v1/auth/register", "", map[string]string{
		"email": email, "password": fakePassword, "display_name": "Systest User",
	}).expect(t, "register", http.StatusAccepted)

	token := verificationToken(t, p.Mail.waitFor(t, email))

	var tk tokens
	p.do("POST", "/api/v1/auth/verify-email", "", map[string]string{"token": token}).
		expect(t, "verify-email", http.StatusOK).decode(t, &tk)
	if tk.AccessToken == "" || tk.RefreshToken == "" {
		t.Fatalf("verify-email returned no token pair: %+v", tk)
	}
	return tk
}

var verifyLink = regexp.MustCompile(`/verify-email\?token=([A-Za-z0-9_\-]+)`)

// verificationToken extracts the token from a verification email.
func verificationToken(t *testing.T, msg string) string {
	t.Helper()
	m := verifyLink.FindStringSubmatch(msg)
	if m == nil {
		t.Fatalf("no verification link in mail:\n%s", msg)
	}
	return m[1]
}

// uniqueEmail is an address no other run uses: the test's name plus a random
// suffix, at the reserved example.test domain.
func uniqueEmail(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, r := range strings.ToLower(t.Name()) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	name := b.String()
	if len(name) > 40 {
		name = name[:40]
	}
	return name + "-" + randHex(t, 6) + "@example.test"
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}

// freePort returns a loopback port nothing listens on right now.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// syncBuffer is a bytes.Buffer safe for the process's writes and the test's
// reads at once.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ── SMTP capture ──────────────────────────────────────────────────────────

// capturedMail is one message the app delivered.
type capturedMail struct {
	To   []string
	Data string
}

// smtpCapture is a minimal loopback SMTP server (EHLO/HELO, MAIL, RCPT, DATA,
// RSET, NOOP, QUIT, no auth, no STARTTLS) that keeps every message, so the
// app's net/smtp delivery is real.
type smtpCapture struct {
	Port string
	l    net.Listener
	mu   sync.Mutex
	msgs []capturedMail
	wg   sync.WaitGroup
}

func newSMTPCapture(t *testing.T) *smtpCapture {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("smtp capture listen: %v", err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	s := &smtpCapture{Port: port, l: l}
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(func() {
		_ = l.Close()
		s.wg.Wait()
	})
	return s
}

func (s *smtpCapture) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.l.Accept()
		if err != nil {
			return // listener closed
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(conn)
		}()
	}
}

func (s *smtpCapture) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(mailDeadline))
	r := bufio.NewReader(conn)
	reply := func(line string) bool {
		_, err := io.WriteString(conn, line+"\r\n")
		return err == nil
	}
	if !reply("220 systest ESMTP capture") {
		return
	}
	var cur capturedMail
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			if !reply("250-systest") || !reply("250 8BITMIME") {
				return
			}
		case strings.HasPrefix(cmd, "HELO"):
			reply("250 systest")
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			cur = capturedMail{}
			reply("250 OK")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			addr := strings.TrimSpace(strings.TrimSpace(line)[len("RCPT TO:"):])
			cur.To = append(cur.To, strings.Trim(addr, "<>"))
			reply("250 OK")
		case cmd == "DATA":
			if !reply("354 end with <CRLF>.<CRLF>") {
				return
			}
			var data strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(l, "\r\n") == "." {
					break
				}
				data.WriteString(strings.TrimPrefix(l, ".")) // dot-unstuffing
			}
			cur.Data = data.String()
			s.mu.Lock()
			s.msgs = append(s.msgs, cur)
			s.mu.Unlock()
			cur = capturedMail{}
			reply("250 OK queued")
		case cmd == "RSET":
			cur = capturedMail{}
			reply("250 OK")
		case cmd == "NOOP":
			reply("250 OK")
		case cmd == "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 command not implemented")
		}
	}
}

// waitFor returns the first message delivered to addr, failing the test if
// none arrives within mailDeadline.
func (s *smtpCapture) waitFor(t *testing.T, addr string) string {
	t.Helper()
	deadline := time.Now().Add(mailDeadline)
	for time.Now().Before(deadline) {
		if msg, ok := s.find(addr); ok {
			return msg
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no mail to %s within %s", addr, mailDeadline)
	return ""
}

func (s *smtpCapture) find(addr string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.msgs {
		for _, to := range m.To {
			if strings.EqualFold(to, addr) {
				return m.Data, true
			}
		}
	}
	return "", false
}
