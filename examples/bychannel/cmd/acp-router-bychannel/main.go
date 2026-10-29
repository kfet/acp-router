// Command acp-router-bychannel is acp-router with the bychannel policy as
// its base. Same flags and config as acp-router.
package main

import (
	"os"

	kitlog "github.com/kfet/acp-kit/log"

	"github.com/kfet/acp-router/examples/bychannel"
	"github.com/kfet/acp-router/internal/app"
)

var version = "dev"

func main() {
	kitlog.Register("ACP_ROUTER_DEBUG")
	app.QuietSDK()
	os.Exit(app.RunWith(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, version, bychannel.Base(bychannel.Channel())))
}
