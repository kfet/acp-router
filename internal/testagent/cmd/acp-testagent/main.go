// Command acp-testagent runs the scripted test agent over stdio, so the
// router can be demonstrated without an LLM. Its session ids are
// "<prefix>-<n>"; the prefix is the first argument (default "ta").
// See internal/testagent for the prompt script.
package main

import (
	"log/slog"
	"os"

	"github.com/kfet/acp-router/internal/testagent"
)

func main() {
	prefix := "ta"
	if len(os.Args) > 1 {
		prefix = os.Args[1]
	}
	slog.SetLogLoggerLevel(slog.LevelWarn) // the SDK logs each normal close at INFO
	a := testagent.Serve(prefix, nil, os.Stdin, os.Stdout)
	<-a.Conn.Done()
}
