# Demo: acp-router end to end

This demo runs acp-router in a throwaway box and drives it with
[`scripts/demo_client.py`](../scripts/demo_client.py), a small stdlib-only
ACP client that acts like a relay. Every `session/new` and `session/prompt`
carries origin `_meta` (`dev.acp-kit.origin/v1`), just as `slack-acp` and
`zulip-acp` send it. Three Slack users talk in the same thread (`C-DEV` /
`T1`). `userA` and `userB` are allowed, and `userC` is not.

The box is a fresh `mktemp -d /tmp/acp-router-demo.XXXXXX` (shown as
`$DEMO`) with its own `HOME`. Binaries, configs, state and audit logs all
live in it, so nothing outside it is read or written. The only exception is
the optional provider key file, which acp-router reads in place as the
users' `env_file`.

## Run it

```sh
scripts/demo.sh                                   # phases 1-2: testagent, no LLM, no cost
DEMO_FIR_ENV_FILE=~/keys.env scripts/demo.sh      # + phase 3: one real fir turn per user
```

`~/keys.env` holds `OPENROUTER_API_KEY=...`. You can change the provider
and model with `DEMO_FIR_PROVIDER` and `DEMO_FIR_MODEL` (the defaults are
`openrouter` and `google/gemini-2.5-flash-lite`). `DEMO_DIR` pins the box
directory.

The script builds three binaries into `$DEMO/bin`:

| binary | what |
|---|---|
| `acp-router` | the stock router |
| `acp-testagent` | `internal/testagent` over stdio. It echoes prompts, asks one permission, and answers `block` by waiting for `session/cancel`. Its `session/new` reply echoes the `HOME` and `cwd` it got. |
| `acp-router-bychannel` | the stock router with the [`examples/bychannel`](../examples/bychannel) policy as its base ([docs/policy.md](policy.md)) |

## What to look for

**Phase 1: built-in `per_user` policy with testagent backends.**

- **Per-user routing.** `userA` and `userB` each get their own agent process, with target `user:slack:T1:<user>`, `HOME=$DEMO/policy/users/<user>` and `cwd=.../users/<user>/threads/C-DEV/T1`. The relay sent `/relay/cwd`, and `ThreadCwd` replaced it.
- **Session-id namespaces.** Both agents call their first session `ta-1`. The relay sees `ta-1` for `userA` and `ta-1@user:slack:T1:userB` for `userB`.
- **Allowlist.** `userC` gets `-32003 forbidden`, and the audit line says `user "slack:T1:userC" is not allowed`.
- **Ownership.** When `userB` prompts `userA`'s session, the request is refused (`session ta-1 is not owned by slack:T1:userB`) and never reaches an agent.
- **`max_concurrent_prompts`.** `userA` has a per-user override of 1. While a `block` prompt is in flight (the agent says `blocked: ...`), a second prompt is refused with `has 1 prompts in flight`. After `session/cancel` the first prompt ends `cancelled`, and the next prompt goes through.
- **Audit.** There is one JSON line per upstream request, including the denied ones, plus `downstream:session/request_permission` for each agent-to-client call.

**Phase 2: a custom policy (`examples/bychannel`).** The config is the same, but the binary is different.

- `C-SUPPORT` sessions from both users go to one shared target, `support` (`HOME=$DEMO/bychannel/support`). There is one agent process, so its ids are `ta-1` and `ta-2`.
- The shared sessions get a cwd under `$DEMO/bychannel/support/threads/<user>/...`, not under `users/<user>/`, which is each user's private agent HOME. The policy sets its own cwd, and `ThreadCwd` leaves it alone.
- `C-DEV` falls through to the built-in `per_user` (`user:slack:T1:userA`).
- DMs are refused by the custom policy (`DMs are not served here`). No thread directory is created for them.
- The built-in middleware still wraps the custom base, so `userC` is still rejected by the allowlist.

**Phase 3: real `fir --mode acp`.** There is one turn per user, and each turn runs in that user's own fir process. Each fir keeps its sessions under its own `HOME` (`users/<user>/.config/fir/sessions/...`), in a directory named after its thread cwd.

## Captured output

Captured with `DEMO_FIR_ENV_FILE` set. `$DEMO` stands for the box directory.

```text
box: $DEMO=/tmp/ard-final (HOME=$DEMO/home)
router: v0.1.0-7-gd25f741-dirty

## 1. built-in policy, testagent backends

### config policy.json
{
  "state_dir": "$DEMO/policy",
  "audit_log": "$DEMO/policy/audit.jsonl",
  "backend": {"kind": "exec", "command": ["$DEMO/bin/acp-testagent"], "inherit_env": ["PATH"]},
  "policy": {
    "route": "per_user",
    "allow": ["slack:T1:userA", "slack:T1:userB"],
    "ownership": true,
    "thread_cwd": true,
    "max_concurrent_prompts": 2
  },
  "users": {"slack:T1:userA": {"max_concurrent_prompts": 1}, "slack:T1:userB": {}}
}

### client
initialize: protocolVersion=1
== per_user routing: A and B in the same thread get separate agents
A session/new: sessionId=ta-1 home=$DEMO/policy/users/slack:T1:userA cwd=$DEMO/policy/users/slack:T1:userA/threads/C-DEV/T1
B session/new: sessionId=ta-1@user:slack:T1:userB home=$DEMO/policy/users/slack:T1:userB cwd=$DEMO/policy/users/slack:T1:userB/threads/C-DEV/T1
== allowlist: userC is not allowed
C session/new: ERROR -32003 forbidden
== prompts go to the owner's agent
A: prompt 'hello from A' on ta-1
  <- permission request on ta-1: allow ok
  <- update ta-1: 'echo: hello from A'
  -> stopReason=end_turn
B: prompt 'hello from B' on ta-1@user:slack:T1:userB
  <- permission request on ta-1@user:slack:T1:userB: allow ok
  <- update ta-1@user:slack:T1:userB: 'echo: hello from B'
  -> stopReason=end_turn
== ownership: B prompts A's session
B->A: prompt 'let me in' on ta-1
  ->: ERROR -32003 forbidden
== max_concurrent_prompts=1: A's second in-flight prompt is refused
A: prompt 'block' on ta-1 (holds the slot until cancelled)
  <- update ta-1: 'blocked: waiting for cancel'
A (2nd): prompt 'are you there?' on ta-1
  ->: ERROR -32003 forbidden
A 'block': stopReason=cancelled after session/cancel
A (after release): prompt 'slot free again' on ta-1
  <- permission request on ta-1: allow ok
  <- update ta-1: 'echo: slot free again'
  -> stopReason=end_turn

### state tree
./users
./users/slack:T1:userA
./users/slack:T1:userA/threads
./users/slack:T1:userA/threads/C-DEV
./users/slack:T1:userA/threads/C-DEV/T1
./users/slack:T1:userB
./users/slack:T1:userB/threads
./users/slack:T1:userB/threads/C-DEV
./users/slack:T1:userB/threads/C-DEV/T1

### audit.jsonl
{"time":"2026-10-29T08:44:00.17735203Z","method":"initialize","outcome":"ok","duration_ms":0.078}
{"time":"2026-10-29T08:44:00.177993476Z","method":"session/new","user":"slack:T1:userA","session":"ta-1","target":"user:slack:T1:userA","channel":"C-DEV","thread":"T1","outcome":"ok","duration_ms":6.571}
{"time":"2026-10-29T08:44:00.185197516Z","method":"session/new","user":"slack:T1:userB","session":"ta-1@user:slack:T1:userB","target":"user:slack:T1:userB","channel":"C-DEV","thread":"T1","outcome":"ok","duration_ms":10.37}
{"time":"2026-10-29T08:44:00.19725933Z","method":"session/new","user":"slack:T1:userC","channel":"C-DEV","thread":"T1","outcome":"denied","error":"{\"code\":-32003,\"message\":\"forbidden\",\"data\":{\"error\":\"rejected: user \\\"slack:T1:userC\\\" is not allowed\"}}","duration_ms":0.093}
{"time":"2026-10-29T08:44:00.198240133Z","method":"downstream:session/request_permission","session":"ta-1","target":"user:slack:T1:userA","outcome":"ok","duration_ms":0}
{"time":"2026-10-29T08:44:00.197637365Z","method":"session/prompt","user":"slack:T1:userA","session":"ta-1","target":"user:slack:T1:userA","channel":"C-DEV","thread":"T1","outcome":"ok","duration_ms":0.987}
{"time":"2026-10-29T08:44:00.200423577Z","method":"downstream:session/request_permission","session":"ta-1@user:slack:T1:userB","target":"user:slack:T1:userB","outcome":"ok","duration_ms":0}
{"time":"2026-10-29T08:44:00.198806318Z","method":"session/prompt","user":"slack:T1:userB","session":"ta-1@user:slack:T1:userB","target":"user:slack:T1:userB","channel":"C-DEV","thread":"T1","outcome":"ok","duration_ms":3.38}
{"time":"2026-10-29T08:44:00.202388412Z","method":"session/prompt","user":"slack:T1:userB","session":"ta-1","channel":"C-DEV","thread":"T1","outcome":"denied","error":"{\"code\":-32003,\"message\":\"forbidden\",\"data\":{\"error\":\"rejected: session ta-1 is not owned by slack:T1:userB\"}}","duration_ms":0.051}
{"time":"2026-10-29T08:44:00.205895796Z","method":"session/prompt","user":"slack:T1:userA","session":"ta-1","channel":"C-DEV","thread":"T1","outcome":"denied","error":"{\"code\":-32003,\"message\":\"forbidden\",\"data\":{\"error\":\"rejected: user slack:T1:userA has 1 prompts in flight\"}}","duration_ms":0.05}
{"time":"2026-10-29T08:44:00.206074922Z","method":"session/cancel","session":"ta-1","target":"user:slack:T1:userA","outcome":"ok","duration_ms":0.045}
{"time":"2026-10-29T08:44:00.20299Z","method":"session/prompt","user":"slack:T1:userA","session":"ta-1","target":"user:slack:T1:userA","channel":"C-DEV","thread":"T1","outcome":"ok","duration_ms":3.333}
{"time":"2026-10-29T08:44:00.209206694Z","method":"downstream:session/request_permission","session":"ta-1","target":"user:slack:T1:userA","outcome":"ok","duration_ms":0}
{"time":"2026-10-29T08:44:00.20649928Z","method":"session/prompt","user":"slack:T1:userA","session":"ta-1","target":"user:slack:T1:userA","channel":"C-DEV","thread":"T1","outcome":"ok","duration_ms":4.299}

## 2. custom policy: examples/bychannel

### config bychannel.json
{
  "state_dir": "$DEMO/bychannel",
  "audit_log": "$DEMO/bychannel/audit.jsonl",
  "backend": {"kind": "exec", "command": ["$DEMO/bin/acp-testagent"], "inherit_env": ["PATH"]},
  "policy": {
    "route": "per_user",
    "allow": ["slack:T1:userA", "slack:T1:userB"],
    "ownership": true,
    "thread_cwd": true,
    "max_concurrent_prompts": 2
  },
  "users": {"slack:T1:userA": {"max_concurrent_prompts": 1}, "slack:T1:userB": {}}
}

### client
initialize: protocolVersion=1
== custom policy: C-SUPPORT goes to one shared agent
A in C-SUPPORT: sessionId=ta-1 home=$DEMO/bychannel/support cwd=$DEMO/bychannel/support/threads/slack:T1:userA/C-SUPPORT/T1
B in C-SUPPORT: sessionId=ta-2 home=$DEMO/bychannel/support cwd=$DEMO/bychannel/support/threads/slack:T1:userB/C-SUPPORT/T1
A: prompt 'support question' on ta-1
  <- permission request on ta-1: allow ok
  <- update ta-1: 'echo: support question'
  -> stopReason=end_turn
== other channels fall through to per_user
A in C-DEV: sessionId=ta-1@user:slack:T1:userA home=$DEMO/bychannel/users/slack:T1:userA cwd=$DEMO/bychannel/users/slack:T1:userA/threads/C-DEV/T1
== DMs are refused by the custom policy
A in a DM: ERROR -32003 forbidden
== built-in middleware still applies: userC is not allowed
C in C-SUPPORT: ERROR -32003 forbidden

### state tree
./support
./support/threads
./support/threads/slack:T1:userA
./support/threads/slack:T1:userA/C-SUPPORT
./support/threads/slack:T1:userA/C-SUPPORT/T1
./support/threads/slack:T1:userB
./support/threads/slack:T1:userB/C-SUPPORT
./support/threads/slack:T1:userB/C-SUPPORT/T1
./users
./users/slack:T1:userA
./users/slack:T1:userA/threads
./users/slack:T1:userA/threads/C-DEV
./users/slack:T1:userA/threads/C-DEV/T1

### audit.jsonl
{"time":"2026-10-29T08:44:00.330247023Z","method":"initialize","outcome":"ok","duration_ms":0.041}
{"time":"2026-10-29T08:44:00.332580061Z","method":"session/new","user":"slack:T1:userA","session":"ta-1","target":"support","channel":"C-SUPPORT","thread":"T1","outcome":"ok","duration_ms":7.223}
{"time":"2026-10-29T08:44:00.340180723Z","method":"session/new","user":"slack:T1:userB","session":"ta-2","target":"support","channel":"C-SUPPORT","thread":"T1","outcome":"ok","duration_ms":0.641}
{"time":"2026-10-29T08:44:00.341598477Z","method":"downstream:session/request_permission","session":"ta-1","target":"support","outcome":"ok","duration_ms":0}
{"time":"2026-10-29T08:44:00.34102777Z","method":"session/prompt","user":"slack:T1:userA","session":"ta-1","target":"support","channel":"C-SUPPORT","thread":"T1","outcome":"ok","duration_ms":0.912}
{"time":"2026-10-29T08:44:00.342084008Z","method":"session/new","user":"slack:T1:userA","session":"ta-1@user:slack:T1:userA","target":"user:slack:T1:userA","channel":"C-DEV","thread":"T1","outcome":"ok","duration_ms":14.244}
{"time":"2026-10-29T08:44:00.357341818Z","method":"session/new","user":"slack:T1:userA","channel":"D1","thread":"T1","outcome":"denied","error":"{\"code\":-32003,\"message\":\"forbidden\",\"data\":{\"error\":\"rejected: DMs are not served here\"}}","duration_ms":0.088}
{"time":"2026-10-29T08:44:00.357635029Z","method":"session/new","user":"slack:T1:userC","channel":"C-SUPPORT","thread":"T1","outcome":"denied","error":"{\"code\":-32003,\"message\":\"forbidden\",\"data\":{\"error\":\"rejected: user \\\"slack:T1:userC\\\" is not allowed\"}}","duration_ms":0.052}

## 3. real fir --mode acp, one turn per user

### config fir.json
{
  "state_dir": "$DEMO/fir",
  "audit_log": "$DEMO/fir/audit.jsonl",
  "backend": {"kind": "exec", "command": ["fir", "--mode", "acp", "--provider", "openrouter", "--model", "google/gemini-2.5-flash-lite"], "inherit_env": ["PATH", "LANG"]},
  "policy": {
    "route": "per_user",
    "allow": ["slack:T1:userA", "slack:T1:userB"],
    "ownership": true,
    "thread_cwd": true,
    "max_concurrent_prompts": 2
  },
  "users": {"slack:T1:userA": {"env_file": "$DEMO_FIR_ENV_FILE"}, "slack:T1:userB": {"env_file": "$DEMO_FIR_ENV_FILE"}}
}

### client
initialize: protocolVersion=1
userA session/new: sessionId=2568c91f-a839-4113-8979-2e911ace6038
userA: prompt 'Reply with exactly the words: hello userA' on 2568c91f-a839-4113-8979-2e911ace6038
  <- update 2568c91f-a839-4113-8979-2e911ace6038: 'hello'
  <- update 2568c91f-a839-4113-8979-2e911ace6038: ' userA'
  -> stopReason=end_turn
userB session/new: sessionId=205ec164-cb95-4b34-8939-744373a94d62
userB: prompt 'Reply with exactly the words: hello userB' on 205ec164-cb95-4b34-8939-744373a94d62
  <- update 205ec164-cb95-4b34-8939-744373a94d62: 'hello'
  <- update 205ec164-cb95-4b34-8939-744373a94d62: ' userB'
  -> stopReason=end_turn

### state tree
./users
./users/slack:T1:userA
./users/slack:T1:userA/threads
./users/slack:T1:userA/threads/C-DEV
./users/slack:T1:userA/threads/C-DEV/T1
./users/slack:T1:userB
./users/slack:T1:userB/threads
./users/slack:T1:userB/threads/C-DEV
./users/slack:T1:userB/threads/C-DEV/T1

### audit.jsonl
{"time":"2026-10-29T08:44:00.440756487Z","method":"initialize","outcome":"ok","duration_ms":0.022}
{"time":"2026-10-29T08:44:00.441370239Z","method":"session/new","user":"slack:T1:userA","session":"2568c91f-a839-4113-8979-2e911ace6038","target":"user:slack:T1:userA","channel":"C-DEV","thread":"T1","outcome":"ok","duration_ms":704.595}
{"time":"2026-10-29T08:44:01.149653781Z","method":"session/prompt","user":"slack:T1:userA","session":"2568c91f-a839-4113-8979-2e911ace6038","target":"user:slack:T1:userA","channel":"C-DEV","thread":"T1","outcome":"ok","duration_ms":1464.625}
{"time":"2026-10-29T08:44:02.614721126Z","method":"session/new","user":"slack:T1:userB","session":"205ec164-cb95-4b34-8939-744373a94d62","target":"user:slack:T1:userB","channel":"C-DEV","thread":"T1","outcome":"ok","duration_ms":731.568}
{"time":"2026-10-29T08:44:03.35333291Z","method":"session/prompt","user":"slack:T1:userB","session":"205ec164-cb95-4b34-8939-744373a94d62","target":"user:slack:T1:userB","channel":"C-DEV","thread":"T1","outcome":"ok","duration_ms":1938.108}

### each user's fir keeps its sessions in its own HOME, keyed by its thread cwd
users/slack:T1:userA/.config/fir/sessions/--tmp-ard-final-fir-users-slack-T1-userA-threads-C-DEV-T1--
users/slack:T1:userB/.config/fir/sessions/--tmp-ard-final-fir-users-slack-T1-userB-threads-C-DEV-T1--
```
