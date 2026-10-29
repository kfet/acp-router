package policy_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kfet/acp-router/examples/bychannel"
	"github.com/kfet/acp-router/internal/backend"
	"github.com/kfet/acp-router/internal/origin"
	"github.com/kfet/acp-router/internal/policy"
	"github.com/kfet/acp-router/internal/proxy"
)

// examples/bychannel is a custom routing policy: one shared agent answers
// the support channel, DMs are refused, everything else falls through to
// the per-user default.
func Example_customPolicy() {
	perUser := policy.PerUser{Template: backend.Target{Kind: "exec", Command: []string{"fir", "--mode", "acp"}}, Root: "/srv/acp-router/users"}
	route := proxy.Wrap(
		bychannel.Policy{Support: "C-SUPPORT", Shared: backend.Target{ID: "support", Kind: "socket", Socket: "/run/support.sock"}, Next: perUser},
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
