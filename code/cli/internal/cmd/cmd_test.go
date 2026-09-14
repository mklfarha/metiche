package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mklfarha/metiche/cli/internal/mcpclient"
)

func testApp(stdin string) (*app, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	a := &app{stdin: strings.NewReader(stdin), stdout: &out, stderr: &errb, env: func(string) string { return "" }}
	a.in = bufio.NewReader(a.stdin)
	return a, &out, &errb
}

func TestExitCodeMapping(t *testing.T) {
	a, _, _ := testApp("")
	cases := []struct {
		err  error
		exit int
	}{
		{&mcpclient.Error{Kind: mcpclient.KindUnreachable}, exitNoServer},
		{&mcpclient.Error{Kind: mcpclient.KindTool, Code: "not_permitted"}, exitRefused},
		{&mcpclient.Error{Kind: mcpclient.KindTool, Code: "not_found"}, exitRefused},
		{&mcpclient.Error{Kind: mcpclient.KindTool, Code: "rate_limited"}, exitRefused},
		{&mcpclient.Error{Kind: mcpclient.KindTool, Code: "already_exists"}, exitConflict},
		{&mcpclient.Error{Kind: mcpclient.KindTool, Code: "ambiguous_team"}, exitUsage},
		{&mcpclient.Error{Kind: mcpclient.KindTool, Code: "unavailable"}, exitNoServer},
		{&mcpclient.Error{Kind: mcpclient.KindTool, Code: "unknown_tool"}, exitError},
		{&mcpclient.Error{Kind: mcpclient.KindTool, Code: "invalid_argument"}, exitError},
		{&mcpclient.Error{Kind: mcpclient.KindProtocol}, exitError},
	}
	for _, c := range cases {
		got := a.mapErr(c.err).(*cliError)
		if got.exit != c.exit {
			t.Errorf("%+v -> exit %d, want %d", c.err, got.exit, c.exit)
		}
	}
}

func TestUnknownCommandJSONEnvelope(t *testing.T) {
	var out, errb bytes.Buffer
	code := Main([]string{"nonsense", "--json"}, strings.NewReader(""), &out, &errb)
	var d map[string]any
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		t.Fatalf("not one JSON document: %q", out.String())
	}
	if code != exitUsage || d["schema"] != "metiche.cli.error/1" || d["ok"] != false || d["exit"].(float64) != 2 {
		t.Errorf("exit %d doc %v", code, d)
	}
}

func TestVersionAndUninstall(t *testing.T) {
	var out bytes.Buffer
	if code := Main([]string{"version", "--json"}, strings.NewReader(""), &out, &bytes.Buffer{}); code != 0 || !strings.Contains(out.String(), `"schema":"metiche.cli.version/1"`) {
		t.Errorf("version --json: %d %s", code, out.String())
	}
	out.Reset()
	if code := Main([]string{"uninstall"}, strings.NewReader(""), &out, &bytes.Buffer{}); code != 0 || !strings.Contains(out.String(), "--uninstall --dry-run") {
		t.Errorf("uninstall: %d %s", code, out.String())
	}
}

// TestSlugBaseMatchesTheServer mirrors app/mcp handler.go slugKey(name, 40).
func TestSlugBaseMatchesTheServer(t *testing.T) {
	for in, want := range map[string]string{
		"Hack Night": "hack-night", "hack-night": "hack-night", " HACK  night ": "hack-night", "hack_night!": "hack-night",
		"Hack Nights": "hack-nights", "!!!": "", "Ünïcode Team": "n-code-team",
	} {
		if got := slugBase(in); got != want {
			t.Errorf("slugBase(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPromptsHaveNoDefault: Enter alone asks again; EOF is not a yes.
func TestPromptsHaveNoDefault(t *testing.T) {
	a, out, _ := testApp("\n\n9\n2\n")
	n, err := a.choose("team", 3)
	if err != nil || n != 2 {
		t.Fatalf("choose = %d, %v", n, err)
	}
	if strings.Count(out.String(), "team [1-3]: ") != 4 {
		t.Errorf("prompt shown %d times: %q", strings.Count(out.String(), "team [1-3]: "), out.String())
	}
	a, _, _ = testApp("\n")
	if a.confirm("Write?") {
		t.Error("Enter alone confirmed a write")
	}
	a, _, _ = testApp("")
	if a.confirm("Write?") {
		t.Error("EOF confirmed a write")
	}
	a, _, _ = testApp("yes\n")
	if !a.confirm("Write?") {
		t.Error("yes did not confirm")
	}
	a, _, _ = testApp("\n")
	if v, err := a.ask("project", "demo"); err != nil || v != "demo" {
		t.Errorf("ask default = %q %v", v, err)
	}
}
