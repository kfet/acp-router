// Package dist holds the one definition of how acp-router is distributed:
// which GitHub repo its releases come from and how its assets are named.
// Both the `update` subcommand (distkit.Main) and the generated install.sh
// read it, checked by TestInstallShSpecMatchesConfig.
package dist

import "github.com/kfet/distkit"

// Repo is the GitHub owner/name releases are published to.
const Repo = "kfet/acp-router"

// Binary is the installed file name.
const Binary = "acp-router"

// RestartHint tells the operator how to pick up a swapped binary.
// acp-router has no service of its own: a relay spawns it over stdio, so
// the new binary runs once the relay respawns its agent.
const RestartHint = "restart the relay that spawns acp-router (its agent command) to run the new binary"

// Config is the distkit configuration for this binary.
func Config(version string) distkit.Config {
	return distkit.Config{
		Repo:        Repo,
		Binary:      Binary,
		AssetStem:   Binary,
		Version:     version,
		RestartHint: RestartHint,
	}
}
