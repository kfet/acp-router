// Package store persists the router's session table: which target serves
// each upstream session, under which downstream id, with which policy
// labels. It survives router restarts.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ErrExists reports an upstream session id that is already recorded.
var ErrExists = errors.New("store: session already recorded")

// Session is one routed session.
type Session struct {
	// Upstream is the id the relay sees.
	Upstream string `json:"upstream"`
	// Target is the id of the target serving it.
	Target string `json:"target"`
	// Downstream is the id the target's agent uses.
	Downstream string `json:"downstream"`
	// Labels are opaque policy annotations (e.g. owner).
	Labels map[string]string `json:"labels,omitempty"`
}

// Store is a file-backed session table. Safe for concurrent use.
type Store struct {
	path string

	mu       sync.Mutex
	sessions map[string]Session   // by upstream id
	down     map[[2]string]string // {target, downstream} -> upstream
}

// Open loads the table at path, starting empty when the file does not exist.
func Open(path string) (*Store, error) {
	s := &Store{path: path, sessions: map[string]Session{}, down: map[[2]string]string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Session
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("store %s: %w", path, err)
	}
	for _, ss := range list {
		s.sessions[ss.Upstream] = ss
		s.down[[2]string{ss.Target, ss.Downstream}] = ss.Upstream
	}
	return s, nil
}

// Get returns the session with upstream id up.
func (s *Store) Get(up string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[up]
	return ss, ok
}

// Upstream maps a target's downstream id to the upstream id.
func (s *Store) Upstream(target, down string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	up, ok := s.down[[2]string{target, down}]
	return up, ok
}

// Put records a new session and persists the table.
func (s *Store) Put(ss Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[ss.Upstream]; ok {
		return ErrExists
	}
	s.sessions[ss.Upstream] = ss
	key := [2]string{ss.Target, ss.Downstream}
	s.down[key] = ss.Upstream
	if err := s.save(); err != nil {
		delete(s.sessions, ss.Upstream)
		delete(s.down, key)
		return err
	}
	return nil
}

// Delete forgets the session with upstream id up.
func (s *Store) Delete(up string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[up]
	if !ok {
		return nil
	}
	delete(s.sessions, up)
	delete(s.down, [2]string{ss.Target, ss.Downstream})
	return s.save()
}

// save writes the table atomically. Caller holds mu.
func (s *Store) save() error {
	list := make([]Session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		list = append(list, ss)
	}
	b, _ := json.Marshal(list) // strings and string maps always marshal
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
