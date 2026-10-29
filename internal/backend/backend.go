// Package backend reaches a downstream ACP agent described by a Target.
// Backend is an interface so a sandbox / exec-environment service can plug
// in; Exec spawns the target's command and Socket dials the target's unix
// socket (served by such a service).
package backend

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// Target describes one downstream agent. Policies build Targets; the
// router opens one Peer per Target.ID and reuses it.
type Target struct {
	// ID identifies the agent; requests routed to the same ID share it.
	ID string `json:"id"`
	// Kind selects the Backend ("exec", "socket", or a custom one).
	Kind string `json:"kind"`
	// Command is the argv to spawn (exec).
	Command []string `json:"command,omitempty"`
	// Dir is the agent's working directory and HOME (exec).
	Dir string `json:"dir,omitempty"`
	// Env is added to the agent's environment (exec). It may carry
	// credentials and is never persisted. It is applied when the target
	// is opened; changes reach a running agent only after it restarts.
	Env map[string]string `json:"-"`
	// Socket is the unix socket to dial (socket).
	Socket string `json:"socket,omitempty"`
}

// Peer is a live ACP connection to one user's agent.
type Peer interface {
	// Conn is the ACP connection; the router is the client side.
	Conn() *acp.Connection
	// Done is closed when the agent is gone.
	Done() <-chan struct{}
	// Close tears the agent connection down.
	Close() error
}

// Backend opens a Peer for a Target. h handles agent-initiated calls
// (session/update, session/request_permission, fs/*, ...).
type Backend interface {
	Open(ctx context.Context, t Target, h acp.MethodHandler) (Peer, error)
}

// DefaultGrace is Exec's default shutdown grace period.
const DefaultGrace = 5 * time.Second

// Exec spawns Target.Command with cwd and HOME set to Target.Dir and an
// environment built only from InheritEnv and Target.Env.
type Exec struct {
	InheritEnv []string
	// Grace is how long Close waits for the agent to exit after its stdin
	// closes before killing it (0 means DefaultGrace).
	Grace time.Duration
	// Stderr receives the agent's stderr (nil discards).
	Stderr io.Writer
}

// Environ builds the agent environment for t: inherited variables, then
// Target.Env, then HOME (when Dir is set). Later entries win.
func (e *Exec) Environ(t Target) []string {
	m := map[string]string{}
	for _, k := range e.InheritEnv {
		if v, ok := os.LookupEnv(k); ok {
			m[k] = v
		}
	}
	for k, v := range t.Env {
		m[k] = v
	}
	if t.Dir != "" {
		m["HOME"] = t.Dir
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Open implements Backend.
func (e *Exec) Open(_ context.Context, t Target, h acp.MethodHandler) (Peer, error) {
	if len(t.Command) == 0 {
		return nil, errors.New("backend: empty command")
	}
	if t.Dir != "" {
		if err := os.MkdirAll(t.Dir, 0o700); err != nil {
			return nil, err
		}
	}
	// Not CommandContext: the agent outlives the request that started it.
	cmd := exec.Command(t.Command[0], t.Command[1:]...) //nolint:gosec // operator-configured command
	cmd.Dir = t.Dir
	cmd.Env = e.Environ(t)
	cmd.Stderr = e.Stderr
	// Own process group, so shutdown also reaches the agent's children
	// (tool processes, MCP servers) and none keeps the pipes open.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	stdin, _ := cmd.StdinPipe()   // cannot fail before Start
	stdout, _ := cmd.StdoutPipe() // cannot fail before Start
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	grace := e.Grace
	if grace <= 0 {
		grace = DefaultGrace
	}
	p := &procPeer{cmd: cmd, stdin: stdin, grace: grace, done: make(chan struct{})}
	p.conn = acp.NewConnection(h, stdin, stdout)
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

type procPeer struct {
	cmd   *exec.Cmd
	stdin io.Closer
	conn  *acp.Connection
	grace time.Duration
	done  chan struct{}
	once  sync.Once
}

func (p *procPeer) Conn() *acp.Connection { return p.conn }

func (p *procPeer) Done() <-chan struct{} { return p.done }

func (p *procPeer) Close() error {
	p.once.Do(func() {
		// EOF on stdin lets the agent flush and exit on its own.
		_ = p.stdin.Close()
		select {
		case <-p.done:
		case <-time.After(p.grace):
		}
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	})
	<-p.done
	return nil
}

// Socket dials Target.Socket. The service behind the socket owns the
// agent's environment and sandbox; Target.Env is not sent.
type Socket struct{}

// Open implements Backend.
func (Socket) Open(ctx context.Context, t Target, h acp.MethodHandler) (Peer, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", t.Socket)
	if err != nil {
		return nil, err
	}
	return &connPeer{c: c, conn: acp.NewConnection(h, c, c)}, nil
}

type connPeer struct {
	c    net.Conn
	conn *acp.Connection
}

func (p *connPeer) Conn() *acp.Connection { return p.conn }

func (p *connPeer) Done() <-chan struct{} { return p.conn.Done() }

func (p *connPeer) Close() error {
	err := p.c.Close()
	<-p.conn.Done()
	return err
}
