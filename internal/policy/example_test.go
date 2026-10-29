package policy_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kfet/acp-router/internal/backend"
	"github.com/kfet/acp-router/internal/origin"
	"github.com/kfet/acp-router/internal/policy"
	"github.com/kfet/acp-router/internal/proxy"
)

// byChannel is a custom routing policy: one shared agent answers the
// support channel, DMs are refused, everything else falls through to the
// per-user default. Existing sessions stay on the target that created them.
type byChannel struct {
	support string
	shared  backend.Target
	next    proxy.Policy
}

func (p byChannel) Route(ctx context.Context, r *proxy.Request) (*proxy.Decision, error) {
	if r.Session != nil && r.Session.Target == p.shared.ID {
		return &proxy.Decision{Targets: []backend.Target{p.shared}}, nil
	}
	if o := r.Origin; o != nil {
		if o.IsDM {
			return nil, fmt.Errorf("%w: DMs are not served here", proxy.ErrRejected)
		}
		if o.ChannelID == p.support {
			return &proxy.Decision{Targets: []backend.Target{p.shared}}, nil
		}
	}
	return p.next.Route(ctx, r)
}

func Example_customPolicy() {
	perUser := policy.PerUser{Template: backend.Target{Kind: "exec", Command: []string{"fir", "--mode", "acp"}}, Root: "/srv/acp-router/users"}
	route := proxy.Wrap(
		byChannel{support: "C-SUPPORT", shared: backend.Target{ID: "support", Kind: "socket", Socket: "/run/support.sock"}, next: perUser},
		policy.Allowlist([]string{"slack:T1:*"}),
		policy.Ownership(),
	)
	// proxy.New(proxy.Options{Policy: route, ...}) would serve it; here we
	// just ask it where three requests go.
	for _, o := range []origin.Origin{
		{Platform: "slack", TeamID: "T1", UserID: "U1", ChannelID: "C-SUPPORT"},
		{Platform: "slack", TeamID: "T1", UserID: "U1", ChannelID: "C-DEV"},
		{Platform: "slack", TeamID: "T1", UserID: "U1", IsDM: true},
	} {
		d, err := route.Route(context.Background(), &proxy.Request{
			Method: "session/new", Params: map[string]json.RawMessage{}, Origin: &o,
		})
		if err != nil {
			fmt.Println("rejected:", err)
			continue
		}
		fmt.Println(d.Targets[0].ID, "owner="+d.Labels["owner"])
	}
	// Output:
	// support owner=slack:T1:U1
	// user:slack:T1:U1 owner=slack:T1:U1
	// rejected: rejected: DMs are not served here
}
