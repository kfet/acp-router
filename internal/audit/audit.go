// Package audit writes one JSON line per routed upstream request.
package audit

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Outcomes.
const (
	OK     = "ok"
	Denied = "denied"
	Error  = "error"
)

// Record is one audit line.
type Record struct {
	Time     time.Time `json:"time"`
	Method   string    `json:"method"`
	User     string    `json:"user,omitempty"`
	Session  string    `json:"session,omitempty"`
	Target   string    `json:"target,omitempty"`
	Channel  string    `json:"channel,omitempty"`
	Thread   string    `json:"thread,omitempty"`
	Message  string    `json:"message,omitempty"`
	Outcome  string    `json:"outcome"`
	Error    string    `json:"error,omitempty"`
	Duration float64   `json:"duration_ms"`
}

// Logger serialises Records to w. A nil *Logger discards.
type Logger struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// New returns a Logger writing to w.
func New(w io.Writer) *Logger { return &Logger{enc: json.NewEncoder(w)} }

// Log writes r. Write errors are ignored: auditing must not fail a request.
func (l *Logger) Log(r Record) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.enc.Encode(r)
}
