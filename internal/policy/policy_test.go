package policy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kfet/acp-router/internal/backend"
	"github.com/kfet/acp-router/internal/origin"
	"github.com/kfet/acp-router/internal/proxy"
	"github.com/kfet/acp-router/internal/store"
)

var ctx = context.Background()

func org(user string) *origin.Origin {
	return &origin.Origin{Platform: "slack", TeamID: "T", UserID: user, ChannelID: "C", ThreadID: "1.5"}
}

func req(method string, o *origin.Origin, s *store.Session) *proxy.Request {
	return &proxy.Request{Method: method, Params: map[string]json.RawMessage{}, Origin: o, Session: s}
}

func rejected(err error) bool { return errors.Is(err, proxy.ErrRejected) }

// accept is a terminal policy returning one empty target.
var accept = proxy.PolicyFunc(func(context.Context, *proxy.Request) (*proxy.Decision, error) {
	return &proxy.Decision{Targets: []backend.Target{{ID: "t"}}}, nil
})

var refuse = proxy.PolicyFunc(func(context.Context, *proxy.Request) (*proxy.Decision, error) {
	return nil, errors.New("inner")
})

func TestUserAndSafe(t *testing.T) {
	if User(req("x", org("a"), nil)) != "slack:T:a" {
		t.Fatal()
	}
	if User(req("x", nil, &store.Session{Labels: map[string]string{LabelUser: "u"}})) != "u" {
		t.Fatal()
	}
	if User(req("x", nil, &store.Session{Labels: map[string]string{LabelOwner: "o"}})) != "o" {
		t.Fatal()
	}
	if User(req("x", nil, nil)) != "" {
		t.Fatal()
	}
	for in, want := range map[string]string{"": "_", ".": "%2E", "..": "%2E%2E", "a/b": "a%2Fb", "1.5": "1.5"} {
		if got := Safe(in); got != want {
			t.Errorf("Safe(%q) = %q", in, got)
		}
	}
}

func TestStatic(t *testing.T) {
	p := Static(backend.Target{ID: "one", Env: map[string]string{"A": "1"}})
	d, _ := p.Route(ctx, req("session/new", nil, nil))
	d.Targets[0].Env["A"] = "changed"
	d2, _ := p.Route(ctx, req("session/new", nil, nil))
	if d2.Targets[0].ID != "one" || d2.Targets[0].Env["A"] != "1" {
		t.Fatal(d2.Targets)
	}
}

func TestPerUser(t *testing.T) {
	p := PerUser{Template: backend.Target{Kind: "socket", Socket: "/run/{user}.sock"}, Root: "/r"}
	d, err := p.Route(ctx, req("session/new", org("a"), nil))
	if err != nil {
		t.Fatal(err)
	}
	tg := d.Targets[0]
	if tg.ID != "user:slack:T:a" || tg.Dir != "/r/slack:T:a" || tg.Socket != "/run/slack:T:a.sock" || d.Labels[LabelUser] != "slack:T:a" {
		t.Fatalf("%+v %v", tg, d.Labels)
	}
	if _, err := p.Route(ctx, req("session/new", nil, nil)); !rejected(err) {
		t.Fatal(err)
	}
}

func TestAllowlist(t *testing.T) {
	p := Allowlist([]string{"slack:T:*", "zulip::9"})(accept)
	for _, c := range []struct {
		o    *origin.Origin
		want bool
	}{
		{org("a"), true},
		{&origin.Origin{Platform: "zulip", UserID: "9"}, true},
		{&origin.Origin{Platform: "zulip", UserID: "8"}, false},
		{nil, false},
	} {
		_, err := p.Route(ctx, req("session/new", c.o, nil))
		if (err == nil) != c.want {
			t.Errorf("%+v: %v", c.o, err)
		}
	}
	if _, err := Allowlist([]string{"*"})(accept).Route(ctx, req("x", nil, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := Allowlist(nil)(accept).Route(ctx, req("x", org("a"), nil)); !rejected(err) {
		t.Fatal(err)
	}
	if _, err := Allowlist([]string{""})(accept).Route(ctx, req("x", nil, nil)); !rejected(err) {
		t.Fatal("empty entry must not match an unknown user")
	}
}

func TestOwnership(t *testing.T) {
	p := Ownership()(accept)
	d, err := p.Route(ctx, req("session/new", org("a"), nil))
	if err != nil || d.Labels[LabelOwner] != "slack:T:a" {
		t.Fatal(d, err)
	}
	if _, err := p.Route(ctx, req("session/prompt", nil, &store.Session{})); !rejected(err) {
		t.Fatal(err)
	}
	s := &store.Session{Upstream: "s", Labels: map[string]string{LabelOwner: "slack:T:a"}}
	if _, err := p.Route(ctx, req("session/prompt", org("b"), s)); !rejected(err) {
		t.Fatal(err)
	}
	d, err = p.Route(ctx, req("session/fork", org("a"), s))
	if err != nil || d.Labels[LabelOwner] != "slack:T:a" {
		t.Fatal(d, err)
	}
	if _, err := p.Route(ctx, req("session/cancel", nil, s)); err != nil {
		t.Fatal(err)
	}
	if _, err := Ownership()(refuse).Route(ctx, req("session/new", org("a"), nil)); err == nil {
		t.Fatal("inner error swallowed")
	}
}

func TestPromptLimit(t *testing.T) {
	inner := 0
	withRelease := proxy.PolicyFunc(func(context.Context, *proxy.Request) (*proxy.Decision, error) {
		return &proxy.Decision{Targets: []backend.Target{{ID: "t"}}, Release: func() { inner++ }}, nil
	})
	p := PromptLimit(1, map[string]int{"slack:T:vip": 0})(withRelease)
	r := req("session/prompt", org("a"), nil)
	d, err := p.Route(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Route(ctx, r); !rejected(err) {
		t.Fatal(err)
	}
	d.Release()
	if inner != 1 {
		t.Fatal("inner release not called")
	}
	d, err = p.Route(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	d.Release()
	// Unlimited override, other methods, and a terminal without Release.
	for _, rr := range []*proxy.Request{req("session/prompt", org("vip"), nil), req("session/new", org("a"), nil)} {
		if _, err := p.Route(ctx, rr); err != nil {
			t.Fatal(err)
		}
	}
	d, _ = PromptLimit(1, nil)(accept).Route(ctx, r)
	d.Release()
	// A rejection further down frees the slot.
	q := PromptLimit(1, nil)(refuse)
	q.Route(ctx, r)
	if _, err := q.Route(ctx, r); rejected(err) {
		t.Fatal("slot leaked")
	}
}

func TestThreadCwd(t *testing.T) {
	root := t.TempDir()
	p := ThreadCwd(root)(accept)
	r := req("session/new", org("a"), nil)
	r.Params["cwd"] = json.RawMessage(`"/placeholder"`)
	if _, err := p.Route(ctx, r); err != nil {
		t.Fatal(err)
	}
	var cwd string
	json.Unmarshal(r.Params["cwd"], &cwd)
	want := filepath.Join(root, "slack:T:a", "threads", "C", "1.5")
	if cwd != want {
		t.Fatal(cwd)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Fatal(err)
	}
	r = req("session/prompt", org("a"), nil)
	p.Route(ctx, r)
	if _, ok := r.Params["cwd"]; ok {
		t.Fatal("prompt got a cwd")
	}
	f := filepath.Join(root, "file")
	os.WriteFile(f, nil, 0o600)
	if _, err := ThreadCwd(f)(accept).Route(ctx, req("session/new", org("a"), nil)); err == nil {
		t.Fatal("mkdir under a file")
	}
}

func TestCredentials(t *testing.T) {
	env := func(u string) (map[string]string, error) {
		if u == "slack:T:bad" {
			return nil, errors.New("no env")
		}
		return map[string]string{"KEY": u}, nil
	}
	d, err := Credentials(env)(accept).Route(ctx, req("session/new", org("a"), nil))
	if err != nil || d.Targets[0].Env["KEY"] != "slack:T:a" {
		t.Fatal(d, err)
	}
	released := false
	withRelease := proxy.PolicyFunc(func(context.Context, *proxy.Request) (*proxy.Decision, error) {
		return &proxy.Decision{Targets: []backend.Target{{Env: map[string]string{"B": "1"}}}, Release: func() { released = true }}, nil
	})
	if _, err := Credentials(env)(withRelease).Route(ctx, req("session/new", org("bad"), nil)); err == nil || !released {
		t.Fatal(err, released)
	}
	if _, err := Credentials(env)(accept).Route(ctx, req("session/new", org("bad"), nil)); err == nil {
		t.Fatal("env error swallowed")
	}
	if _, err := Credentials(env)(refuse).Route(ctx, req("session/new", org("a"), nil)); err == nil {
		t.Fatal("inner error swallowed")
	}
	d, _ = Credentials(env)(withRelease).Route(ctx, req("session/new", org("a"), nil))
	if d.Targets[0].Env["B"] != "1" || d.Targets[0].Env["KEY"] != "slack:T:a" {
		t.Fatal(d.Targets[0].Env)
	}
}

// A session created with ownership off (no owner label) still belongs to
// the user that PerUser recorded; a session with neither is refused.
func TestOwnershipFallsBackToUserLabel(t *testing.T) {
	p := Ownership()(accept)
	s := &store.Session{Upstream: "s", Labels: map[string]string{LabelUser: "slack:T:a"}}
	if _, err := p.Route(ctx, req("session/prompt", org("a"), s)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Route(ctx, req("session/prompt", org("b"), s)); !rejected(err) {
		t.Fatal(err)
	}
	if _, err := p.Route(ctx, req("session/prompt", org("a"), &store.Session{Upstream: "s"})); !rejected(err) {
		t.Fatal(err)
	}
}

// Requests on an existing session route to the session's user, not the
// origin's.
func TestPerUserKeepsSessionTarget(t *testing.T) {
	s := &store.Session{Labels: map[string]string{LabelUser: "slack:T:a"}}
	d, err := PerUser{Root: "/r"}.Route(ctx, req("session/prompt", org("b"), s))
	if err != nil || d.Targets[0].ID != "user:slack:T:a" {
		t.Fatal(d, err)
	}
}
