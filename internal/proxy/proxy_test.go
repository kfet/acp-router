package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-router/internal/audit"
	"github.com/kfet/acp-router/internal/backend"
	"github.com/kfet/acp-router/internal/origin"
	"github.com/kfet/acp-router/internal/store"
	"github.com/kfet/acp-router/internal/testagent"
)

var bg = context.Background()

// pipeBackend runs a testagent per target over in-memory pipes. Every
// agent numbers sessions "s-1", "s-2", ... unless prefix says otherwise,
// so different targets hand out colliding ids.
type pipeBackend struct {
	mu       sync.Mutex
	opened   map[string]int
	peers    map[string]*pipePeer
	prefix   map[string]string
	openErr  error
	gate     chan struct{}
	entered  chan struct{}
	initFail bool
}

type pipePeer struct {
	conn       *acp.Connection
	agent      *testagent.Agent
	toAgent    *io.PipeWriter
	fromAgent  *io.PipeWriter
	agentInput *io.PipeReader
}

func (p *pipePeer) Conn() *acp.Connection { return p.conn }
func (p *pipePeer) Done() <-chan struct{} { return p.conn.Done() }
func (p *pipePeer) Close() error {
	p.toAgent.Close()
	p.fromAgent.Close()
	<-p.conn.Done()
	return nil
}

func (b *pipeBackend) Open(_ context.Context, t backend.Target, h acp.MethodHandler) (backend.Peer, error) {
	if b.entered != nil {
		b.entered <- struct{}{}
	}
	if b.gate != nil {
		<-b.gate
	}
	if b.openErr != nil {
		return nil, b.openErr
	}
	ar, aw := io.Pipe()
	rr, rw := io.Pipe()
	prefix, ok := b.prefix[t.ID]
	if !ok {
		prefix = "s"
	}
	p := &pipePeer{toAgent: rw, fromAgent: aw, agentInput: rr}
	if b.initFail {
		rr.Close()
		aw.Close()
	} else {
		env := map[string]string{"HOME": t.Dir, "SECRET": t.Env["SECRET"]}
		p.agent = testagent.Serve(prefix, func(k string) string { return env[k] }, rr, aw)
	}
	p.conn = acp.NewConnection(h, rw, ar)
	b.mu.Lock()
	b.opened[t.ID]++
	b.peers[t.ID] = p
	b.mu.Unlock()
	return p, nil
}

func (b *pipeBackend) peer(id string) *pipePeer {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peers[id]
}

// client is the fake relay.
type client struct {
	conn     *acp.Connection
	mu       sync.Mutex
	updates  []string
	permFail atomic.Bool
}

func (c *client) handle(_ context.Context, method string, raw json.RawMessage) (any, *acp.RequestError) {
	switch method {
	case "session/update":
		var n acp.SessionNotification
		json.Unmarshal(raw, &n)
		c.mu.Lock()
		c.updates = append(c.updates, string(n.SessionId)+":"+n.Update.AgentMessageChunk.Content.Text.Text)
		c.mu.Unlock()
		return nil, nil
	case "session/request_permission":
		if c.permFail.Load() {
			return nil, acp.NewMethodNotFound(method)
		}
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected("ok")}, nil
	}
	return nil, acp.NewMethodNotFound(method)
}

func (c *client) Updates() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.updates...)
}

type harness struct {
	t     *testing.T
	rt    *Router
	be    *pipeBackend
	c     *client
	audit *bytes.Buffer
	st    *store.Store
	dir   string
	// policy is swappable per test.
	policy atomic.Pointer[Policy]
}

// byUser routes to target "t:<user_id>" from the origin, else the
// session's target, and labels the session with its user.
var byUser = PolicyFunc(func(_ context.Context, r *Request) (*Decision, error) {
	id := ""
	switch {
	case r.Session != nil:
		id = r.Session.Target
	case r.Origin != nil:
		id = "t:" + r.Origin.UserID
	default:
		return nil, errors.New("no route")
	}
	return &Decision{
		Targets: []backend.Target{{ID: id, Kind: "pipe", Dir: "/home/" + id, Env: map[string]string{"SECRET": id + "-key"}}},
		Labels:  map[string]string{"by": "test"},
	}, nil
})

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, be: &pipeBackend{opened: map[string]int{}, peers: map[string]*pipePeer{}}, audit: &bytes.Buffer{}, st: st, dir: dir}
	h.setPolicy(byUser)
	h.rt = New(Options{
		Policy:   PolicyFunc(func(ctx context.Context, r *Request) (*Decision, error) { return (*h.policy.Load()).Route(ctx, r) }),
		Backends: map[string]backend.Backend{"pipe": h.be},
		Store:    st,
		Audit:    audit.New(h.audit),
	})
	cr, rw := io.Pipe()
	rr, cw := io.Pipe()
	up := h.rt.Serve(rr, rw)
	h.c = &client{}
	h.c.conn = acp.NewConnection(h.c.handle, cw, cr)
	t.Cleanup(func() {
		h.rt.Close()
		cw.Close()
		rw.Close()
		<-up.Done()
	})
	return h
}

func (h *harness) setPolicy(p Policy) { h.policy.Store(&p) }

func org(user string) origin.Origin {
	return origin.Origin{Platform: "slack", TeamID: "T", UserID: user, ChannelID: "C", ThreadID: "t"}
}

func (h *harness) call(method string, params any) (map[string]any, error) {
	return acp.SendRequest[map[string]any](h.c.conn, bg, method, params)
}

func (h *harness) newSession(user string) (string, map[string]any) {
	h.t.Helper()
	res, err := h.call("session/new", map[string]any{"cwd": "/placeholder", "mcpServers": []any{}, "_meta": org(user).Meta()})
	if err != nil {
		h.t.Fatal(err)
	}
	return res["sessionId"].(string), res["_meta"].(map[string]any)
}

func prompt(sid, text string) map[string]any {
	o := org("x")
	return map[string]any{"sessionId": sid, "prompt": []acp.ContentBlock{acp.TextBlock(text)}, "_meta": o.Meta()}
}

func code(err error) int {
	var re *acp.RequestError
	if errors.As(err, &re) {
		return re.Code
	}
	return 0
}

func auditLines(t *testing.T, b *bytes.Buffer) []audit.Record {
	t.Helper()
	var out []audit.Record
	dec := json.NewDecoder(bytes.NewReader(b.Bytes()))
	for {
		var r audit.Record
		if dec.Decode(&r) != nil {
			return out
		}
		out = append(out, r)
	}
}

func TestFlowAndIDMapping(t *testing.T) {
	h := newHarness(t)
	init, err := h.call("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	if err != nil || init["agentCapabilities"].(map[string]any)["loadSession"] != true {
		t.Fatal(init, err)
	}
	if _, err := h.call("authenticate", map[string]any{"methodId": "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.call("session/list", map[string]any{}); code(err) != -32601 {
		t.Fatal(err)
	}

	asid, meta := h.newSession("alice")
	if asid != "s-1" || meta["home"] != "/home/t:alice" || meta["secret"] != "t:alice-key" || meta["cwd"] != "/placeholder" {
		t.Fatal(asid, meta)
	}
	// Bob's agent also says "s-1": the relay gets a distinct id.
	bsid, _ := h.newSession("bob")
	if bsid != "s-1@t:bob" {
		t.Fatal(bsid)
	}
	if ss, _ := h.st.Get(bsid); ss.Target != "t:bob" || ss.Downstream != "s-1" || ss.Labels["by"] != "test" {
		t.Fatalf("%+v", ss)
	}

	res, err := h.call("session/prompt", prompt(bsid, "hi"))
	if err != nil || res["stopReason"] != "end_turn" || res["_meta"].(map[string]any)["permission"] != "ok" {
		t.Fatal(res, err)
	}
	// The update came back under the relay's id, not the agent's.
	if u := h.c.Updates(); len(u) != 1 || u[0] != bsid+":echo: hi" {
		t.Fatal(u)
	}

	for _, m := range []string{"session/load", "session/resume", "session/set_mode"} {
		if _, err := h.call(m, map[string]any{"sessionId": asid, "cwd": "/x"}); err != nil {
			t.Fatal(m, err)
		}
	}
	fk, err := h.call("session/fork", map[string]any{"sessionId": asid, "cwd": "/x"})
	if err != nil || fk["sessionId"] != "s-2" {
		t.Fatal(fk, err)
	}
	if _, err := h.call("session/delete", map[string]any{"sessionId": "s-2"}); code(err) != -32601 {
		t.Fatal(err)
	}
	if _, ok := h.st.Get("s-2"); !ok {
		t.Fatal("a failed delete forgot the session")
	}
	if h.be.opened["t:alice"] != 1 || h.be.opened["t:bob"] != 1 {
		t.Fatal(h.be.opened)
	}
	lines := auditLines(t, h.audit)
	var promptLine audit.Record
	for _, l := range lines {
		if l.Method == "session/prompt" {
			promptLine = l
		}
	}
	if promptLine.Target != "t:bob" || promptLine.Session != bsid || promptLine.User != "slack:T:x" || promptLine.Outcome != audit.OK {
		t.Fatalf("%+v", promptLine)
	}
}

func TestAnnounceDuringCreateIsBuffered(t *testing.T) {
	h := newHarness(t)
	h.newSession("bob") // bob takes "s-1"
	o := org("alice")
	meta := o.Meta()
	meta["announce"] = true
	res, err := h.call("session/new", map[string]any{"cwd": "/", "_meta": meta})
	if err != nil {
		t.Fatal(err)
	}
	// Alice's agent announced "s-1" before the response; it reached the
	// relay under alice's upstream id, which bob's session forced to differ.
	if res["sessionId"] != "s-1@t:alice" {
		t.Fatal(res)
	}
	if u := h.c.Updates(); len(u) != 1 || u[0] != "s-1@t:alice:hello" {
		t.Fatal(u)
	}
}

func TestDownstreamUnknownSessionRefused(t *testing.T) {
	h := newHarness(t)
	sid, _ := h.newSession("alice")
	res, err := h.call("session/prompt", prompt(sid, "spoof:other"))
	if err != nil || res["_meta"].(map[string]any)["permission"] != "error" {
		t.Fatal(res, err)
	}
	if u := h.c.Updates(); len(u) != 0 {
		t.Fatal(u)
	}
	if !strings.Contains(h.audit.String(), "downstream:session/request_permission") {
		t.Fatal(h.audit.String())
	}
	// An upstream error on a forwarded request reaches the agent.
	h.c.permFail.Store(true)
	res, err = h.call("session/prompt", prompt(sid, "hi"))
	if err != nil || res["_meta"].(map[string]any)["permission"] != "error" {
		t.Fatal(res, err)
	}
}

func TestCancel(t *testing.T) {
	h := newHarness(t)
	sid, _ := h.newSession("alice")
	done := make(chan map[string]any)
	go func() {
		res, _ := h.call("session/prompt", prompt(sid, "block"))
		done <- res
	}()
	if err := h.c.conn.SendNotification(bg, "session/cancel", map[string]any{"sessionId": sid}); err != nil {
		t.Fatal(err)
	}
	if res := <-done; res["stopReason"] != "cancelled" {
		t.Fatal(res)
	}
}

func TestFanout(t *testing.T) {
	h := newHarness(t)
	h.be.prefix = map[string]string{"a": "A", "b": "B"}
	two := []backend.Target{{ID: "a", Kind: "pipe"}, {ID: "b", Kind: "pipe"}}
	h.setPolicy(PolicyFunc(func(context.Context, *Request) (*Decision, error) {
		return &Decision{Targets: two}, nil
	}))
	res, err := h.call("_ping", nil)
	if err != nil || res["prefix"] != "A" {
		t.Fatal(res, err)
	}
	for _, id := range []string{"a", "b"} {
		if m := h.be.peer(id).agent.Methods(); m[len(m)-1] != "_ping" {
			t.Fatal(id, m)
		}
	}
	if _, err := h.call("session/new", map[string]any{"cwd": "/"}); code(err) != -32603 {
		t.Fatal(err)
	}
	// Notifications go to every target.
	if _, rerr := h.rt.handle(bg, "document/didOpen", json.RawMessage(`{}`)); rerr != nil {
		t.Fatal(rerr)
	}
	lines := auditLines(t, h.audit)
	if l := lines[len(lines)-1]; l.Method != "document/didOpen" || l.Target != "a,b" || l.Outcome != audit.OK {
		t.Fatalf("%+v", l)
	}
}

func TestPolicyAnswers(t *testing.T) {
	h := newHarness(t)
	sid, _ := h.newSession("alice")
	cases := []struct {
		p    Policy
		code int
	}{
		{PolicyFunc(func(context.Context, *Request) (*Decision, error) {
			return nil, errors.Join(ErrRejected, errors.New("no"))
		}), CodeForbidden},
		{PolicyFunc(func(context.Context, *Request) (*Decision, error) {
			return nil, &acp.RequestError{Code: -32042, Message: "custom"}
		}), -32042},
		{PolicyFunc(func(context.Context, *Request) (*Decision, error) { return nil, errors.New("plain") }), -32603},
		{PolicyFunc(func(context.Context, *Request) (*Decision, error) { return &Decision{}, nil }), CodeForbidden},
		{PolicyFunc(func(context.Context, *Request) (*Decision, error) {
			return &Decision{Targets: []backend.Target{{ID: "elsewhere", Kind: "pipe"}}}, nil
		}), -32602},
	}
	for i, c := range cases {
		h.setPolicy(c.p)
		if _, err := h.call("session/prompt", prompt(sid, "hi")); code(err) != c.code {
			t.Errorf("case %d: %v", i, err)
		}
	}
	lines := auditLines(t, h.audit)
	if l := lines[len(lines)-1]; l.Outcome != audit.Error {
		t.Fatalf("%+v", l)
	}
	if !strings.Contains(h.audit.String(), `"outcome":"denied"`) {
		t.Fatal(h.audit.String())
	}
	// Release runs after the request.
	released := make(chan struct{})
	h.setPolicy(PolicyFunc(func(ctx context.Context, r *Request) (*Decision, error) {
		d, _ := byUser.Route(ctx, r)
		d.Release = func() { close(released) }
		return d, nil
	}))
	if _, err := h.call("session/prompt", prompt(sid, "hi")); err != nil {
		t.Fatal(err)
	}
	<-released
	// Policies may rewrite params.
	h.setPolicy(PolicyFunc(func(ctx context.Context, r *Request) (*Decision, error) {
		r.Params["cwd"] = json.RawMessage(`"/chosen"`)
		return byUser.Route(ctx, r)
	}))
	res, err := h.call("session/load", map[string]any{"sessionId": sid, "cwd": "/relay"})
	if err != nil || res["_meta"].(map[string]any)["cwd"] != "/chosen" {
		t.Fatal(res, err)
	}
}

func TestBadParams(t *testing.T) {
	h := newHarness(t)
	sid, _ := h.newSession("alice")
	cases := []struct {
		method string
		params any
		code   int
	}{
		{"session/new", []int{1}, -32602},
		{"session/set_mode", map[string]any{"sessionId": sid, "_meta": map[string]any{origin.Key: "bad"}}, -32602},
		{"session/set_mode", map[string]any{"sessionId": ""}, -32602},
		{"session/set_mode", map[string]any{"sessionId": 7}, -32602},
		{"session/prompt", prompt("nope", "hi"), CodeForbidden},
		{"session/prompt", prompt(sid, "fail"), -32603},
		{"_none", map[string]any{"_meta": map[string]any{"other": 1}}, -32603}, // no origin: byUser has no route
	}
	for _, c := range cases {
		if _, err := h.call(c.method, c.params); code(err) != c.code {
			t.Errorf("%s %v: %v", c.method, c.params, err)
		}
	}
}

func TestAgentRestart(t *testing.T) {
	h := newHarness(t)
	sid, _ := h.newSession("alice")
	h.be.peer("t:alice").Close()
	if _, err := h.call("session/load", map[string]any{"sessionId": sid, "cwd": "/"}); err != nil {
		t.Fatal(err)
	}
	if h.be.opened["t:alice"] != 2 {
		t.Fatal(h.be.opened)
	}
}

func TestOpenFailures(t *testing.T) {
	newReq := map[string]any{"cwd": "/", "_meta": org("alice").Meta()}

	h := newHarness(t)
	h.be.openErr = errors.New("boom")
	if _, err := h.call("session/new", newReq); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatal(err)
	}
	h.be.openErr = nil
	h.newSession("alice") // a failed open is retried

	h = newHarness(t)
	h.be.initFail = true
	if _, err := h.call("session/new", newReq); err == nil || !strings.Contains(err.Error(), "initialize") {
		t.Fatal(err)
	}

	h = newHarness(t)
	h.setPolicy(PolicyFunc(func(context.Context, *Request) (*Decision, error) {
		return &Decision{Targets: []backend.Target{{ID: "x", Kind: "nope"}}}, nil
	}))
	if _, err := h.call("session/new", newReq); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatal(err)
	}
	// Notifications surface open errors too.
	if _, rerr := h.rt.handle(bg, "document/didOpen", nil); rerr == nil {
		t.Fatal("notify to a missing backend succeeded")
	}
}

func TestCreateFailures(t *testing.T) {
	h := newHarness(t)
	h.be.prefix = map[string]string{"t:alice": ""}
	if _, err := h.call("session/new", map[string]any{"cwd": "/", "_meta": org("alice").Meta()}); code(err) != -32603 {
		t.Fatal(err)
	}
	if _, err := h.call("session/new", map[string]any{"cwd": "/", "_meta": org("bob").Meta()}); err != nil {
		t.Fatal(err)
	}
	// The relay's id and the first fallback are both taken.
	h.st.Put(store.Session{Upstream: "s-2"})
	h.st.Put(store.Session{Upstream: "s-2@t:bob"})
	res, err := h.call("session/new", map[string]any{"cwd": "/", "_meta": org("bob").Meta()})
	if err != nil || res["sessionId"] != "s-2@t:bob#2" {
		t.Fatal(res, err)
	}
	// The store cannot persist: the create fails.
	sub := filepath.Join(t.TempDir(), "sub")
	os.Mkdir(sub, 0o700)
	st, _ := store.Open(filepath.Join(sub, "s.json"))
	h.rt.opts.Store = st
	os.Remove(sub)
	os.WriteFile(sub, nil, 0o600)
	if _, err := h.call("session/new", map[string]any{"cwd": "/", "_meta": org("bob").Meta()}); err == nil || !strings.Contains(err.Error(), "record session") {
		t.Fatal(err)
	}
}

func TestConcurrentOpenWaits(t *testing.T) {
	h := newHarness(t)
	h.be.gate = make(chan struct{})
	h.be.entered = make(chan struct{}, 1)
	first := make(chan error)
	go func() {
		_, err := h.call("session/new", map[string]any{"cwd": "/", "_meta": org("alice").Meta()})
		first <- err
	}()
	<-h.be.entered
	tg := backend.Target{ID: "t:alice", Kind: "pipe"}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := h.rt.peer(ctx, tg); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	second := make(chan backend.Peer)
	go func() {
		p, _ := h.rt.peer(bg, tg)
		second <- p
	}()
	close(h.be.gate)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if p := <-second; p != h.be.peer("t:alice") {
		t.Fatal("different peer")
	}
}

func TestSendFailures(t *testing.T) {
	h := newHarness(t)
	sid, _ := h.newSession("alice")
	// Break the router -> agent pipe: forwarding a notification fails.
	h.be.peer("t:alice").agentInput.Close()
	raw, _ := json.Marshal(map[string]any{"sessionId": sid})
	if _, rerr := h.rt.handle(bg, "session/cancel", raw); rerr == nil {
		t.Fatal("cancel on a broken pipe succeeded")
	}
	// Break the router -> relay pipe: downstream forwarding fails.
	r2 := New(Options{Store: h.st})
	pr, pw := io.Pipe()
	pr.Close()
	r2.Serve(strings.NewReader(""), pw)
	down := r2.downstream("t:alice")
	if _, rerr := down(bg, "session/update", raw); rerr == nil {
		t.Fatal("notification on a broken pipe succeeded")
	}
	if _, rerr := down(bg, "session/request_permission", json.RawMessage(`{}`)); rerr == nil {
		t.Fatal("request on a broken pipe succeeded")
	}
}

// session/delete forgets the mapping, and a store failure surfaces.
func TestDelete(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "s.json"))
	st.Put(store.Session{Upstream: "u1", Target: "d", Downstream: "x1"})
	st.Put(store.Session{Upstream: "u2", Target: "d", Downstream: "x2"})
	be := &deleteBackend{}
	rt := New(Options{
		Store:    st,
		Backends: map[string]backend.Backend{"del": be},
		Policy: PolicyFunc(func(context.Context, *Request) (*Decision, error) {
			return &Decision{Targets: []backend.Target{{ID: "d", Kind: "del"}}}, nil
		}),
	})
	if _, rerr := rt.handle(bg, "session/delete", json.RawMessage(`{"sessionId":"u1"}`)); rerr != nil {
		t.Fatal(rerr)
	}
	if _, ok := st.Get("u1"); ok {
		t.Fatal("deleted session kept")
	}
	os.Remove(filepath.Join(dir, "s.json"))
	os.Mkdir(filepath.Join(dir, "s.json.tmp"), 0o700)
	if _, rerr := rt.handle(bg, "session/delete", json.RawMessage(`{"sessionId":"u2"}`)); rerr == nil {
		t.Fatal("store failure swallowed")
	}
	rt.Close()
}

// deleteBackend is an in-process agent that answers initialize and
// session/delete.
type deleteBackend struct{}

func (deleteBackend) Open(_ context.Context, _ backend.Target, h acp.MethodHandler) (backend.Peer, error) {
	ar, aw := io.Pipe()
	rr, rw := io.Pipe()
	acp.NewConnection(func(context.Context, string, json.RawMessage) (any, *acp.RequestError) {
		return map[string]any{}, nil
	}, aw, rr)
	return &pipePeer{conn: acp.NewConnection(h, rw, ar), toAgent: rw, fromAgent: aw, agentInput: rr}, nil
}

func TestWrapOrder(t *testing.T) {
	var order []string
	mw := func(name string) Middleware {
		return func(next Policy) Policy {
			return PolicyFunc(func(ctx context.Context, r *Request) (*Decision, error) {
				order = append(order, name)
				return next.Route(ctx, r)
			})
		}
	}
	p := Wrap(PolicyFunc(func(context.Context, *Request) (*Decision, error) {
		order = append(order, "base")
		return &Decision{}, nil
	}), mw("a"), mw("b"))
	p.Route(bg, &Request{})
	if strings.Join(order, ",") != "a,b,base" {
		t.Fatal(order)
	}
}

// Held notifications for another session still being created stay held.
func TestBufferKeepsOtherSessions(t *testing.T) {
	h := newHarness(t)
	other := held{method: "session/update", down: "other", obj: map[string]json.RawMessage{}}
	h.rt.bufMu.Lock()
	h.rt.creating["t:alice"] = 1 // a concurrent create still in flight
	h.rt.buf["t:alice"] = []held{other}
	h.rt.bufMu.Unlock()
	h.newSession("alice")
	h.rt.bufMu.Lock()
	defer h.rt.bufMu.Unlock()
	if b := h.rt.buf["t:alice"]; len(b) != 1 || b[0].down != "other" {
		t.Fatal(b)
	}
}
