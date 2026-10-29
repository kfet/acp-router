// Package policy holds the built-in routing policies and middleware for
// the proxy core:
//
//   - Static: every request goes to one target.
//   - PerUser: one target per origin user (the default).
//
// and optional middleware, none of which the core requires:
//
//   - Allowlist: only listed users get through.
//   - Ownership: a session belongs to the user who created it.
//   - PromptLimit: cap a user's in-flight prompts.
//   - ThreadCwd: pick the real cwd per user and thread.
//   - Credentials: per-user env (LLM keys, ...) on the chosen targets.
//
// A custom policy is any proxy.Policy; see the package example.
package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kfet/acp-router/internal/backend"
	"github.com/kfet/acp-router/internal/origin"
	"github.com/kfet/acp-router/internal/proxy"
	"github.com/kfet/acp-router/internal/store"
)

// Label keys the built-ins persist on sessions.
const (
	LabelUser  = "user"
	LabelOwner = "owner"
)

// UserKey is the identity of an origin: "<platform>:<team_id>:<user_id>".
func UserKey(o origin.Origin) string { return proxy.UserKey(o) }

// sessionUser is the user recorded on a session, "" when none.
func sessionUser(s *store.Session) string {
	if u := s.Labels[LabelUser]; u != "" {
		return u
	}
	return s.Labels[LabelOwner]
}

// User returns the user a request acts for: the one recorded on its
// session (sessions stay with their user), else the origin's. Empty when
// neither is known.
func User(r *proxy.Request) string {
	if r.Session != nil {
		if u := sessionUser(r.Session); u != "" {
			return u
		}
	}
	if r.Origin != nil {
		return UserKey(*r.Origin)
	}
	return ""
}

// Safe makes s usable as one path segment: injective, no separators, no
// "." or "..".
func Safe(s string) string {
	switch s {
	case "":
		return "_"
	case ".", "..":
		return strings.ReplaceAll(s, ".", "%2E")
	}
	return url.PathEscape(s)
}

func rejectf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{proxy.ErrRejected}, args...)...)
}

func addLabel(d *proxy.Decision, k, v string) {
	if d.Labels == nil {
		d.Labels = map[string]string{}
	}
	d.Labels[k] = v
}

// Static routes every request to t. Each Decision gets its own Env copy so
// middleware (Credentials) never writes into t.
func Static(t backend.Target) proxy.Policy {
	return proxy.PolicyFunc(func(context.Context, *proxy.Request) (*proxy.Decision, error) {
		t := t
		t.Env = maps.Clone(t.Env)
		return &proxy.Decision{Targets: []backend.Target{t}}, nil
	})
}

// PerUser routes each user to its own target built from Template: ID
// "user:<key>", Dir "<Root>/<safe key>", and "{user}" in Socket replaced
// by the safe key. Requests with no known user are rejected.
type PerUser struct {
	Template backend.Target
	Root     string
}

// Route implements proxy.Policy.
func (p PerUser) Route(_ context.Context, r *proxy.Request) (*proxy.Decision, error) {
	u := User(r)
	if u == "" {
		return nil, rejectf("%s: no user (origin _meta missing)", r.Method)
	}
	t := p.Template
	t.ID = TargetID(u)
	t.Dir = filepath.Join(p.Root, Safe(u))
	t.Socket = strings.ReplaceAll(t.Socket, "{user}", Safe(u))
	t.Env = maps.Clone(t.Env)
	d := &proxy.Decision{Targets: []backend.Target{t}}
	addLabel(d, LabelUser, u)
	return d, nil
}

// Allowlist rejects users that match no entry. An entry ending in "*"
// matches a prefix; "*" matches everyone.
func Allowlist(allow []string) proxy.Middleware {
	return func(next proxy.Policy) proxy.Policy {
		return proxy.PolicyFunc(func(ctx context.Context, r *proxy.Request) (*proxy.Decision, error) {
			u := User(r)
			for _, a := range allow {
				if pre, ok := strings.CutSuffix(a, "*"); ok && strings.HasPrefix(u, pre) || a == u && u != "" {
					return next.Route(ctx, r)
				}
			}
			return nil, rejectf("user %q is not allowed", u)
		})
	}
}

var originRequired = map[string]bool{
	"session/new": true, "session/load": true, "session/resume": true,
	"session/fork": true, "session/prompt": true,
}

// Ownership requires an origin on session/new, load, resume, fork and
// prompt; records the creator as owner of new sessions; and refuses any
// request whose origin is not the owner of the session it names.
// Session-scoped calls without an origin (cancel, set_mode, ...) are left
// to the rest of the chain.
func Ownership() proxy.Middleware {
	return func(next proxy.Policy) proxy.Policy {
		return proxy.PolicyFunc(func(ctx context.Context, r *proxy.Request) (*proxy.Decision, error) {
			if originRequired[r.Method] && r.Origin == nil {
				return nil, rejectf("%s needs origin _meta", r.Method)
			}
			if r.Origin != nil && r.Session != nil {
				owner := r.Session.Labels[LabelOwner]
				if owner == "" {
					// Created while ownership was off: the routed user owns it.
					owner = r.Session.Labels[LabelUser]
				}
				if me := UserKey(*r.Origin); owner == "" || owner != me {
					return nil, rejectf("session %s is not owned by %s", r.Session.Upstream, me)
				}
			}
			d, err := next.Route(ctx, r)
			if err == nil && (r.Method == "session/new" || r.Method == "session/fork") {
				addLabel(d, LabelOwner, UserKey(*r.Origin))
			}
			return d, err
		})
	}
}

// PromptLimit caps each user's in-flight session/prompt requests at max
// (per[user] overrides; <= 0 means unlimited).
func PromptLimit(max int, per map[string]int) proxy.Middleware {
	var mu sync.Mutex
	inflight := map[string]int{}
	return func(next proxy.Policy) proxy.Policy {
		return proxy.PolicyFunc(func(ctx context.Context, r *proxy.Request) (*proxy.Decision, error) {
			if r.Method != "session/prompt" {
				return next.Route(ctx, r)
			}
			u, limit := User(r), max
			if n, ok := per[u]; ok {
				limit = n
			}
			if limit <= 0 {
				return next.Route(ctx, r)
			}
			mu.Lock()
			if inflight[u] >= limit {
				mu.Unlock()
				return nil, rejectf("user %s has %d prompts in flight", u, limit)
			}
			inflight[u]++
			mu.Unlock()
			release := func() {
				mu.Lock()
				inflight[u]--
				mu.Unlock()
			}
			d, err := next.Route(ctx, r)
			if err != nil {
				release()
				return nil, err
			}
			inner := d.Release
			d.Release = func() {
				if inner != nil {
					inner()
				}
				release()
			}
			return d, nil
		})
	}
}

var cwdMethods = map[string]bool{
	"session/new": true, "session/load": true, "session/resume": true, "session/fork": true,
}

// TakesCwd reports whether method carries a cwd that ThreadCwd sets.
func TakesCwd(method string) bool { return cwdMethods[method] }

// ThreadCwd replaces the relay's cwd (opaque, possibly a placeholder) on
// session/new, load, resume and fork with
// "<root>/<safe user>/threads/<channel>/<thread>", creating it once the
// rest of the chain has accepted the request. An inner policy that sets a
// different cwd owns it. Requests
// without an origin keep their cwd.
func ThreadCwd(root string) proxy.Middleware {
	return func(next proxy.Policy) proxy.Policy {
		return proxy.PolicyFunc(func(ctx context.Context, r *proxy.Request) (*proxy.Decision, error) {
			if !TakesCwd(r.Method) || r.Origin == nil {
				return next.Route(ctx, r)
			}
			o := r.Origin
			dir := filepath.Join(root, Safe(UserKey(*o)), "threads", Safe(o.ChannelID), Safe(o.ThreadID))
			cwd, _ := json.Marshal(dir) // a string always marshals
			r.Params["cwd"] = cwd
			d, err := next.Route(ctx, r)
			if err != nil {
				return nil, err // rejected: leave nothing on disk
			}
			if !bytes.Equal(r.Params["cwd"], cwd) {
				return d, nil // the inner policy chose its own cwd
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				if d.Release != nil {
					d.Release()
				}
				return nil, err
			}
			return d, nil
		})
	}
}

// TargetID is the ID PerUser gives user's agent.
func TargetID(user string) string { return "user:" + user }

// EnvFunc returns the environment overrides for a user.
type EnvFunc func(user string) (map[string]string, error)

// Credentials adds env(user) to the user's own target (TargetID(user)),
// on a copy of its Env. Shared targets (static, or a custom policy's) get
// none: they are opened once and serve every user. Env only reaches an
// agent when its target is first opened.
func Credentials(env EnvFunc) proxy.Middleware {
	return func(next proxy.Policy) proxy.Policy {
		return proxy.PolicyFunc(func(ctx context.Context, r *proxy.Request) (*proxy.Decision, error) {
			d, err := next.Route(ctx, r)
			if err != nil {
				return nil, err
			}
			e, err := env(User(r))
			if err != nil {
				if d.Release != nil {
					d.Release()
				}
				return nil, err
			}
			own := TargetID(User(r))
			for i := range d.Targets {
				if t := &d.Targets[i]; t.ID == own {
					t.Env = maps.Clone(t.Env)
					if t.Env == nil {
						t.Env = map[string]string{}
					}
					maps.Copy(t.Env, e)
				}
			}
			return d, nil
		})
	}
}
