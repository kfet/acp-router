package bychannel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kfet/acp-router/internal/backend"
	"github.com/kfet/acp-router/internal/config"
	"github.com/kfet/acp-router/internal/origin"
	"github.com/kfet/acp-router/internal/proxy"
	"github.com/kfet/acp-router/internal/store"
)

var next = proxy.PolicyFunc(func(context.Context, *proxy.Request) (*proxy.Decision, error) {
	return &proxy.Decision{Targets: []backend.Target{{ID: "next"}}}, nil
})

func route(t *testing.T, p proxy.Policy, r *proxy.Request) string {
	t.Helper()
	r.Params = map[string]json.RawMessage{}
	d, err := p.Route(context.Background(), r)
	if err != nil {
		return "err:" + err.Error()
	}
	return d.Targets[0].ID
}

func TestRoute(t *testing.T) {
	p := Policy{Support: "C-S", Shared: backend.Target{ID: "support"}, Next: next}
	for name, c := range map[string]struct {
		r    *proxy.Request
		want string
	}{
		"support":                  {&proxy.Request{Origin: &origin.Origin{ChannelID: "C-S"}}, "support"},
		"other":                    {&proxy.Request{Origin: &origin.Origin{ChannelID: "C-X"}}, "next"},
		"no origin":                {&proxy.Request{}, "next"},
		"dm":                       {&proxy.Request{Origin: &origin.Origin{IsDM: true}}, "err:rejected: DMs are not served here"},
		"own session from support": {&proxy.Request{Session: &store.Session{Target: "user:x"}, Origin: &origin.Origin{ChannelID: "C-S"}}, "next"},
		"existing":                 {&proxy.Request{Session: &store.Session{Target: "support"}, Origin: &origin.Origin{IsDM: true}}, "support"},
	} {
		if got := route(t, p, c.r); got != c.want {
			t.Errorf("%s: got %q want %q", name, got, c.want)
		}
	}
	_, err := p.Route(context.Background(), &proxy.Request{Origin: &origin.Origin{IsDM: true}})
	if !errors.Is(err, proxy.ErrRejected) {
		t.Fatal(err)
	}
}

func TestBase(t *testing.T) {
	cfg := &config.Config{StateDir: "/s", Backend: config.Backend{Kind: "exec", Command: []string{"a"}, Env: map[string]string{"K": "v"}}}
	p := Base("C-S")(cfg, next).(Policy)
	if p.Support != "C-S" || p.Shared.ID != "support" || p.Shared.Dir != "/s/support" || p.Shared.Command[0] != "a" || p.Shared.Env["K"] != "v" {
		t.Fatalf("%+v", p)
	}
	if route(t, p, &proxy.Request{}) != "next" {
		t.Fatal("next not wired")
	}
}

func TestChannel(t *testing.T) {
	t.Setenv("BYCHANNEL_SUPPORT", "")
	if Channel() != DefaultChannel {
		t.Fatal(Channel())
	}
	t.Setenv("BYCHANNEL_SUPPORT", "C-1")
	if Channel() != "C-1" {
		t.Fatal(Channel())
	}
}

// Shared sessions get a cwd under the shared agent's own root, never under
// a user's directory (that is the user's agent HOME).
func TestSharedCwd(t *testing.T) {
	root := t.TempDir()
	p := Policy{Support: "C-S", Shared: backend.Target{ID: "support", Dir: root}, Next: next}
	o := &origin.Origin{Platform: "slack", TeamID: "T", UserID: "a", ChannelID: "C-S", ThreadID: "1.5"}
	r := &proxy.Request{Method: "session/new", Origin: o, Params: map[string]json.RawMessage{"cwd": json.RawMessage(`"/users/a/threads"`)}}
	if _, err := p.Route(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "threads", "slack:T:a", "C-S", "1.5")
	var cwd string
	json.Unmarshal(r.Params["cwd"], &cwd)
	if cwd != want {
		t.Fatal(cwd)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Fatal(err)
	}
	// A prompt keeps its params.
	r = &proxy.Request{Method: "session/prompt", Origin: o, Params: map[string]json.RawMessage{}}
	p.Route(context.Background(), r)
	if _, ok := r.Params["cwd"]; ok {
		t.Fatal("prompt got a cwd")
	}
	// mkdir failure is returned.
	f := filepath.Join(root, "file")
	os.WriteFile(f, nil, 0o600)
	p.Shared.Dir = f
	r = &proxy.Request{Method: "session/new", Origin: o, Params: map[string]json.RawMessage{}}
	if _, err := p.Route(context.Background(), r); err == nil {
		t.Fatal("mkdir under a file")
	}
}

// load/resume/fork on an existing shared session also get the shared cwd;
// without an origin the relay's cwd is kept.
func TestSharedCwdExistingSession(t *testing.T) {
	root := t.TempDir()
	p := Policy{Support: "C-S", Shared: backend.Target{ID: "support", Dir: root}, Next: next}
	o := &origin.Origin{Platform: "slack", TeamID: "T", UserID: "a", ChannelID: "C-S", ThreadID: "1.5"}
	r := &proxy.Request{Method: "session/load", Origin: o, Session: &store.Session{Target: "support"}, Params: map[string]json.RawMessage{}}
	if d, err := p.Route(context.Background(), r); err != nil || d.Targets[0].ID != "support" {
		t.Fatal(d, err)
	}
	var cwd string
	json.Unmarshal(r.Params["cwd"], &cwd)
	if cwd != filepath.Join(root, "threads", "slack:T:a", "C-S", "1.5") {
		t.Fatal(cwd)
	}
	r = &proxy.Request{Method: "session/load", Session: &store.Session{Target: "support"}, Params: map[string]json.RawMessage{"cwd": json.RawMessage(`"/relay"`)}}
	if d, err := p.Route(context.Background(), r); err != nil || d.Targets[0].ID != "support" || string(r.Params["cwd"]) != `"/relay"` {
		t.Fatal(d, err, string(r.Params["cwd"]))
	}
}
