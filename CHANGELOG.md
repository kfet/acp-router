# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added

- `app.RunWith` / `BuildWith` / `PolicyWith`: a binary in this module (the packages are internal) can replace the base routing policy and keep the configured middleware.
- `examples/bychannel`: a custom policy (a support channel goes to one shared agent, DMs are refused) with its `acp-router-bychannel` binary.
- `internal/testagent/cmd/acp-testagent`: the scripted test agent as a binary, for demos without an LLM.
- `scripts/demo.sh` and `docs/demo.md`: an end-to-end demo in a throwaway box (per-user routing, ownership, allowlist, prompt cap, audit, custom policy, real `fir --mode acp` turns).

### Fixed

- `Credentials` adds a user's env only to that user's own target (`policy.TargetID`), on a copy of its `Env`. Before, a shared target (`static`, or a custom policy's) was opened with the first caller's credentials and then served everyone. A custom policy's shared `Env` map could also be written into, which leaked one user's env to later users.
- `ThreadCwd` no longer creates the thread directory for a request the rest of the policy rejects. If an inner policy sets its own cwd, `ThreadCwd` keeps it (`policy.TakesCwd` tells a policy which methods carry one).
- The SDK's INFO line on every normal connection close no longer reaches stderr unless `ACP_ROUTER_DEBUG` is on (`app.QuietSDK`, called by the binaries).
- Config: with `policy.route` `static`, `users.<key>.env` / `env_file` is an error (use `backend.env`). The static agent is shared, so it never gets per-user env.

## [0.1.0] - 2026-10-29

### Added

- A generic ACP-to-ACP router. Upstream it is one ACP agent (stdio) for relays. Downstream it reaches the agents chosen by a pluggable routing policy.
- Core proxy plumbing:
  - per-target session-id mapping, with collision-free upstream ids
  - a persisted session table (`<state_dir>/sessions.json`)
  - forwarding of `session/update`, permission requests and other agent calls upstream
  - agents may only speak for their own sessions
  - notifications sent during `session/new`/`fork` are held until the session is mapped
  - cancel propagation
  - fan-out to several targets
  - one JSON audit line per request (`audit_log`, default stderr)
- `proxy.Policy` / `proxy.Middleware` interface. A policy sees the method, params, origin `_meta` and prior session state, and decides the target(s), or rejects or rewrites the request. See `docs/policy.md` for a runnable custom-policy example.
- Built-in policies: `static` and `per_user` (default; one agent per origin user). Middleware: allowlist, session ownership, per user+thread cwd, per-user credentials (`users.<key>.env` / `env_file`), and per-user prompt limits.
- Origin `_meta` contract `dev.acp-kit.origin/v1`. This is a vendored copy until acp-kit releases its `origin` package.
- Backends: `exec` spawns a command with its own HOME, cwd and env, inherits only `inherit_env`, and stops it with an EOF-then-kill grace period. `socket` dials a unix socket served by a sandbox or exec-environment service.
- `acp-router update` self-update (distkit), a generated `install.sh`, and a Homebrew formula via GoReleaser.
