package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPutGetPersist(t *testing.T) {
	p := filepath.Join(t.TempDir(), "d", "sessions.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	a := Session{Upstream: "u1", Target: "t", Downstream: "d1", Labels: map[string]string{"owner": "a"}}
	if err := s.Put(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(a); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	s.Put(Session{Upstream: "u2", Target: "t", Downstream: "d2"})
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s2.Get("u1"); !ok || got.Labels["owner"] != "a" || got.Downstream != "d1" {
		t.Fatal(got, ok)
	}
	if up, ok := s2.Upstream("t", "d2"); !ok || up != "u2" {
		t.Fatal(up, ok)
	}
	if err := s2.Delete("u1"); err != nil {
		t.Fatal(err)
	}
	if err := s2.Delete("nope"); err != nil {
		t.Fatal(err)
	}
	s3, _ := Open(p)
	if _, ok := s3.Get("u1"); ok {
		t.Fatal("deleted session reloaded")
	}
	if _, ok := s3.Upstream("t", "d1"); ok {
		t.Fatal("deleted mapping reloaded")
	}
}

func TestOpenErrors(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "s.json")
	os.WriteFile(p, []byte("{"), 0o600)
	if _, err := Open(p); err == nil {
		t.Fatal("bad json")
	}
	if _, err := Open(d); err == nil {
		t.Fatal("dir read")
	}
}

func TestSaveErrors(t *testing.T) {
	d := t.TempDir()
	f := filepath.Join(d, "file")
	os.WriteFile(f, nil, 0o600)
	// Parent is a file: MkdirAll fails, and the failed Put is rolled back.
	s, _ := Open(filepath.Join(d, "ok.json"))
	s.path = filepath.Join(f, "sub", "s.json")
	if err := s.Put(Session{Upstream: "u", Target: "t", Downstream: "d"}); err == nil {
		t.Fatal("mkdir")
	}
	if _, ok := s.Get("u"); ok {
		t.Fatal("failed put kept")
	}
	if _, ok := s.Upstream("t", "d"); ok {
		t.Fatal("failed put kept mapping")
	}
	// tmp path is a directory: WriteFile fails.
	p := filepath.Join(d, "s.json")
	os.Mkdir(p+".tmp", 0o700)
	s, _ = Open(p)
	if err := s.Put(Session{Upstream: "u"}); err == nil {
		t.Fatal("write")
	}
	// Target is a non-empty directory: Rename fails.
	q := filepath.Join(d, "q.json")
	os.MkdirAll(filepath.Join(q, "x"), 0o700)
	s, _ = Open(filepath.Join(d, "fresh.json"))
	s.path = q
	if err := s.Put(Session{Upstream: "u"}); err == nil {
		t.Fatal("rename")
	}
}
