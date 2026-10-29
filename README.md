# acp-router

A generic ACP-to-ACP router. Relays ([slack-acp](https://github.com/kfet/slack-acp), [poe-acp](https://github.com/kfet/poe-acp), zulip-acp) spawn it as their agent. A pluggable **routing policy** then picks which downstream ACP agent(s) get each request. The default policy runs one agent per user.

```
relay ──ACP/stdio──▶ acp-router ──policy──▶ agent for user A  (HOME, cwd, credentials of A)
                                 ├────────▶ agent for user B
                                 └────────▶ shared / sandboxed / … agents
```

## How it works

- **Core:** ACP proxy plumbing. It opens one connection per target and maps session ids between the relay and each target. The session table is persisted. The core forwards `session/update`, permission requests and other agent calls upstream. An agent can only speak for its own sessions. Cancel is propagated, and every request gets one JSON audit line.
- **Policy:** for each request (method, params, origin `_meta`, prior session state), it returns the target(s), or rejects the request, or rewrites it. The built-ins are `static` and `per_user`. Optional middleware handles the allowlist, session ownership, per-thread cwd, per-user credentials and prompt limits. You can write your own policy: RBAC, an HTTP/exec hook, an LLM. See [docs/policy.md](docs/policy.md).
- **Identity** comes from the origin `_meta` key `dev.acp-kit.origin/v1` = `{platform, team_id, user_id, channel_id, thread_id, message_id, is_dm, is_mention}`. Relays send it on `session/new`, `session/load`, `session/fork` and every `session/prompt`. The user key is `<platform>:<team_id>:<user_id>`.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/kfet/acp-router/main/install.sh | sh
# or: brew install kfet/ai/acp-router
```

Upgrade with `acp-router update`, then restart the relay so it respawns the router.

## Configure

Write `~/.config/acp-router/config.json` (full example: [docs/config.example.json](docs/config.example.json)):

```json
{
  "state_dir": "~/.local/state/acp-router",
  "backend": {"kind": "exec", "command": ["fir", "--mode", "acp"], "inherit_env": ["PATH"]},
  "policy": {"allow": ["slack:T0123:*"], "max_concurrent_prompts": 2},
  "users": {"slack:T0123:U0456": {"env_file": "~/.config/acp-router/users/U0456.env"}}
}
```

Then point the relay's agent command at `acp-router -config ~/.config/acp-router/config.json`.

| key | meaning |
|---|---|
| `state_dir` | session table (`sessions.json`), user homes and thread dirs |
| `audit_log` | audit JSONL file (default: stderr) |
| `backend.kind` | `exec` (default) or `socket` |
| `backend.command` | argv to spawn (`exec`). With `per_user`, the cwd and `HOME` are `<state_dir>/users/<user>` |
| `backend.inherit_env` | variables passed through from the router's env; nothing else is passed |
| `backend.env` | env for every agent |
| `backend.socket` | unix socket for `socket`. With `per_user`, `{user}` becomes the path-safe user key |
| `agent_capabilities` | overrides the `agentCapabilities` advertised upstream |
| `policy.route` | `per_user` (default) or `static` |
| `policy.allow` | allowed user keys. A trailing `*` matches a prefix, `"*"` matches everyone, and an empty list matches nobody |
| `policy.ownership` | (default true) only the creator may use a session |
| `policy.thread_cwd` | (default true) cwd = `<state_dir>/users/<user>/threads/<channel>/<thread>` |
| `policy.max_concurrent_prompts` | in-flight prompts per user (0 = no cap) |
| `users.<key>.env`, `env_file` | per-user env. `env_file` is `KEY=VALUE` lines, read when the agent is spawned |
| `users.<key>.max_concurrent_prompts` | per-user override |

## Extending

- **Routing / RBAC:** implement `proxy.Policy` or `proxy.Middleware`. See [docs/policy.md](docs/policy.md) for an example.
- **Backends:** implement `backend.Backend` (`Open(ctx, target, handler) (Peer, error)`). For example, a sandbox or exec-environment service. The built-in `socket` backend already dials a per-user socket served by such a service.

Set `ACP_ROUTER_DEBUG=1` for debug logging on stderr.

## Development

`make all` runs fmt, vet, race tests, a 100% coverage gate, cross-builds and the license check. See [AGENTS.md](AGENTS.md).

## License

MIT
