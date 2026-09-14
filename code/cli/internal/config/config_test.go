package config

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func envOf(m map[string]string) Env { return func(k string) string { return m[k] } }

func TestResolveEndpoint(t *testing.T) {
	cases := []struct {
		flag, env, want, source string
		warn, bad               bool
	}{
		{"", "", DefaultEndpoint, "default", false, false},
		{"", "https://example.test/v1/mcp", "https://example.test/v1/mcp", "env", false, false},
		{"https://flag.test/v1/mcp", "https://example.test/v1/mcp", "https://flag.test/v1/mcp", "flag", false, false},
		{"http://127.0.0.1:8080/v1/mcp", "", "http://127.0.0.1:8080/v1/mcp", "flag", true, false},
		{"http://localhost:8080/v1/mcp", "", "http://localhost:8080/v1/mcp", "flag", true, false},
		{"http://example.test/v1/mcp", "", "", "", false, true},
		{"ftp://example.test", "", "", "", false, true},
		{"not a url", "", "", "", false, true},
	}
	for _, c := range cases {
		ep, err := ResolveEndpoint(c.flag, envOf(map[string]string{"METICHE_MCP_URL": c.env}))
		if c.bad {
			if err == nil {
				t.Errorf("%q accepted", c.flag)
			}
			continue
		}
		if err != nil || ep.URL != c.want || ep.Source != c.source || (ep.Warning != "") != c.warn {
			t.Errorf("flag=%q env=%q -> %+v %v", c.flag, c.env, ep, err)
		}
	}
}

func TestBoardBase(t *testing.T) {
	if got := BoardBase(envOf(nil)); got != DefaultBoard {
		t.Errorf("default board = %q", got)
	}
	if got := BoardURL(BoardBase(envOf(map[string]string{"METICHE_BOARD_URL": "http://localhost:8787/"})), "hack-night"); got != "http://localhost:8787/t/hack-night" {
		t.Errorf("board url = %q", got)
	}
}

// TestMachineIDMatchesTheInstaller runs install.sh's own machine_id() in sh
// and compares (docs/CLI.md §2.1, §8.1).
func TestMachineIDMatchesTheInstaller(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "install.sh"))
	if err != nil {
		t.Skipf("install.sh not readable: %v", err)
	}
	var fn strings.Builder
	in := false
	for _, line := range strings.Split(string(script), "\n") {
		if strings.HasPrefix(line, "machine_id() {") {
			in = true
		}
		if in {
			fn.WriteString(line + "\n")
			if line == "}" {
				break
			}
		}
	}
	if fn.Len() == 0 {
		t.Fatal("machine_id() not found in install.sh")
	}
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib.sh")
	if err := os.WriteFile(lib, []byte(fn.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ override, host string }{
		{"", "Laptop.local"}, {"", "MY_HOST.example.com"}, {"", "anas-mbp"}, {"Dev Box 2", "ignored"},
		{"", "UPPER-lower-09.x"}, {"weird!!name", ""},
	} {
		shim := filepath.Join(dir, "bin")
		_ = os.MkdirAll(shim, 0o700)
		_ = os.WriteFile(filepath.Join(shim, "hostname"), []byte("#!/bin/sh\nprintf '%s\\n' '"+c.host+"'\n"), 0o700)
		cmd := exec.Command("/bin/sh", "-c", ". \"$1\"; machine_id", "sh", lib)
		cmd.Env = []string{"PATH=" + shim + ":/usr/bin:/bin", "METICHE_MACHINE_ID=" + c.override, "HOSTNAME="}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("running machine_id: %v", err)
		}
		host := c.host
		got := MachineID(envOf(map[string]string{"METICHE_MACHINE_ID": c.override}), func() (string, error) {
			if host == "" {
				return "", errors.New("none")
			}
			return host, nil
		})
		if got != string(out) {
			t.Errorf("override=%q host=%q: install.sh says %q, the CLI says %q", c.override, c.host, out, got)
		}
	}
}
