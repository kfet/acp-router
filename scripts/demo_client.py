#!/usr/bin/env python3
"""Tiny ACP client that drives acp-router like a relay would.

Usage: demo_client.py <scenario> <router argv...>

Scenarios:
  policy    per-user routing, ownership, allowlist, prompt cap (testagent)
  bychannel the custom bychannel policy (testagent)
  fir       one real prompt per allowed user (fir --mode acp)

Every request carries origin _meta (dev.acp-kit.origin/v1), exactly as
slack-acp / zulip-acp send it. Stdlib only.
"""
import json
import subprocess
import sys
import threading

KEY = "dev.acp-kit.origin/v1"


def origin(user, channel="C-DEV", thread="T1", dm=False):
    o = {"platform": "slack", "team_id": "T1", "user_id": user,
         "channel_id": channel, "thread_id": thread}
    if dm:
        o["is_dm"] = True
    return {KEY: o}


class Client:
    def __init__(self, argv):
        self.p = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        self.n = 0
        self.lock = threading.Lock()
        self.pending = {}
        self.blocked = threading.Event()
        threading.Thread(target=self._read, daemon=True).start()

    def _send(self, msg):
        with self.lock:
            self.p.stdin.write(json.dumps(msg) + "\n")
            self.p.stdin.flush()

    def _read(self):
        for line in self.p.stdout:
            m = json.loads(line)
            if "method" in m and "id" in m:  # agent -> client request
                if m["method"] == "session/request_permission":
                    opt = m["params"]["options"][0]["optionId"]
                    print(f"  <- permission request on {m['params']['sessionId']}: allow {opt}")
                    self._send({"jsonrpc": "2.0", "id": m["id"],
                                "result": {"outcome": {"outcome": "selected", "optionId": opt}}})
                else:
                    self._send({"jsonrpc": "2.0", "id": m["id"],
                                "error": {"code": -32601, "message": "not supported by demo client"}})
            elif "method" in m:  # notification
                if m["method"] == "session/update":
                    u = m["params"]["update"]
                    if u.get("sessionUpdate") == "agent_message_chunk":
                        t = u["content"].get("text", "")
                        print(f"  <- update {m['params']['sessionId']}: {t!r}")
                        if t.startswith("blocked:"):
                            self.blocked.set()
            elif m.get("id") in self.pending:
                ev, box = self.pending.pop(m["id"])
                box.append(m)
                ev.set()
        for ev, box in list(self.pending.values()):
            box.append({"error": {"code": 0, "message": "router exited"}})
            ev.set()

    def start(self, method, params):
        with self.lock:
            self.n += 1
            i = self.n
        ev, box = threading.Event(), []
        self.pending[i] = (ev, box)
        self._send({"jsonrpc": "2.0", "id": i, "method": method, "params": params})
        return ev, box

    def call(self, method, params, timeout=300):
        ev, box = self.start(method, params)
        return wait(ev, box, timeout)

    def notify(self, method, params):
        self._send({"jsonrpc": "2.0", "method": method, "params": params})

    def close(self):
        self.p.stdin.close()
        self.p.wait()


def wait(ev, box, timeout=300):
    if not ev.wait(timeout):
        raise SystemExit("timeout")
    return box[0]


def show(label, r):
    if "error" in r:
        e = r["error"]
        print(f"{label}: ERROR {e['code']} {e['message']}")
    else:
        print(f"{label}: {json.dumps(r['result'], sort_keys=True)}")
    return r.get("result")


def text(s):
    return [{"type": "text", "text": s}]


def new(c, label, user, **kw):
    r = c.call("session/new", {"cwd": "/relay/cwd", "mcpServers": [], "_meta": origin(user, **kw)})
    res = r.get("result")
    if res:
        m = res.get("_meta", {})
        where = "".join(f" {k}={m[k]}" for k in ("home", "cwd") if k in m)  # testagent echoes these
        print(f"{label}: sessionId={res['sessionId']}{where}")
        return res["sessionId"]
    show(label, r)
    return None


def prompt(c, label, sid, user, msg, **kw):
    print(f"{label}: prompt {msg!r} on {sid}")
    r = c.call("session/prompt", {"sessionId": sid, "prompt": text(msg), "_meta": origin(user, **kw)})
    res = r.get("result")
    if res:
        print(f"  -> stopReason={res['stopReason']}")
    else:
        show("  ->", r)


def scenario_policy(c):
    print("== per_user routing: A and B in the same thread get separate agents")
    a = new(c, "A session/new", "userA")
    b = new(c, "B session/new", "userB")
    print("== allowlist: userC is not allowed")
    new(c, "C session/new", "userC")
    print("== prompts go to the owner's agent")
    prompt(c, "A", a, "userA", "hello from A")
    prompt(c, "B", b, "userB", "hello from B")
    print("== ownership: B prompts A's session")
    prompt(c, "B->A", a, "userB", "let me in")
    print("== max_concurrent_prompts=1: A's second in-flight prompt is refused")
    print(f"A: prompt 'block' on {a} (holds the slot until cancelled)")
    ev, box = c.start("session/prompt", {"sessionId": a, "prompt": text("block"), "_meta": origin("userA")})
    if not c.blocked.wait(60):  # the agent says it is blocked: the slot is taken
        raise SystemExit("timeout")
    prompt(c, "A (2nd)", a, "userA", "are you there?")
    c.notify("session/cancel", {"sessionId": a})
    r = wait(ev, box)
    print(f"A 'block': stopReason={r['result']['stopReason']} after session/cancel")
    prompt(c, "A (after release)", a, "userA", "slot free again")


def scenario_bychannel(c):
    print("== custom policy: C-SUPPORT goes to one shared agent")
    a = new(c, "A in C-SUPPORT", "userA", channel="C-SUPPORT")
    new(c, "B in C-SUPPORT", "userB", channel="C-SUPPORT")
    prompt(c, "A", a, "userA", "support question", channel="C-SUPPORT")
    print("== other channels fall through to per_user")
    new(c, "A in C-DEV", "userA")
    print("== DMs are refused by the custom policy")
    new(c, "A in a DM", "userA", channel="D1", dm=True)
    print("== built-in middleware still applies: userC is not allowed")
    new(c, "C in C-SUPPORT", "userC", channel="C-SUPPORT")


def scenario_fir(c):
    for user in ("userA", "userB"):
        sid = new(c, f"{user} session/new", user)
        prompt(c, user, sid, user, f"Reply with exactly the words: hello {user}")


def main():
    scen = {"policy": scenario_policy, "bychannel": scenario_bychannel, "fir": scenario_fir}[sys.argv[1]]
    c = Client(sys.argv[2:])
    r = c.call("initialize", {"protocolVersion": 1, "clientCapabilities": {}})
    print(f"initialize: protocolVersion={r['result']['protocolVersion']}")
    scen(c)
    c.close()


if __name__ == "__main__":
    main()
