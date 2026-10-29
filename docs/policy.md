# Routing policies

acp-router's core (`internal/proxy`) is ACP proxy plumbing only. Every
upstream request except `initialize` and `authenticate` goes to one
`proxy.Policy`. The policy decides where the request goes.

```go
type Policy interface {
	Route(ctx context.Context, r *proxy.Request) (*proxy.Decision, error)
}

type Request struct {
	Method  string
	Params  map[string]json.RawMessage // rewritable (e.g. cwd)
	Origin  *origin.Origin             // dev.acp-kit.origin/v1, nil if absent
	Session *store.Session             // prior state of params.sessionId: target, downstream id, labels
}

type Decision struct {
	Targets []backend.Target  // one, or several (fan-out: first answer wins)
	Labels  map[string]string // persisted on sessions this request creates
	Release func()            // called when the request finishes
}
```

- **Reject:** return an error that wraps `proxy.ErrRejected`. The relay gets JSON-RPC `-32003`. You can also return an `*acp.RequestError`, and it is sent back exactly as it is.
- **Transform:** change `r.Params` before you return.
- **Target:** a `backend.Target{ID, Kind, Command, Dir, Env, Socket}`. The core opens one agent connection per `ID` and reuses it. `Kind` selects the backend: `exec` or `socket` (built in), or your own `backend.Backend`.
- A request on an existing session must target that session's target. `session/new` and `session/fork` take exactly one target.

The core takes care of the rest:

- session-id mapping (each target has its own id space, and the relay never sees a collision)
- the persisted session table
- forwarding of `session/update`, permission requests and `fs/*` back upstream
- refusing an agent that names a session it does not serve
- cancel
- audit lines

## Built-ins (`internal/policy`)

| | |
|---|---|
| `Static(target)` | everything goes to one agent |
| `PerUser{Template, Root}` | one agent per origin user: `ID user:<key>`, `Dir <Root>/<key>`, `{user}` in the socket path |
| `Allowlist(keys)` | middleware: only listed users (`prefix*`, `*`) |
| `Ownership()` | middleware: origin is required on new/load/resume/fork/prompt, and only the creator can use a session |
| `PromptLimit(max, perUser)` | middleware: caps in-flight prompts per user |
| `ThreadCwd(root)` | middleware: replaces the relay's cwd with `<root>/<user>/threads/<channel>/<thread>` |
| `Credentials(envFunc)` | middleware: per-user env on the user's own target (`TargetID(user)`); shared targets get none |

Compose them with `proxy.Wrap(base, mw1, mw2, ...)`. The first middleware runs first.

## Example: a custom policy

[`examples/bychannel`](../examples/bychannel) is a runnable version of this,
and `internal/policy/example_test.go` exercises it. It sends a support
channel to one shared agent, refuses DMs, and leaves everything else to the
per-user default:

```go
type Policy struct {
	Support string
	Shared  backend.Target
	Next    proxy.Policy
}

func (p Policy) Route(ctx context.Context, r *proxy.Request) (*proxy.Decision, error) {
	if r.Session != nil { // sessions stay on the target that created them
		if r.Session.Target == p.Shared.ID {
			return p.shared(r)
		}
		return p.Next.Route(ctx, r)
	}
	if o := r.Origin; o != nil {
		if o.IsDM {
			return nil, fmt.Errorf("%w: DMs are not served here", proxy.ErrRejected)
		}
		if o.ChannelID == p.Support {
			return p.shared(r)
		}
	}
	return p.Next.Route(ctx, r)
}

// shared routes r to the shared agent. Its sessions get a cwd under its own
// Dir: a user's directory is that user's agent HOME and stays private.
func (p Policy) shared(r *proxy.Request) (*proxy.Decision, error) {
	if o := r.Origin; o != nil && policy.TakesCwd(r.Method) {
		dir := filepath.Join(p.Shared.Dir, "threads", policy.Safe(policy.UserKey(*o)), policy.Safe(o.ChannelID), policy.Safe(o.ThreadID))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		r.Params["cwd"], _ = json.Marshal(dir) // a string always marshals
	}
	return &proxy.Decision{Targets: []backend.Target{p.Shared}}, nil
}

route := proxy.Wrap(
	bychannel.Policy{Support: "C-SUPPORT", Shared: supportTarget, Next: policy.PerUser{Template: firTarget, Root: usersDir}},
	policy.Allowlist([]string{"slack:T1:*"}),
	policy.Ownership(),
)
```

To serve it, keep the stock flags, config and middleware and swap only the
base policy with `app.RunWith`. The packages are `internal`, so the binary
lives in this module, next to `examples/bychannel`. That is all
`examples/bychannel/cmd/acp-router-bychannel` does:

```go
os.Exit(app.RunWith(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, version, bychannel.Base(bychannel.Channel())))
```

`bychannel.Base` gets the config and the built-in base (`PerUser` or
`Static`) and returns `Policy{Next: def, ...}`. The allowlist, ownership,
prompt limit, thread cwd and credentials middleware still wrap it. The
support channel is `$BYCHANNEL_SUPPORT` (default `C-SUPPORT`).
[docs/demo.md](demo.md) runs it.

Other policies use the same pattern: an RBAC lookup, an HTTP or exec hook,
or a model that picks a target.

## Notes

- Policies are trusted code. A `Target` can name any command, directory or socket.
- `Target.Env` reaches an agent only when its target is opened. A shared target is opened once and serves every user, which is why `Credentials` leaves it alone. Rotated credentials take effect after that agent restarts.
- With fan-out, the first target is authoritative. Its answer goes to the relay, and the other targets' answers are dropped.
- `session/close` keeps the session mapping, so the session can still be loaded or resumed. A successful `session/delete` drops it.
- Every agent request forwarded upstream is audited as `downstream:<method>`. Requests that name a session the target does not serve are refused.
