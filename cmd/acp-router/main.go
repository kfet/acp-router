// Command acp-router is an ACP-to-ACP router: one ACP agent upstream for
// relays, one ACP agent per user downstream.
package main

import (
	"os"

	kitlog "github.com/kfet/acp-kit/log"
	"github.com/kfet/distkit"

	"github.com/kfet/acp-router/internal/app"
	"github.com/kfet/acp-router/internal/dist"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "update" {
		os.Exit(distkit.Main(dist.Config(version)))
	}
	kitlog.Register("ACP_ROUTER_DEBUG")
	app.QuietSDK()
	os.Exit(app.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, version))
}
