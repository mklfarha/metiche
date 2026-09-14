// Package config resolves where metiche is (the endpoint and the board) and
// where this machine keeps its metiche state (docs/CLI.md §1.1, §2).
package config

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultEndpoint = "https://mcp.metiche.xyz/v1/mcp"
	DefaultBoard    = "https://metiche.xyz"
	// ServerName is the MCP entry name the installer writes into every client.
	ServerName = "metiche"
)

// Env is the process environment, injectable for tests.
type Env func(string) string

// Endpoint is the resolved MCP URL and where it came from.
type Endpoint struct {
	URL     string
	Source  string // flag | env | default
	Warning string // set for a plaintext local endpoint
}

// ResolveEndpoint: --url beats METICHE_MCP_URL beats the default. https only,
// except http://localhost and http://127.0.0.1, with a warning (the installer's
// rule).
func ResolveEndpoint(flagURL string, env Env) (Endpoint, error) {
	ep := Endpoint{URL: DefaultEndpoint, Source: "default"}
	if v := strings.TrimSpace(env("METICHE_MCP_URL")); v != "" {
		ep = Endpoint{URL: v, Source: "env"}
	}
	if v := strings.TrimSpace(flagURL); v != "" {
		ep = Endpoint{URL: v, Source: "flag"}
	}
	u, err := url.Parse(ep.URL)
	if err != nil || u.Host == "" {
		return ep, errors.New("the MCP endpoint is not a URL: " + ep.URL)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLocalHost(u.Hostname()):
		ep.Warning = "using a plaintext local endpoint: " + ep.URL
	default:
		return ep, errors.New("the MCP endpoint must be https (or a local http endpoint), got: " + ep.URL)
	}
	return ep, nil
}

func isLocalHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || strings.HasPrefix(h, "localhost.")
}

// BoardBase is METICHE_BOARD_URL, else https://metiche.xyz, without a trailing
// slash.
func BoardBase(env Env) string {
	b := strings.TrimSpace(env("METICHE_BOARD_URL"))
	if b == "" {
		b = DefaultBoard
	}
	return strings.TrimRight(b, "/")
}

// BoardURL is <base>/t/<slug>.
func BoardURL(base, slug string) string { return base + "/t/" + url.PathEscape(slug) }

// Paths are the files the installer writes on this machine.
type Paths struct {
	Home       string
	MeticheDir string
	EnvFile    string
	Claude     string
	Cursor     string
	Windsurf   string
	CodexDir   string
	Codex      string
}

// ResolvePaths follows install.sh's client_config.
func ResolvePaths(env Env) Paths {
	home := env("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	claudeDir := env("CLAUDE_CONFIG_DIR")
	if claudeDir == "" {
		claudeDir = home
	}
	codex := env("CODEX_HOME")
	if codex == "" {
		codex = filepath.Join(home, ".codex")
	}
	md := filepath.Join(home, ".metiche")
	return Paths{
		Home:       home,
		MeticheDir: md,
		EnvFile:    filepath.Join(md, "env"),
		Claude:     filepath.Join(claudeDir, ".claude.json"),
		Cursor:     filepath.Join(home, ".cursor", "mcp.json"),
		Windsurf:   filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"),
		CodexDir:   codex,
		Codex:      filepath.Join(codex, "config.toml"),
	}
}

// Tilde shortens a path under home for display.
func (p Paths) Tilde(path string) string {
	if p.Home != "" && strings.HasPrefix(path, p.Home+string(filepath.Separator)) {
		return "~" + path[len(p.Home):]
	}
	return path
}

// MachineID is install.sh's machine_id: $METICHE_MACHINE_ID, else the
// hostname, cut at the first dot, lowercased, every byte outside [a-z0-9-]
// replaced by '-'.
func MachineID(env Env, hostname func() (string, error)) string {
	h := env("METICHE_MACHINE_ID")
	if h == "" && hostname != nil {
		if v, err := hostname(); err == nil {
			h = v
		}
	}
	if h == "" {
		h = env("HOSTNAME")
	}
	if h == "" {
		h = "unknown-host"
	}
	if i := strings.Index(h, "."); i >= 0 {
		h = h[:i]
	}
	b := []byte(h)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			c = '-'
		}
		b[i] = c
	}
	return string(b)
}
