// Package testagent is a scripted ACP agent used by the router and backend
// tests. It is test support, not product code.
package testagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	acp "github.com/coder/acp-go-sdk"
)

// Agent is a scripted ACP agent. An empty Prefix makes session/new and
// session/fork answer without a sessionId; _meta.announce=true on
// session/new emits a session/update for the new session before the
// response; "_ping" answers {"prefix": Prefix}. Prompt text drives it:
//
//	"block"       say "blocked: waiting for cancel", wait for session/cancel (or request cancel), stop "cancelled"
//	"spoof:<sid>" emit a session/update for <sid> instead of the prompt's session
//	"fail"        return a JSON-RPC error
//	anything else echo it back as an agent_message_chunk, ask permission, end_turn
type Agent struct {
	Prefix string
	Conn   *acp.Connection
	getenv func(string) string

	n         atomic.Int64
	mu        sync.Mutex
	cancelled map[string]chan struct{}
	// Calls records every method received, in order.
	Calls []string
}

// Serve runs an Agent over r/w and returns it. getenv reads the agent's
// environment; nil means os.Getenv.
func Serve(prefix string, getenv func(string) string, r io.Reader, w io.Writer) *Agent {
	if getenv == nil {
		getenv = os.Getenv
	}
	a := &Agent{Prefix: prefix, getenv: getenv, cancelled: map[string]chan struct{}{}}
	a.Conn = acp.NewConnection(a.handle, w, r)
	return a
}

// Methods returns a copy of Calls.
func (a *Agent) Methods() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.Calls...)
}

func (a *Agent) cancelCh(sid string) chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	ch, ok := a.cancelled[sid]
	if !ok {
		ch = make(chan struct{})
		a.cancelled[sid] = ch
	}
	return ch
}

type params struct {
	SessionID string             `json:"sessionId"`
	Cwd       string             `json:"cwd"`
	Meta      map[string]any     `json:"_meta"`
	Prompt    []acp.ContentBlock `json:"prompt"`
}

func (a *Agent) handle(ctx context.Context, method string, raw json.RawMessage) (any, *acp.RequestError) {
	a.mu.Lock()
	a.Calls = append(a.Calls, method)
	a.mu.Unlock()
	var p params
	_ = json.Unmarshal(raw, &p)
	echo := map[string]any{"cwd": p.Cwd, "home": a.getenv("HOME"), "secret": a.getenv("SECRET"), "meta": p.Meta}
	switch method {
	case "initialize":
		return map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true}, "_meta": map[string]any{"params": json.RawMessage(raw)}}, nil
	case "session/new", "session/fork":
		if a.Prefix == "" {
			return map[string]any{"_meta": echo}, nil
		}
		sid := fmt.Sprintf("%s-%d", a.Prefix, a.n.Add(1))
		if p.Meta["announce"] == true {
			_ = a.Conn.SendNotification(ctx, "session/update", acp.SessionNotification{
				SessionId: acp.SessionId(sid), Update: acp.UpdateAgentMessageText("hello"),
			})
		}
		return map[string]any{"sessionId": sid, "_meta": echo}, nil
	case "session/load", "session/resume", "session/set_mode", "session/close":
		return map[string]any{"_meta": echo}, nil
	case "session/cancel":
		ch := a.cancelCh(p.SessionID)
		a.mu.Lock()
		select {
		case <-ch:
		default:
			close(ch)
		}
		a.mu.Unlock()
		return nil, nil
	case "session/prompt":
		return a.prompt(ctx, p)
	case "_ping":
		return map[string]any{"prefix": a.Prefix}, nil
	}
	return nil, acp.NewMethodNotFound(method)
}

func (a *Agent) prompt(ctx context.Context, p params) (any, *acp.RequestError) {
	text := ""
	for _, b := range p.Prompt {
		if b.Text != nil {
			text += b.Text.Text
		}
	}
	switch {
	case text == "block":
		_ = a.Conn.SendNotification(ctx, "session/update", acp.SessionNotification{
			SessionId: acp.SessionId(p.SessionID), Update: acp.UpdateAgentMessageText("blocked: waiting for cancel"),
		})
		select {
		case <-a.cancelCh(p.SessionID):
		case <-ctx.Done():
		}
		return map[string]any{"stopReason": "cancelled"}, nil
	case text == "fail":
		return nil, acp.NewInternalError(map[string]any{"error": "scripted failure"})
	}
	sid := p.SessionID
	if rest, ok := strings.CutPrefix(text, "spoof:"); ok {
		sid = rest
	}
	_ = a.Conn.SendNotification(ctx, "session/update", acp.SessionNotification{
		SessionId: acp.SessionId(sid),
		Update:    acp.UpdateAgentMessageText("echo: " + text),
	})
	perm, err := acp.SendRequest[acp.RequestPermissionResponse](a.Conn, ctx, "session/request_permission", acp.RequestPermissionRequest{
		SessionId: acp.SessionId(sid),
		ToolCall:  acp.ToolCallUpdate{ToolCallId: "t1"},
		Options:   []acp.PermissionOption{{OptionId: "ok", Name: "OK", Kind: acp.PermissionOptionKindAllowOnce}},
	})
	outcome := "error"
	if err == nil && perm.Outcome.Selected != nil {
		outcome = string(perm.Outcome.Selected.OptionId)
	} else if err == nil {
		outcome = "cancelled"
	}
	return map[string]any{"stopReason": "end_turn", "_meta": map[string]any{"permission": outcome, "meta": p.Meta}}, nil
}
