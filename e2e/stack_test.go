// Package e2e runs the GameStore walkthrough of section 4.1 against the real binaries:
// cmd/api, cmd/worker and cmd/pixsim as processes, a real Postgres, and nothing but
// HTTP between them and the test, which plays the merchant and the payer.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
)

const (
	pixsimSecret = "e2e-pixsim-webhook-secret"                //nolint:gosec // G101: exists only between these processes, for this test.
	signingKey   = "e2e-webhook-signing-key-0123456789abcdef" //nolint:gosec // G101: exists only between these processes, for this test.
	// providerTimeout is short so a hung provider call turns unknown quickly.
	providerTimeout = "1s"
	webhookTimeout  = "2s"
)

// bin holds the binaries TestMain builds once for every test.
var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cbp-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bin = dir
	build := exec.CommandContext(context.Background(), "go", "build", //nolint:gosec // G204: a fixed go build into a directory TestMain made.
		"-race", "-o", dir+string(os.PathSeparator), "./cmd/api", "./cmd/worker", "./cmd/pixsim", "./cmd/migrate", "./cmd/admin")
	build.Dir = ".."
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: build:", err)
		os.Exit(1)
	}
	code := pgtest.Main(m)
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

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

// process is one of the binaries, running. exited is closed once it has exited, with
// err its result, so any number of waiters can see it.
type process struct {
	name   string
	cmd    *exec.Cmd
	out    *syncBuffer
	exited chan struct{}
	err    error
}

// start runs a binary with only the environment given: nothing leaks in from the
// developer's shell or mise.toml. The process is killed when the test ends.
func start(t *testing.T, name string, env map[string]string, args ...string) *process {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), filepath.Join(bin, name), args...) //nolint:gosec // G204: one of the binaries TestMain just built.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	p := &process{name: name, cmd: cmd, out: &syncBuffer{}, exited: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p.out, p.out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	go func() {
		p.err = cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		<-p.exited
		if t.Failed() {
			t.Logf("%s log:\n%s", name, p.out.String())
		}
	})
	return p
}

// addr waits for the "<name> started" line and returns the address it listens on.
func (p *process) addr(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		sc := bufio.NewScanner(strings.NewReader(p.out.String()))
		for sc.Scan() {
			var line struct{ Msg, Addr string }
			if json.Unmarshal(sc.Bytes(), &line) == nil && line.Msg == p.name+" started" {
				return "http://" + line.Addr
			}
		}
		select {
		case <-p.exited:
			t.Fatalf("%s exited before listening: %v\n%s", p.name, p.err, p.out.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("%s not listening after 15 s:\n%s", p.name, p.out.String())
	return ""
}

func (p *process) waitLog(t *testing.T, substr string) {
	t.Helper()
	eventually(t, 15*time.Second, p.name+" logging "+substr, func() bool { return strings.Contains(p.out.String(), substr) })
}

// stop sends SIGTERM, as an orchestrator would, and wants a clean exit.
func (p *process) stop(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal %s: %v", p.name, err)
	}
	select {
	case <-p.exited:
		if p.err != nil {
			t.Errorf("%s exited with %v after SIGTERM:\n%s", p.name, p.err, p.out.String())
		}
	case <-time.After(15 * time.Second):
		t.Errorf("%s did not stop 15 s after SIGTERM", p.name)
	}
}

// run runs a binary to completion and returns its output.
func run(t *testing.T, name string, env map[string]string, args ...string) string {
	t.Helper()
	p := start(t, name, env, args...)
	select {
	case <-p.exited:
		if p.err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, p.err, p.out.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("%s %v still running after 30 s", name, args)
	}
	return p.out.String()
}

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not %s after %v", what, within)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stack is the gateway as deployed: the simulator, the API and the worker, each its
// own process, on a database migrated by cmd/migrate.
type stack struct {
	t        *testing.T
	pool     *pgxpool.Pool
	dbURL    string
	api      string
	pixsim   string
	charges  *chargeRecorder
	inbox    *eventRecorder
	procs    []*process
	merchant *merchantServer
}

func newStack(t *testing.T) *stack {
	t.Helper()
	pool := pgtest.NewEmptyPool(t)
	s := &stack{t: t, pool: pool, dbURL: pool.Config().ConnString()}
	run(t, "migrate", map[string]string{"DATABASE_URL": s.dbURL})

	// The simulator posts its events to the API, which starts after it: the recorder
	// forwards once the API is up, and keeps each event ID for redeliveries.
	s.inbox = newEventRecorder(t)
	pixsim := start(t, "pixsim", map[string]string{
		"PIXSIM_ADDR": "127.0.0.1:0", "PIXSIM_SEED": "7",
		"PIXSIM_WEBHOOK_URL": s.inbox.URL + "/v1/providers/pixsim/events", "PIXSIM_WEBHOOK_SECRET": pixsimSecret,
	})
	s.pixsim = pixsim.addr(t)
	// The gateway reaches the simulator through a recorder that notes each charge's
	// ID by its BR Code, which is what a payer holding the code needs to pay it.
	s.charges = newChargeRecorder(t, s.pixsim)

	common := map[string]string{
		"DATABASE_URL": s.dbURL, "PIXSIM_URL": s.charges.URL, "PIXSIM_WEBHOOK_SECRET": pixsimSecret,
		"WEBHOOK_SIGNING_KEY": signingKey, "WEBHOOK_ALLOW_PRIVATE": "true", "WEBHOOK_TIMEOUT": webhookTimeout,
		"PROVIDER_TIMEOUT": providerTimeout, "SHUTDOWN_TIMEOUT": "5s", "LOG_LEVEL": "debug",
	}
	apiEnv := map[string]string{"HTTP_ADDR": "127.0.0.1:0"}
	for k, v := range common {
		apiEnv[k] = v
	}
	api := start(t, "api", apiEnv)
	s.api = api.addr(t)
	s.inbox.forwardTo(s.api)
	worker := start(t, "worker", common)
	worker.waitLog(t, `"job":"webhooks"`)
	s.procs = []*process{api, worker, pixsim}
	s.merchant = newMerchantServer(t)
	return s
}

// stop shuts every process down with SIGTERM and wants each to exit cleanly.
func (s *stack) stop() {
	for _, p := range s.procs {
		p.stop(s.t)
	}
}

// createMerchant runs cmd/admin, as an operator does, and returns the API key it
// prints once.
func (s *stack) createMerchant(name string) string {
	s.t.Helper()
	out := run(s.t, "admin", map[string]string{"DATABASE_URL": s.dbURL}, "merchant", "create", "-name", name, "-mode", "test")
	m := regexp.MustCompile(`api key\s+(sk_test_\S+)`).FindStringSubmatch(out)
	if m == nil {
		s.t.Fatalf("no API key in the admin's output:\n%s", out)
	}
	return m[1]
}

// chargeRecorder sits between the gateway and the simulator and notes every charge it
// sees in an answer, creations and lookups alike.
type chargeRecorder struct {
	*httptest.Server
	mu       sync.Mutex
	byBRCode map[string]string
}

func newChargeRecorder(t *testing.T, target string) *chargeRecorder {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	c := &chargeRecorder{byBRCode: map[string]string{}}
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.ModifyResponse = func(resp *http.Response) error {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(body))
		var ch struct {
			ID     string `json:"id"`
			BRCode string `json:"br_code"`
		}
		if json.Unmarshal(body, &ch) == nil && ch.ID != "" && ch.BRCode != "" {
			c.mu.Lock()
			c.byBRCode[ch.BRCode] = ch.ID
			c.mu.Unlock()
		}
		return nil
	}
	c.Server = httptest.NewServer(proxy)
	t.Cleanup(c.Close)
	return c
}

func (c *chargeRecorder) charge(brCode string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byBRCode[brCode]
}

// eventRecorder sits between the simulator and the API's inbox and keeps the ID of
// every event the simulator sent.
type eventRecorder struct {
	*httptest.Server
	target atomic.Pointer[httputil.ReverseProxy]
	mu     sync.Mutex
	ids    []string
}

func newEventRecorder(t *testing.T) *eventRecorder {
	t.Helper()
	r := &eventRecorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var e struct{ ID string }
		if json.Unmarshal(body, &e) == nil && e.ID != "" {
			r.mu.Lock()
			r.ids = append(r.ids, e.ID)
			r.mu.Unlock()
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		proxy := r.target.Load()
		if proxy == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, req)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *eventRecorder) forwardTo(target string) {
	u, err := url.Parse(target)
	if err != nil {
		panic(err)
	}
	r.target.Store(httputil.NewSingleHostReverseProxy(u))
}

func (r *eventRecorder) sent() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

// do sends a request with the key, if any, and returns the status and body.
func (s *stack) do(method, rawURL, key string, body any, header ...string) (status int, h http.Header, b []byte) {
	s.t.Helper()
	var r io.Reader = http.NoBody
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			s.t.Fatal(err)
		}
		r = bytes.NewReader(encoded)
	}
	ctx, cancel := context.WithTimeout(s.t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, rawURL, r)
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err = io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, b
}
