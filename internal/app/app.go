// Package app wires config into the proxy core: backends, the built-in
// routing policy and its middleware, the session store and the audit log.
package app

import (
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"

	kitlog "github.com/kfet/acp-kit/log"

	"github.com/kfet/acp-router/internal/audit"
	"github.com/kfet/acp-router/internal/backend"
	"github.com/kfet/acp-router/internal/config"
	"github.com/kfet/acp-router/internal/policy"
	"github.com/kfet/acp-router/internal/proxy"
	"github.com/kfet/acp-router/internal/store"
)

// DefaultConfigPath is used when -config is not given.
const DefaultConfigPath = "~/.config/acp-router/config.json"

// Run serves ACP on stdin/stdout until the relay hangs up and returns the
// process exit code. Diagnostics go to stderr; stdout carries only ACP.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer, version string) int {
	fs := flag.NewFlagSet("acp-router", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", DefaultConfigPath, "path to JSON config file")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}
	cfg, err := config.Load(config.ExpandHome(*cfgPath))
	if err != nil {
		fmt.Fprintf(stderr, "acp-router: %v\n", err)
		return 1
	}
	rt, closeFn, err := Build(cfg, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "acp-router: %v\n", err)
		return 1
	}
	defer closeFn()
	kitlog.Debugf("acp-router %s: serving, state_dir=%s route=%s backend=%s", version, cfg.StateDir, cfg.Policy.Route, cfg.Backend.Kind)
	up := rt.Serve(stdin, stdout)
	<-up.Done()
	rt.Close()
	return 0
}

// Policy builds the configured routing policy with its middleware.
func Policy(cfg *config.Config) proxy.Policy {
	t := backend.Target{Kind: cfg.Backend.Kind, Command: cfg.Backend.Command, Env: cfg.Backend.Env, Socket: cfg.Backend.Socket}
	usersDir := filepath.Join(cfg.StateDir, "users")
	var base proxy.Policy = policy.PerUser{Template: t, Root: usersDir}
	if cfg.Policy.Route == config.RouteStatic {
		t.ID, t.Dir = "static", filepath.Join(cfg.StateDir, "agent")
		base = policy.Static(t)
	}
	per := map[string]int{}
	for k, u := range cfg.Users {
		if u.MaxConcurrentPrompts != nil {
			per[k] = *u.MaxConcurrentPrompts
		}
	}
	mws := []proxy.Middleware{policy.Allowlist(cfg.Policy.Allow)}
	if config.On(cfg.Policy.Ownership) {
		mws = append(mws, policy.Ownership())
	}
	mws = append(mws, policy.PromptLimit(cfg.Policy.MaxConcurrentPrompts, per))
	if config.On(cfg.Policy.ThreadCwd) {
		mws = append(mws, policy.ThreadCwd(usersDir))
	}
	mws = append(mws, policy.Credentials(UserEnv(cfg.Users)))
	return proxy.Wrap(base, mws...)
}

// UserEnv resolves a user's env from config: env, then env_file.
func UserEnv(users map[string]config.User) policy.EnvFunc {
	return func(user string) (map[string]string, error) {
		u := users[user]
		env := maps.Clone(u.Env)
		if env == nil {
			env = map[string]string{}
		}
		if u.EnvFile != "" {
			fe, err := config.ReadEnvFile(u.EnvFile)
			if err != nil {
				return nil, err
			}
			maps.Copy(env, fe)
		}
		return env, nil
	}
}

// Build assembles a Router from cfg. The returned func closes the audit
// log file, if any.
func Build(cfg *config.Config, stderr io.Writer) (*proxy.Router, func(), error) {
	st, err := store.Open(filepath.Join(cfg.StateDir, "sessions.json"))
	if err != nil {
		return nil, nil, err
	}
	auditW, closeFn := io.Writer(stderr), func() {}
	if cfg.AuditLog != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.AuditLog), 0o700); err != nil {
			return nil, nil, err
		}
		f, err := os.OpenFile(cfg.AuditLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, err
		}
		auditW, closeFn = f, func() { _ = f.Close() }
	}
	rt := proxy.New(proxy.Options{
		Policy: Policy(cfg),
		Backends: map[string]backend.Backend{
			config.BackendExec:   &backend.Exec{InheritEnv: cfg.Backend.InheritEnv, Stderr: stderr},
			config.BackendSocket: backend.Socket{},
		},
		Store:             st,
		Audit:             audit.New(auditW),
		AgentCapabilities: cfg.AgentCapabilities,
	})
	return rt, closeFn, nil
}
