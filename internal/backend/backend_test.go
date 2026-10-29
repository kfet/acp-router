package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
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

var ctx = context.Background()

func nopHandler(context.Context, string, json.RawMessage) (any, *acp.RequestError) { return nil, nil }

func TestEnviron(t *testing.T) {
	t.Setenv("KEEP", "k")
	t.Setenv("DROP", "d")
	e := &Exec{InheritEnv: []string{"KEEP", "MISSING", "B"}}
	t.Setenv("B", "parent")
	got := e.Environ(Target{Dir: "/h", Env: map[string]string{"A": "1", "B": "target", "HOME": "/evil"}})
	want := []string{"A=1", "B=target", "HOME=/h", "KEEP=k"}
	if !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
	if got := e.Environ(Target{}); slices.ContainsFunc(got, func(s string) bool { return s[:5] == "HOME=" }) {
		t.Fatalf("HOME set without Dir: %v", got)
	}
}

func TestExec(t *testing.T) {
	home := filepath.Join(t.TempDir(), "u")
	p, err := (&Exec{}).Open(ctx, Target{ID: "k", Command: []string{os.Args[0], "-test.run=^$"}, Dir: home,
		Env: map[string]string{"SECRET": "s3", "TESTAGENT_HELPER": "1"}}, nopHandler)
	if err != nil {
		t.Fatal(err)
	}
	res, err := acp.SendRequest[map[string]any](p.Conn(), ctx, "session/new", map[string]any{"cwd": "/x", "mcpServers": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	m := res["_meta"].(map[string]any)
	if m["home"] != home || m["secret"] != "s3" {
		t.Fatalf("%v", m)
	}
	p.Close()
	p.Close()
	<-p.Done()
}

func TestExecErrors(t *testing.T) {
	if _, err := (&Exec{}).Open(ctx, Target{}, nopHandler); err == nil {
		t.Fatal("empty command")
	}
	f := filepath.Join(t.TempDir(), "f")
	os.WriteFile(f, nil, 0o600)
	if _, err := (&Exec{}).Open(ctx, Target{Command: []string{"true"}, Dir: filepath.Join(f, "h")}, nopHandler); err == nil {
		t.Fatal("mkdir")
	}
	if _, err := (&Exec{}).Open(ctx, Target{Command: []string{"/nonexistent/x"}}, nopHandler); err == nil {
		t.Fatal("start")
	}
}

func TestSocket(t *testing.T) {
	dir := t.TempDir()
	l, err := net.Listen("unix", filepath.Join(dir, "alice.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		testagent.Serve("sock", nil, c, c)
	}()
	p, err := Socket{}.Open(ctx, Target{Socket: filepath.Join(dir, "alice.sock")}, nopHandler)
	if err != nil {
		t.Fatal(err)
	}
	res, err := acp.SendRequest[map[string]any](p.Conn(), ctx, "session/new", map[string]any{"cwd": "/x"})
	if err != nil || res["sessionId"] != "sock-1" {
		t.Fatal(res, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	<-p.Done()
	if _, err := (Socket{}).Open(ctx, Target{Socket: filepath.Join(dir, "bob.sock")}, nopHandler); err == nil {
		t.Fatal("dial missing socket")
	}
}

// An agent that ignores EOF is killed after the grace period, together
// with the children holding its pipes.
func TestExecKillsAfterGrace(t *testing.T) {
	var stderr bytes.Buffer // not an *os.File: Wait copies it until every holder exits
	p, err := (&Exec{Grace: time.Millisecond, Stderr: &stderr}).Open(ctx, Target{Command: []string{"sh", "-c", "sleep 60 & sleep 60"}}, nopHandler)
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	<-p.Done()
}
