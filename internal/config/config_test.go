package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseExec(t *testing.T) {
	c, err := Parse([]byte(`{"state_dir":"~/st","audit_log":"/a.log","backend":{"command":["fir","--mode","acp"]},
	  "agent_capabilities":{"loadSession":true},
	  "users":{"slack:T:U":{"env_file":"~/u.env"},"slack:T:V":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := os.UserHomeDir()
	if c.StateDir != filepath.Join(h, "st") || c.Backend.Kind != BackendExec {
		t.Fatalf("%+v", c)
	}
	if c.Users["slack:T:U"].EnvFile != filepath.Join(h, "u.env") {
		t.Fatal(c.Users)
	}
	if c.Policy.Route != RoutePerUser || !On(c.Policy.Ownership) {
		t.Fatal(c.Policy)
	}
	c, _ = Parse([]byte(`{"state_dir":"/s","backend":{"command":["a"]},"policy":{"route":"static","ownership":false}}`))
	if c.Policy.Route != RouteStatic || On(c.Policy.Ownership) {
		t.Fatal(c.Policy)
	}
}

func TestParseSocket(t *testing.T) {
	c, err := Parse([]byte(`{"state_dir":"/s","backend":{"kind":"socket","socket":"/run/{user}.sock"}}`))
	if err != nil || c.Backend.Socket != "/run/{user}.sock" {
		t.Fatal(c, err)
	}
}

func TestParseErrors(t *testing.T) {
	for _, s := range []string{
		`{`,
		`{"x":1}`,
		`{}`,
		`{"state_dir":"/s"}`,
		`{"state_dir":"/s","backend":{"kind":"socket"}}`,
		`{"state_dir":"/s","backend":{"kind":"nope"}}`,
		`{"state_dir":"/s","backend":{"command":["a"]},"agent_capabilities":"x"}`,
		`{"state_dir":"/s","backend":{"command":["a"]},"policy":{"route":"x"}}`,
	} {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("%s accepted", s)
		}
	}
	// Invalid raw JSON can only arrive via direct struct use.
	c := &Config{StateDir: "/s", Backend: Backend{Command: []string{"a"}}, AgentCapabilities: []byte("{")}
	if err := c.normalize(); err == nil {
		t.Error("invalid caps accepted")
	}
}

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"state_dir":"/s","backend":{"command":["a"]}}`), 0o600)
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p + "x"); err == nil {
		t.Fatal("missing file loaded")
	}
}

func TestExpandHome(t *testing.T) {
	if ExpandHome("/a") != "/a" {
		t.Fatal()
	}
	t.Setenv("HOME", "")
	if ExpandHome("~/a") != "~/a" {
		t.Fatal("no home should leave path alone")
	}
}

func TestReadEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e")
	os.WriteFile(p, []byte("# c\n\nexport A=1\nB = \"two\"\n"), 0o600)
	m, err := ReadEnvFile(p)
	if err != nil || m["A"] != "1" || m["B"] != "two" {
		t.Fatal(m, err)
	}
	os.WriteFile(p, []byte("bad\n"), 0o600)
	if _, err := ReadEnvFile(p); err == nil || !strings.Contains(err.Error(), ":1:") {
		t.Fatal(err)
	}
	if _, err := ReadEnvFile(p + "x"); err == nil {
		t.Fatal()
	}
}

// The shipped example and the distro default must stay loadable.
func TestShippedConfigsParse(t *testing.T) {
	if _, err := Load(filepath.Join("..", "..", "docs", "config.example.json")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "distro.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Config json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(d.Config); err != nil {
		t.Fatal(err)
	}
}
