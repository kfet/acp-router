// Package config loads the acp-router JSON config.
package config

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Backend kinds.
const (
	BackendExec   = "exec"
	BackendSocket = "socket"
)

// Config is the whole router configuration.
type Config struct {
	// StateDir holds the session owner map and every user's home.
	StateDir string `json:"state_dir"`
	// AuditLog is a file the per-request audit lines are appended to.
	// Empty means stderr.
	AuditLog string `json:"audit_log,omitempty"`
	// Backend selects how a user's downstream agent is reached.
	Backend Backend `json:"backend"`
	// AgentCapabilities, when set, replaces the agentCapabilities the
	// router advertises upstream in its initialize response.
	AgentCapabilities json.RawMessage `json:"agent_capabilities,omitempty"`
	// Policy selects the routing policy and its middleware.
	Policy Policy `json:"policy"`
	// Users holds per-user overrides keyed by user key
	// ("<platform>:<team_id>:<user_id>").
	Users map[string]User `json:"users,omitempty"`
}

// Backend configures the downstream agent per user.
type Backend struct {
	// Kind is "exec" (default) or "socket".
	Kind string `json:"kind,omitempty"`
	// Command is the argv spawned per user (exec).
	Command []string `json:"command,omitempty"`
	// InheritEnv lists parent environment variables passed through to the
	// spawned agent (exec). Nothing else is inherited.
	InheritEnv []string `json:"inherit_env,omitempty"`
	// Env is set for every user's agent (exec), before per-user env.
	Env map[string]string `json:"env,omitempty"`
	// Socket is the unix socket (socket). With the per_user route,
	// "{user}" expands to the path-safe user key.
	Socket string `json:"socket,omitempty"`
}

// Routes.
const (
	RoutePerUser = "per_user"
	RouteStatic  = "static"
)

// Policy configures the built-in routing policy and middleware.
type Policy struct {
	// Route is "per_user" (default: one agent per origin user) or
	// "static" (one agent for everyone).
	Route string `json:"route,omitempty"`
	// Allow lists user keys allowed to use the router. An entry may end
	// in "*" to match a prefix; "*" alone allows everyone. Empty denies
	// everyone.
	Allow []string `json:"allow,omitempty"`
	// Ownership (default true) refuses requests on sessions the origin
	// user did not create.
	Ownership *bool `json:"ownership,omitempty"`
	// ThreadCwd (default true) replaces the relay's cwd with a per user +
	// thread directory under state_dir.
	ThreadCwd *bool `json:"thread_cwd,omitempty"`
	// MaxConcurrentPrompts caps each user's in-flight prompts (0 = no cap).
	MaxConcurrentPrompts int `json:"max_concurrent_prompts,omitempty"`
}

// On reports a default-true flag.
func On(b *bool) bool { return b == nil || *b }

// User holds per-user overrides.
type User struct {
	// Env is added to the user's agent environment (credentials, model
	// selection, ...).
	Env map[string]string `json:"env,omitempty"`
	// EnvFile is a KEY=VALUE file read at spawn time, applied after Env.
	EnvFile string `json:"env_file,omitempty"`
	// MaxConcurrentPrompts, when set, overrides
	// policy.max_concurrent_prompts for this user (0 = no cap).
	MaxConcurrentPrompts *int `json:"max_concurrent_prompts,omitempty"`
}

// Load reads and validates the config at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes and validates a config, rejecting unknown fields.
func Parse(b []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := c.normalize(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}

func (c *Config) normalize() error {
	if c.StateDir == "" {
		return errors.New("state_dir is required")
	}
	c.StateDir = ExpandHome(c.StateDir)
	if c.AuditLog != "" {
		c.AuditLog = ExpandHome(c.AuditLog)
	}
	if c.Backend.Kind == "" {
		c.Backend.Kind = BackendExec
	}
	switch c.Backend.Kind {
	case BackendExec:
		if len(c.Backend.Command) == 0 {
			return errors.New("backend.command is required for exec")
		}
	case BackendSocket:
		if c.Backend.Socket == "" {
			return errors.New("backend.socket is required for socket")
		}
		c.Backend.Socket = ExpandHome(c.Backend.Socket)
	default:
		return fmt.Errorf("unknown backend.kind %q", c.Backend.Kind)
	}
	switch c.Policy.Route {
	case "":
		c.Policy.Route = RoutePerUser
	case RoutePerUser, RouteStatic:
	default:
		return fmt.Errorf("unknown policy.route %q", c.Policy.Route)
	}
	if len(c.AgentCapabilities) > 0 {
		var m map[string]any
		if err := json.Unmarshal(c.AgentCapabilities, &m); err != nil {
			return errors.New("agent_capabilities must be a JSON object")
		}
	}
	for k, u := range c.Users {
		if u.EnvFile != "" {
			u.EnvFile = ExpandHome(u.EnvFile)
			c.Users[k] = u
		}
	}
	return nil
}

// ExpandHome expands a leading "~/" to the user's home directory.
func ExpandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, rest)
		}
	}
	return p
}

// ReadEnvFile parses KEY=VALUE lines; blank lines and # comments are
// skipped and an optional "export " prefix is accepted.
func ReadEnvFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("%s:%d: want KEY=VALUE", path, n)
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out, nil
}
