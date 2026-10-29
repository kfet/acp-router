package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	kitlog "github.com/kfet/acp-kit/log"

	"github.com/kfet/acp-router/internal/backend"
	"github.com/kfet/acp-router/internal/config"
	"github.com/kfet/acp-router/internal/origin"
	"github.com/kfet/acp-router/internal/policy"
	"github.com/kfet/acp-router/internal/proxy"
	"github.com/kfet/acp-router/internal/testagent"
)

func TestMain(m *testing.M) {
	if os.Getenv("TESTAGENT_HELPER") == "1" {
		a := testagent.Serve("p", nil, os.Stdin, os.Stdout)
		<-a.Conn.Done()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func writeConfig(t *testing.T, dir string, extra string) string {
	t.Helper()
	cmd, _ := json.Marshal([]string{os.Args[0], "-test.run=^$"})
	p := filepath.Join(dir, "config.json")
	body := `{"state_dir":"` + filepath.Join(dir, "state") + `",` + extra +
		`"backend":{"command":` + string(cmd) + `,"env":{"TESTAGENT_HELPER":"1"}},
	  "policy":{"allow":["*"]},
	  "users":{"slack:T:alice":{"env":{"SECRET":"alice-key"}}}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func noop(context.Context, string, json.RawMessage) (any, *acp.RequestError) { return nil, nil }

func TestRunEndToEnd(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "logs", "audit.jsonl")
	cfg := writeConfig(t, dir, `"audit_log":"`+auditPath+`",`)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan int)
	go func() {
		code := Run([]string{"-config", cfg}, inR, outW, &stderr, "test")
		outW.Close()
		done <- code
	}()
	c := acp.NewConnection(noop, inW, outR)
	ctx := context.Background()
	if _, err := acp.SendRequest[json.RawMessage](c, ctx, "initialize", map[string]any{"protocolVersion": 1}); err != nil {
		t.Fatal(err)
	}
	o := origin.Origin{Platform: "slack", TeamID: "T", UserID: "alice", ChannelID: "C", ThreadID: "t"}
	res, err := acp.SendRequest[map[string]any](c, ctx, "session/new", map[string]any{"cwd": "/", "mcpServers": []any{}, "_meta": o.Meta()})
	if err != nil {
		t.Fatal(err)
	}
	m := res["_meta"].(map[string]any)
	if m["secret"] != "alice-key" || m["cwd"] != filepath.Join(dir, "state", "users", "slack:T:alice", "threads", "C", "t") ||
		m["home"] != filepath.Join(dir, "state", "users", "slack:T:alice") {
		t.Fatal(m)
	}
	// Ownership: bob cannot prompt alice's session.
	bob := o
	bob.UserID = "bob"
	_, err = acp.SendRequest[map[string]any](c, ctx, "session/prompt", map[string]any{
		"sessionId": res["sessionId"], "prompt": []acp.ContentBlock{acp.TextBlock("hi")}, "_meta": bob.Meta(),
	})
	var re *acp.RequestError
	if !errors.As(err, &re) || re.Code != proxy.CodeForbidden {
		t.Fatal(err)
	}
	inW.Close()
	if code := <-done; code != 0 {
		t.Fatal(code, stderr.String())
	}
	b, _ := os.ReadFile(auditPath)
	if !strings.Contains(string(b), `"method":"session/new"`) {
		t.Fatal(string(b))
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "sessions.json")); err != nil {
		t.Fatal(err)
	}
}

func TestRunFlags(t *testing.T) {
	var out, errb bytes.Buffer
	if Run([]string{"-version"}, nil, &out, &errb, "1.2.3") != 0 || out.String() != "1.2.3\n" {
		t.Fatal(out.String())
	}
	if Run([]string{"-bogus"}, nil, &out, &errb, "") != 2 {
		t.Fatal("bad flag")
	}
	if Run([]string{"-config", filepath.Join(t.TempDir(), "none.json")}, nil, &out, &errb, "") != 1 {
		t.Fatal("missing config")
	}
	// Build failure: state_dir is a file, so sessions.json cannot be read.
	dir := t.TempDir()
	cfg := writeConfig(t, dir, "")
	os.WriteFile(filepath.Join(dir, "state"), nil, 0o600)
	if Run([]string{"-config", cfg}, nil, &out, &errb, "") != 1 {
		t.Fatal("build error")
	}
}

func TestBuild(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file")
	os.WriteFile(f, nil, 0o600)
	base := config.Config{StateDir: dir, Backend: config.Backend{Kind: config.BackendSocket, Socket: "/x/{user}"}}

	c := base
	rt, closeFn, err := Build(&c, io.Discard)
	if err != nil || rt == nil {
		t.Fatal(err)
	}
	closeFn()

	c = base
	c.AuditLog = filepath.Join(f, "a", "audit.log")
	if _, _, err := Build(&c, io.Discard); err == nil {
		t.Fatal("audit mkdir")
	}
	c.AuditLog = dir
	if _, _, err := Build(&c, io.Discard); err == nil {
		t.Fatal("audit open")
	}
}

func cfgFor(t *testing.T, js string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func route(t *testing.T, p proxy.Policy, method, user string) (*proxy.Decision, error) {
	t.Helper()
	o := origin.Origin{Platform: "slack", TeamID: "T", UserID: user, ChannelID: "C"}
	return p.Route(context.Background(), &proxy.Request{Method: method, Params: map[string]json.RawMessage{}, Origin: &o})
}

func TestPolicyFromConfig(t *testing.T) {
	dir := t.TempDir()
	c := cfgFor(t, `{"state_dir":"`+dir+`","backend":{"command":["a"],"env":{"G":"1"}},
	  "policy":{"route":"static","allow":["*"],"ownership":false,"thread_cwd":false,"max_concurrent_prompts":1},
	  "users":{"slack:T:vip":{"max_concurrent_prompts":0}}}`)
	p := Policy(c)
	d, err := route(t, p, "session/new", "a")
	if err != nil {
		t.Fatal(err)
	}
	if tg := d.Targets[0]; tg.ID != "static" || tg.Dir != filepath.Join(dir, "agent") || tg.Env["G"] != "1" || d.Labels["owner"] != "" {
		t.Fatalf("%+v %v", tg, d.Labels)
	}
	if _, err := os.Stat(filepath.Join(dir, "users")); err == nil {
		t.Fatal("thread_cwd off still made dirs")
	}
	// Per-user override lifts the global prompt cap.
	d1, _ := route(t, p, "session/prompt", "vip")
	if _, err := route(t, p, "session/prompt", "vip"); err != nil {
		t.Fatal(err)
	}
	if d1.Release != nil {
		t.Fatal("uncapped prompt holds a slot")
	}
	d2, _ := route(t, p, "session/prompt", "a")
	if _, err := route(t, p, "session/prompt", "a"); !errors.Is(err, proxy.ErrRejected) {
		t.Fatal(err)
	}
	d2.Release()
	// Default per_user route with an empty allowlist denies everyone.
	p = Policy(cfgFor(t, `{"state_dir":"`+dir+`","backend":{"command":["a"]}}`))
	if _, err := route(t, p, "session/new", "a"); !errors.Is(err, proxy.ErrRejected) {
		t.Fatal(err)
	}
}

func TestUserEnv(t *testing.T) {
	f := filepath.Join(t.TempDir(), "e")
	os.WriteFile(f, []byte("K=file\n"), 0o600)
	env := UserEnv(map[string]config.User{
		"a": {Env: map[string]string{"K": "env", "J": "j"}, EnvFile: f},
		"b": {EnvFile: f + "x"},
	})
	if e, err := env("a"); err != nil || e["K"] != "file" || e["J"] != "j" {
		t.Fatal(e, err)
	}
	if e, err := env("nobody"); err != nil || len(e) != 0 {
		t.Fatal(e, err)
	}
	if _, err := env("b"); err == nil {
		t.Fatal("missing env file")
	}
	var _ policy.EnvFunc = env
}

func TestPolicyWithBase(t *testing.T) {
	dir := t.TempDir()
	c := cfgFor(t, `{"state_dir":"`+dir+`","backend":{"command":["a"]},"policy":{"allow":["slack:T:a"]}}`)
	var gotDef proxy.Policy
	p := PolicyWith(c, func(cfg *config.Config, def proxy.Policy) proxy.Policy {
		gotDef = def
		return proxy.PolicyFunc(func(context.Context, *proxy.Request) (*proxy.Decision, error) {
			return &proxy.Decision{Targets: []backend.Target{{ID: "custom"}}}, nil
		})
	})
	if _, ok := gotDef.(policy.PerUser); !ok || c.StateDir != dir {
		t.Fatalf("%T", gotDef)
	}
	d, err := route(t, p, "session/new", "a")
	if err != nil || d.Targets[0].ID != "custom" || d.Labels["owner"] != "slack:T:a" {
		t.Fatal(d, err)
	}
	// Middleware still wraps the custom base.
	if _, err := route(t, p, "session/new", "b"); !errors.Is(err, proxy.ErrRejected) {
		t.Fatal(err)
	}
}

func TestRunWithBase(t *testing.T) {
	dir := t.TempDir()
	cfg := writeConfig(t, dir, "")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	called := make(chan struct{}, 1)
	done := make(chan int)
	go func() {
		done <- RunWith([]string{"-config", cfg}, inR, outW, io.Discard, "test", func(_ *config.Config, def proxy.Policy) proxy.Policy {
			called <- struct{}{}
			return def
		})
		outW.Close()
	}()
	go io.Copy(io.Discard, outR)
	<-called
	inW.Close()
	if code := <-done; code != 0 {
		t.Fatal(code)
	}
}

// The ACP SDK logs every normal connection close at INFO through
// slog.Default; unless ACP_ROUTER_DEBUG is on, QuietSDK keeps stderr for
// warnings. Run itself leaves global logging alone.
func TestQuietSDK(t *testing.T) {
	defer slog.SetLogLoggerLevel(slog.LevelInfo)
	info := func() bool { return slog.Default().Enabled(context.Background(), slog.LevelInfo) }
	slog.SetLogLoggerLevel(slog.LevelInfo)
	Run([]string{"-version"}, nil, io.Discard, io.Discard, "v")
	if !info() {
		t.Fatal("Run changed global logging")
	}
	QuietSDK()
	if info() {
		t.Fatal("INFO still enabled")
	}
	kitlog.SetEnabled(true)
	defer kitlog.SetEnabled(false)
	slog.SetLogLoggerLevel(slog.LevelInfo)
	QuietSDK()
	if !info() {
		t.Fatal("debug lost INFO")
	}
}
