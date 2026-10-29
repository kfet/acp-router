// Package proxy is the acp-router core: ACP proxy plumbing between one
// upstream relay connection and any number of downstream agents.
//
// The core decides nothing about who may do what or where a request goes.
// Every routed request is handed to a Policy, which returns the target
// agent(s) (or rejects, or rewrites the params). The core then does the
// plumbing: opens and reuses one connection per target, maps session ids
// between the relay and each target's own id space, persists that mapping,
// forwards session/update, permission requests and every other agent call
// back upstream, propagates cancel, and writes one audit line per request.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-router/internal/audit"
	"github.com/kfet/acp-router/internal/backend"
	"github.com/kfet/acp-router/internal/origin"
	"github.com/kfet/acp-router/internal/store"
)

// CodeForbidden is the JSON-RPC error code for policy rejections.
const CodeForbidden = -32003

// DefaultAgentCapabilities is advertised upstream unless overridden.
const DefaultAgentCapabilities = `{"loadSession":true,"promptCapabilities":{"image":true,"embeddedContext":true},"sessionCapabilities":{"fork":{},"resume":{}}}`

// UserKey is the identity of an origin: "<platform>:<team_id>:<user_id>".
func UserKey(o origin.Origin) string { return o.Platform + ":" + o.TeamID + ":" + o.UserID }

// ErrRejected marks a policy refusal; wrap it to reject with CodeForbidden.
var ErrRejected = errors.New("rejected")

// Request is one upstream request as a Policy sees it.
type Request struct {
	Method string
	// Params is the decoded top-level params object. A policy may rewrite
	// it (e.g. cwd); the core forwards what is left, after mapping
	// sessionId to the target's id.
	Params map[string]json.RawMessage
	// Origin is the relay's origin _meta, nil when absent.
	Origin *origin.Origin
	// Session is the recorded state of params.sessionId, nil when the
	// request names no session. A request naming an unknown session is
	// refused before any policy runs.
	Session *store.Session
}

// Decision is a Policy's answer.
type Decision struct {
	// Targets receive the request. With several, every target gets it and
	// the first target is authoritative: its response (or error) is
	// returned, the others' are dropped (fan-out / shadowing). A
	// request on an existing session must target that session's target;
	// session/new and session/fork take exactly one target.
	Targets []backend.Target
	// Labels are persisted with a session the request creates.
	Labels map[string]string
	// Release, when set, is called once the request has finished.
	Release func()
}

// Policy routes upstream requests. Implementations may be anything:
// static config, per-user mapping, RBAC, an external hook, a model.
// Return an error wrapping ErrRejected to refuse, or an *acp.RequestError
// to answer with that exact error.
type Policy interface {
	Route(ctx context.Context, r *Request) (*Decision, error)
}

// PolicyFunc adapts a function to Policy.
type PolicyFunc func(ctx context.Context, r *Request) (*Decision, error)

// Route implements Policy.
func (f PolicyFunc) Route(ctx context.Context, r *Request) (*Decision, error) { return f(ctx, r) }

// Middleware wraps a Policy (ownership checks, cwd choice, credentials,
// limits, ...).
type Middleware func(Policy) Policy

// Wrap applies mws to p; the first middleware runs first.
func Wrap(p Policy, mws ...Middleware) Policy {
	for i := len(mws) - 1; i >= 0; i-- {
		p = mws[i](p)
	}
	return p
}

var (
	createsSession = map[string]bool{"session/new": true, "session/fork": true}
	// notifications are forwarded without waiting for a response. The
	// SDK hands requests and notifications to the same handler, so the
	// core has to know which is which.
	notifications = map[string]bool{
		"session/cancel": true, "session/update": true, "_dev.acp-kit/notice": true,
		"document/didChange": true, "document/didClose": true, "document/didFocus": true,
		"document/didOpen": true, "document/didSave": true,
	}
)

// Options wires a Router.
type Options struct {
	Policy Policy
	// Backends by Target.Kind.
	Backends map[string]backend.Backend
	Store    *store.Store
	// Audit may be nil.
	Audit *audit.Logger
	// AgentCapabilities overrides DefaultAgentCapabilities when set.
	AgentCapabilities json.RawMessage
}

// Router proxies one upstream ACP connection to policy-chosen targets.
type Router struct {
	opts Options

	up    atomic.Pointer[acp.Connection]
	upSet chan struct{}

	initParams atomic.Pointer[json.RawMessage]

	mu    sync.Mutex
	peers map[string]*peerEntry

	// bufMu orders session-id mapping against downstream forwarding.
	// While a target has session/new or session/fork in flight, its
	// notifications for a not-yet-mapped session are held in buf and
	// flushed, in order, once the mapping exists.
	bufMu    sync.Mutex
	creating map[string]int
	buf      map[string][]held
}

type peerEntry struct {
	ready chan struct{}
	peer  backend.Peer
	err   error
}

type held struct {
	method string
	down   string
	obj    map[string]json.RawMessage
}

// New builds a Router.
func New(opts Options) *Router {
	if len(opts.AgentCapabilities) == 0 {
		opts.AgentCapabilities = json.RawMessage(DefaultAgentCapabilities)
	}
	return &Router{
		opts: opts, upSet: make(chan struct{}), peers: map[string]*peerEntry{},
		creating: map[string]int{}, buf: map[string][]held{},
	}
}

// Serve speaks ACP (agent side) to the relay over r/w and returns the
// upstream connection; it is Done when the relay goes away.
func (rt *Router) Serve(r io.Reader, w io.Writer) *acp.Connection {
	c := acp.NewConnection(rt.handle, w, r)
	rt.up.Store(c)
	close(rt.upSet)
	return c
}

// Close shuts every downstream connection down.
func (rt *Router) Close() {
	rt.mu.Lock()
	ps := rt.peers
	rt.peers = map[string]*peerEntry{}
	rt.mu.Unlock()
	// Concurrently: each agent may take its whole shutdown grace.
	var wg sync.WaitGroup
	for _, pe := range ps {
		wg.Go(func() {
			<-pe.ready
			if pe.peer != nil {
				_ = pe.peer.Close()
			}
		})
	}
	wg.Wait()
}

func forbidden(err error) *acp.RequestError {
	return &acp.RequestError{Code: CodeForbidden, Message: "forbidden", Data: map[string]any{"error": err.Error()}}
}

func toReqErr(err error) *acp.RequestError {
	// Only an unwrapped RequestError is a peer's or policy's own answer
	// and passes through verbatim; a wrapped one carries context.
	if re, ok := err.(*acp.RequestError); ok { //nolint:errorlint // deliberate: unwrapped only
		return re
	}
	if errors.Is(err, ErrRejected) {
		return forbidden(err)
	}
	return acp.NewInternalError(map[string]any{"error": err.Error()})
}

func (rt *Router) handle(ctx context.Context, method string, raw json.RawMessage) (any, *acp.RequestError) {
	start := time.Now()
	rec := audit.Record{Time: start.UTC(), Method: method}
	res, rerr := rt.route(ctx, method, raw, &rec)
	rec.Duration = float64(time.Since(start).Microseconds()) / 1000
	switch {
	case rerr == nil:
		rec.Outcome = audit.OK
	case rerr.Code == CodeForbidden:
		rec.Outcome, rec.Error = audit.Denied, rerr.Error()
	default:
		rec.Outcome, rec.Error = audit.Error, rerr.Error()
	}
	rt.opts.Audit.Log(rec)
	return res, rerr
}

func (rt *Router) route(ctx context.Context, method string, raw json.RawMessage, rec *audit.Record) (any, *acp.RequestError) {
	switch method {
	case "initialize":
		cp := append(json.RawMessage(nil), raw...)
		rt.initParams.Store(&cp)
		return map[string]any{
			"protocolVersion":   acp.ProtocolVersionNumber,
			"agentCapabilities": rt.opts.AgentCapabilities,
			"authMethods":       []any{},
		}, nil
	case "authenticate":
		// Downstream credentials are a policy concern, never the relay's.
		return map[string]any{}, nil
	case "session/list":
		// Would mix every target's sessions; not offered.
		return nil, acp.NewMethodNotFound(method)
	}

	req := &Request{Method: method, Params: map[string]json.RawMessage{}}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &req.Params); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": "params must be an object"})
		}
	}
	if m, ok := req.Params["_meta"]; ok {
		var meta any
		_ = json.Unmarshal(m, &meta) // already valid JSON: it came out of an object
		switch o, err := origin.FromMeta(meta); {
		case err == nil:
			req.Origin = &o
			rec.User, rec.Channel, rec.Thread, rec.Message = UserKey(o), o.ChannelID, o.ThreadID, o.MessageID
		case !errors.Is(err, origin.ErrMissing):
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}
	}
	var sid string
	if s, ok := req.Params["sessionId"]; ok {
		_ = json.Unmarshal(s, &sid)
		if sid == "" {
			return nil, acp.NewInvalidParams(map[string]any{"error": "sessionId must be a non-empty string"})
		}
		ss, ok := rt.opts.Store.Get(sid)
		if !ok {
			return nil, forbidden(fmt.Errorf("unknown session %s", sid))
		}
		req.Session = &ss
		rec.Session = sid
	}

	dec, err := rt.opts.Policy.Route(ctx, req)
	if err != nil {
		return nil, toReqErr(err)
	}
	if dec.Release != nil {
		defer dec.Release()
	}
	if len(dec.Targets) == 0 {
		return nil, forbidden(errors.New("policy chose no target"))
	}
	ids := make([]string, len(dec.Targets))
	for i, t := range dec.Targets {
		ids[i] = t.ID
		if req.Session != nil && t.ID != req.Session.Target {
			return nil, acp.NewInvalidParams(map[string]any{"error": fmt.Sprintf("session %s lives on target %s, not %s", sid, req.Session.Target, t.ID)})
		}
	}
	rec.Target = strings.Join(ids, ",")
	creates := createsSession[method]
	if creates && len(dec.Targets) > 1 {
		return nil, acp.NewInternalError(map[string]any{"error": method + " needs exactly one target"})
	}
	if req.Session != nil {
		req.Params["sessionId"], _ = json.Marshal(req.Session.Downstream) // a string always marshals
	}

	if notifications[method] {
		var first error
		for _, t := range dec.Targets {
			if err := rt.notify(ctx, t, method, req.Params); err != nil && first == nil {
				first = err
			}
		}
		if first != nil {
			return nil, toReqErr(first)
		}
		return nil, nil
	}

	if creates {
		return rt.create(ctx, dec, req, rec)
	}
	res, err := rt.fanout(ctx, dec.Targets, method, req.Params)
	if err != nil {
		return nil, toReqErr(err)
	}
	if method == "session/delete" {
		if err := rt.opts.Store.Delete(sid); err != nil {
			return nil, toReqErr(err)
		}
	}
	return res, nil
}

func (rt *Router) notify(ctx context.Context, t backend.Target, method string, params any) error {
	p, err := rt.peer(ctx, t)
	if err != nil {
		return err
	}
	return p.Conn().SendNotification(ctx, method, params)
}

func (rt *Router) call(ctx context.Context, t backend.Target, method string, params any) (json.RawMessage, error) {
	p, err := rt.peer(ctx, t)
	if err != nil {
		return nil, err
	}
	return acp.SendRequest[json.RawMessage](p.Conn(), ctx, method, params)
}

// fanout sends the request to every target concurrently and returns the
// first target's answer once all have answered.
func (rt *Router) fanout(ctx context.Context, ts []backend.Target, method string, params any) (json.RawMessage, error) {
	type answer struct {
		res json.RawMessage
		err error
	}
	answers := make([]answer, len(ts))
	var wg sync.WaitGroup
	for i, t := range ts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i].res, answers[i].err = rt.call(ctx, t, method, params)
		}()
	}
	wg.Wait()
	return answers[0].res, answers[0].err
}

func (rt *Router) create(ctx context.Context, dec *Decision, req *Request, rec *audit.Record) (any, *acp.RequestError) {
	t := dec.Targets[0]
	rt.bufMu.Lock()
	rt.creating[t.ID]++
	rt.bufMu.Unlock()
	defer func() {
		rt.bufMu.Lock()
		if rt.creating[t.ID]--; rt.creating[t.ID] == 0 {
			delete(rt.creating, t.ID)
			delete(rt.buf, t.ID)
		}
		rt.bufMu.Unlock()
	}()

	res, err := rt.call(ctx, t, req.Method, req.Params)
	if err != nil {
		return nil, toReqErr(err)
	}
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(res, &obj) // a non-object leaves obj nil: no sessionId
	var down string
	_ = json.Unmarshal(obj["sessionId"], &down)
	if down == "" {
		return nil, acp.NewInternalError(map[string]any{"error": "agent returned no sessionId"})
	}

	rt.bufMu.Lock()
	defer rt.bufMu.Unlock()
	// Probe and Put both run under bufMu, so concurrent creates cannot
	// pick the same id.
	up := down
	for n := 1; ; n++ {
		if _, taken := rt.opts.Store.Get(up); !taken {
			break
		}
		up = down + "@" + t.ID
		if n > 1 {
			up += fmt.Sprintf("#%d", n)
		}
	}
	if err := rt.opts.Store.Put(store.Session{Upstream: up, Target: t.ID, Downstream: down, Labels: dec.Labels}); err != nil {
		return nil, toReqErr(fmt.Errorf("record session %s: %w", up, err))
	}
	rec.Session = up
	upConn := rt.up.Load()
	var rest []held
	for _, h := range rt.buf[t.ID] {
		if h.down != down {
			rest = append(rest, h)
			continue
		}
		h.obj["sessionId"], _ = json.Marshal(up)
		_ = upConn.SendNotification(ctx, h.method, h.obj) // best effort, like any notification
	}
	rt.buf[t.ID] = rest
	obj["sessionId"], _ = json.Marshal(up)
	return obj, nil
}

// peer returns the live connection for t, opening (and initializing) it
// on first use or after it died.
func (rt *Router) peer(ctx context.Context, t backend.Target) (backend.Peer, error) {
	rt.mu.Lock()
	pe := rt.peers[t.ID]
	if pe != nil {
		select {
		case <-pe.ready:
			if pe.err != nil || isDone(pe.peer) {
				pe = nil
			}
		default:
		}
	}
	created := pe == nil
	if created {
		pe = &peerEntry{ready: make(chan struct{})}
		rt.peers[t.ID] = pe
	}
	rt.mu.Unlock()

	if created {
		pe.peer, pe.err = rt.open(ctx, t)
		close(pe.ready)
	} else {
		select {
		case <-pe.ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return pe.peer, pe.err
}

func isDone(p backend.Peer) bool {
	select {
	case <-p.Done():
		return true
	default:
		return false
	}
}

func (rt *Router) open(ctx context.Context, t backend.Target) (backend.Peer, error) {
	be, ok := rt.opts.Backends[t.Kind]
	if !ok {
		return nil, fmt.Errorf("target %s: no backend of kind %q", t.ID, t.Kind)
	}
	p, err := be.Open(ctx, t, rt.downstream(t.ID))
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", t.ID, err)
	}
	init := json.RawMessage(`{"protocolVersion":1}`)
	if ip := rt.initParams.Load(); ip != nil {
		init = *ip
	}
	if _, err := acp.SendRequest[json.RawMessage](p.Conn(), ctx, "initialize", init); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("target %s: initialize: %w", t.ID, err)
	}
	return p, nil
}

// downstream handles calls from target's agent. A call naming a session
// is mapped to its upstream id; a session the target does not serve is
// refused, so one target can never speak into another's sessions.
func (rt *Router) downstream(target string) acp.MethodHandler {
	return func(ctx context.Context, method string, raw json.RawMessage) (any, *acp.RequestError) {
		<-rt.upSet
		up := rt.up.Load()
		var obj map[string]json.RawMessage
		_ = json.Unmarshal(raw, &obj) // non-object params pass through untouched
		var params any = raw
		if s, ok := obj["sessionId"]; ok {
			var down string
			_ = json.Unmarshal(s, &down)
			rt.bufMu.Lock()
			upSid, known := rt.opts.Store.Upstream(target, down)
			if !known {
				if notifications[method] && rt.creating[target] > 0 {
					rt.buf[target] = append(rt.buf[target], held{method: method, down: down, obj: obj})
					rt.bufMu.Unlock()
					return nil, nil
				}
				rt.bufMu.Unlock()
				rt.opts.Audit.Log(audit.Record{
					Time: time.Now().UTC(), Method: "downstream:" + method, Target: target,
					Session: down, Outcome: audit.Denied, Error: "session not served by this target",
				})
				return nil, forbidden(fmt.Errorf("session %s is not served by target %s", down, target))
			}
			rt.bufMu.Unlock()
			obj["sessionId"], _ = json.Marshal(upSid)
			params = obj
		}
		if notifications[method] {
			if err := up.SendNotification(ctx, method, params); err != nil {
				return nil, toReqErr(err)
			}
			return nil, nil
		}
		rec := audit.Record{Time: time.Now().UTC(), Method: "downstream:" + method, Target: target, Outcome: audit.OK}
		if s, ok := obj["sessionId"]; ok {
			_ = json.Unmarshal(s, &rec.Session)
		}
		res, err := acp.SendRequest[json.RawMessage](up, ctx, method, params)
		if err != nil {
			rec.Outcome, rec.Error = audit.Error, err.Error()
		}
		rt.opts.Audit.Log(rec)
		if err != nil {
			return nil, toReqErr(err)
		}
		return res, nil
	}
}
