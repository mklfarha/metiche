package credential

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mklfarha/metiche/cli/internal/config"
)

func canary(c string) string { return "mtk_CANARY_" + c + "_0123456789" }

func paths(t *testing.T) config.Paths {
	home := t.TempDir()
	return config.ResolvePaths(func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	})
}

func put(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func TestReadAnchorFile(t *testing.T) {
	p := paths(t)
	put(t, p.EnvFile, "# >>> metiche >>> managed by install.sh\nMETICHE_TOKEN="+canary("anchor")+"\nexport METICHE_TOKEN\n# <<< metiche <<<\n", 0o600)
	af := ReadAnchorFile(p)
	if !af.Exists || !af.Managed || af.Token.Empty() || af.ClientKey || af.Mode != 0o600 {
		t.Errorf("managed block: %+v", af)
	}
	if strings.Contains(fmt.Sprintf("%+v", af), "CANARY") {
		t.Error("printing an AnchorFile shows the token")
	}
	put(t, p.EnvFile, "# metiche — created by install.sh. Keep this file private.\nMETICHE_TOKEN="+canary("legacy")+"\nMETICHE_CLIENT_KEY=laptop\n", 0o644)
	af = ReadAnchorFile(p)
	if !af.ClientKey || af.Mode != 0o644 || af.Token.Empty() {
		t.Errorf("legacy: %+v", af)
	}
	put(t, p.EnvFile, "METICHE_TOKEN=${SOMETHING}\n", 0o600)
	if af := ReadAnchorFile(p); !af.Token.Empty() {
		t.Error("a placeholder read as a token")
	}
}

func TestEnvTokenAndBackups(t *testing.T) {
	p := paths(t)
	env := func(v string) config.Env {
		return func(k string) string { return map[string]string{"METICHE_TOKEN": v}[k] }
	}
	if tok, set := EnvToken(env("")); set || !tok.Empty() {
		t.Error("unset")
	}
	if tok, set := EnvToken(env("join-code")); !set || !tok.Empty() {
		t.Error("a non-token value must be set-but-empty")
	}
	old, _ := EnvToken(env(canary("old")))
	put(t, p.EnvFile+".metiche-backup-20260913-120000", "METICHE_TOKEN="+canary("old")+"\n", 0o600)
	if !InBackups(p, old) {
		t.Error("the backup token was not found")
	}
	cur, _ := EnvToken(env(canary("current")))
	if InBackups(p, cur) {
		t.Error("a token in no backup was found")
	}
	put(t, p.MeticheDir+".metiche-backup-20260101-000000/env", "METICHE_TOKEN="+canary("current")+"\n", 0o600)
	if !InBackups(p, cur) {
		t.Error("an uninstall backup was not searched")
	}
}

func TestReadEntryPerClient(t *testing.T) {
	p := paths(t)
	url := "https://mcp.metiche.xyz/v1/mcp"
	put(t, p.Claude, `{"mcpServers":{"metiche":{"type":"http","url":"`+url+`","headers":{"Authorization":"Bearer `+canary("claude")+`"}},"metiche-direct":{"url":"`+url+`","headers":{"Authorization":"x","X-Metiche-Client-Key":"k"}}},"projects":{}}`, 0o600)
	put(t, p.Cursor, `{"mcpServers":{"metiche":{"url":"`+url+`","headers":{"Authorization":"Bearer ${env:METICHE_TOKEN}"}}}}`, 0o600)
	put(t, p.Windsurf, `{"mcpServers":{"metiche":{"serverUrl":"`+url+`","headers":{"Authorization":"Bearer `+canary("windsurf")+`"}}}}`, 0o644)
	put(t, p.Codex, "model = \"x\"\n\n[mcp_servers.metiche]\nurl = \""+url+"\"\nhttp_headers = { Authorization = \"Bearer "+canary("codex")+"\" }\n", 0o600)

	claude := ReadEntry(p, "claude")
	if !claude.Present || claude.Token.Empty() || claude.URL != url || len(claude.OtherNames) != 1 || claude.OtherNames[0] != "metiche-direct" {
		t.Errorf("claude: %+v", claude)
	}
	cursor := ReadEntry(p, "cursor")
	if !cursor.EnvBased || !cursor.Token.Empty() {
		t.Errorf("cursor env placeholder: %+v", cursor)
	}
	ws := ReadEntry(p, "windsurf")
	if ws.URL != url || ws.Token.Empty() || ws.Mode != 0o644 {
		t.Errorf("windsurf serverUrl: %+v", ws)
	}
	codex := ReadEntry(p, "codex")
	if !codex.Present || codex.Token.Empty() || codex.URL != url {
		t.Errorf("codex: %+v", codex)
	}
	if claude.Token.Equal(codex.Token) {
		t.Error("distinct tokens compare equal")
	}
	put(t, p.Codex, "[mcp_servers.metiche]\nurl = \""+url+"\"\nbearer_token_env_var = \"METICHE_TOKEN\"\n", 0o600)
	if e := ReadEntry(p, "codex"); !e.EnvBased {
		t.Errorf("bearer_token_env_var not detected: %+v", e)
	}
	put(t, p.Codex, "[mcp_servers.metiche\nurl=", 0o600)
	if e := ReadEntry(p, "codex"); e.ParseError == "" {
		t.Error("malformed TOML parsed")
	}
	for _, c := range Clients {
		if s := fmt.Sprintf("%+v %v", ReadEntry(p, c), ReadEntry(p, c)); strings.Contains(s, "CANARY") {
			t.Errorf("printing %s's entry leaks a token", c)
		}
	}
}
