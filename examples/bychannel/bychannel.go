// Package bychannel is a custom routing policy plugged into the stock
// router: one shared agent answers a support channel, DMs are refused,
// and everything else falls through to the built-in per-user policy. The
// configured middleware (allowlist, ownership, limits, cwd, credentials)
// still wraps it. See docs/policy.md.
package bychannel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kfet/acp-router/internal/app"
	"github.com/kfet/acp-router/internal/backend"
	"github.com/kfet/acp-router/internal/config"
	"github.com/kfet/acp-router/internal/policy"
	"github.com/kfet/acp-router/internal/proxy"
)

// DefaultChannel is the support channel when $BYCHANNEL_SUPPORT is unset.
const DefaultChannel = "C-SUPPORT"

// Policy routes Support to Shared and the rest to Next. Existing sessions
// stay on the target that created them.
type Policy struct {
	Support string
	Shared  backend.Target
	Next    proxy.Policy
}

// Route implements proxy.Policy.
func (p Policy) Route(ctx context.Context, r *proxy.Request) (*proxy.Decision, error) {
	if r.Session != nil { // sessions stay on the target that created them
		if r.Session.Target == p.Shared.ID {
			return p.shared(r)
		}
		return p.Next.Route(ctx, r)
	}
	if o := r.Origin; o != nil {
		if o.IsDM {
			return nil, fmt.Errorf("%w: DMs are not served here", proxy.ErrRejected)
		}
		if o.ChannelID == p.Support {
			return p.shared(r)
		}
	}
	return p.Next.Route(ctx, r)
}

// shared routes r to the shared agent. Its sessions get a cwd under its own
// Dir: a user's directory is that user's agent HOME and stays private.
func (p Policy) shared(r *proxy.Request) (*proxy.Decision, error) {
	if o := r.Origin; o != nil && policy.TakesCwd(r.Method) {
		dir := filepath.Join(p.Shared.Dir, "threads", policy.Safe(policy.UserKey(*o)), policy.Safe(o.ChannelID), policy.Safe(o.ThreadID))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		r.Params["cwd"], _ = json.Marshal(dir) // a string always marshals
	}
	return &proxy.Decision{Targets: []backend.Target{p.Shared}}, nil
}

// Base returns an app.BaseFunc that puts Policy in front of the built-in
// base. The shared agent runs the configured backend command with ID
// "support" and HOME <state_dir>/support.
func Base(channel string) app.BaseFunc {
	return func(cfg *config.Config, def proxy.Policy) proxy.Policy {
		shared := backend.Target{
			ID: "support", Kind: cfg.Backend.Kind, Command: cfg.Backend.Command,
			Env: cfg.Backend.Env, Socket: cfg.Backend.Socket,
			Dir: filepath.Join(cfg.StateDir, "support"),
		}
		return Policy{Support: channel, Shared: shared, Next: def}
	}
}

// Channel returns $BYCHANNEL_SUPPORT, or DefaultChannel.
func Channel() string {
	if c := os.Getenv("BYCHANNEL_SUPPORT"); c != "" {
		return c
	}
	return DefaultChannel
}
