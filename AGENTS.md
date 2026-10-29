Use idiomatic Go. Keep it simple.

Prefer `sync/atomic`, `sync.Once`, and channels over manual mutex management when appropriate.

Do not ignore any issues, address them promptly, even if preexisting. Do not postpone any work, even if it seems daunting — just break it down into smaller tasks. **Never dismiss a problem as "pre-existing" or "out of scope" — you own this entire codebase. If you see it, you fix it.**

Do not leave incomplete or stubbed code. Ensure all code is functional and tested.

## What this is

`acp-router` is a generic ACP-to-ACP router. Upstream, it is **one ACP agent** that a relay (`slack-acp`, `zulip-acp`, `poe-acp`) spawns over stdio. Downstream, it talks to whatever agents a **routing policy** picks. The default policy gives each user one agent (`fir --mode acp` per user, with its own HOME, cwd and env). See [docs/policy.md](docs/policy.md).

## Repository layout

```
cmd/acp-router/     entry point: `update` subcommand, else internal/app
internal/app/       flags + wiring: config → backends, policy chain, store, audit → proxy
internal/audit/     one JSON line per upstream request
internal/backend/   Target + Backend interface; Exec (spawn), Socket (dial)
internal/config/    JSON config loader (DisallowUnknownFields)
internal/dist/      release repo + asset naming (distkit + install.sh)
internal/origin/    VENDORED copy of acp-kit's origin _meta contract (see below)
internal/policy/    built-in policies (Static, PerUser) + middleware (Allowlist, Ownership, PromptLimit, ThreadCwd, Credentials)
internal/proxy/     the core: Policy interface, session-id mapping, forwarding, cancel, audit
internal/store/     persisted session table (upstream id → target, downstream id, labels)
internal/testagent/ scripted ACP agent for tests (coverage-excluded test support)
```

## Core vs policy — keep the split

The core (`internal/proxy`) makes **no** routing or authorization decisions. Everything about *who* and *where* goes in a `proxy.Policy` or `proxy.Middleware`: ownership, cwd choice, credentials, limits, allowlists, and any future RBAC, hook or model-based routing. If you are adding identity logic to `proxy`, stop and write middleware instead.

The core guarantees these, and they must not regress:

- **Session-id namespaces.** Each target has its own id space. The relay sees upstream ids (the downstream id, or `<id>@<target>` when that id is already taken). Ids are rewritten in both directions.
- **Downstream isolation.** An agent call that names a session its target does not serve is refused and audited (`downstream:<method>`). Notifications for a session still being created are held and flushed in order once the session is mapped.
- **Sessions stay put.** A request on a session must go to that session's target.
- **Stdout is ACP.** Diagnostics, agent stderr and the default audit log go to stderr.
- **Notifications vs requests.** The SDK hands both to one handler. `proxy.notifications` lists the methods that are forwarded as notifications.
- **No credentials on disk.** `Target.Env` is never persisted. Policies recompute it on each request.

Built-in middleware rules: the relay's cwd is opaque, and `ThreadCwd` replaces it. Identity only travels in `_meta`, never in prompt text. The Exec backend inherits only `backend.inherit_env`.

## Think before you specialise

Does this belong in [`acp-kit`](https://github.com/kfet/acp-kit)? **origin** is a vendored copy of `acp-kit/origin`. When acp-kit releases it, delete `internal/origin` and import `github.com/kfet/acp-kit/origin`. Do not let the copy drift.

## Build and test

`install.sh` is **generated** from `install.sh.json` via [distkit](https://github.com/kfet/distkit) (`make install.sh`); `make check-installsh` fails the build on drift. Never hand-edit it.

Always finish every task with `make all` (fmt, vet, race tests, 100% coverage gate, cross-builds, license check, no-leak check).

When fixing a regression, **write the test first** so it fails before your fix.

No personal hosts, IPs or domains anywhere in the tree: use `example.com`. Deploy config lives outside this repo.

## Testing — avoid wall-clock timeouts

Use channels and callbacks, not `time.Sleep` or `require.Eventually`. Proxy tests run `testagent`s over `io.Pipe`; backend and app tests re-exec the test binary as the agent (`TESTAGENT_HELPER=1`).

## Coverage

`make all` enforces 100% statement coverage over a profile filtered by `.covignore` (via `go tool covgate`). `.covignore` uses **file-level patterns only** — never line numbers or per-function regexes, and no `unreachable.go` grab-bags.

## Changelog

Add non-trivial changes under `## [Unreleased]` in `CHANGELOG.md` (`### Added` / `### Fixed` / `### Changed` / `### Removed`).
