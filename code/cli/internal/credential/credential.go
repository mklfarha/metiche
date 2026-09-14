// Package credential finds the tokens on this machine without ever printing
// one (docs/CLI.md §3). It reads files; it never writes, sources or evals them.
//
// Resolution order, applied by the caller (it needs whoami):
//  1. METICHE_TOKEN from the environment;
//  2. METICHE_TOKEN= in ~/.metiche/env;
//  3. only when 1-2 are absent or rejected: the tokens in the client configs,
//     used only if every accepted one resolves to the same account.
//
// One refinement, from docs/LEARNINGS.md §4 and the installer (95cad88): a
// $METICHE_TOKEN that is byte for byte a token in a BACKUP of ~/.metiche/env is
// a stale shell from before the last install, not a token passed on purpose.
// The saved token is used instead, and the caller says so.
package credential

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/mklfarha/metiche/cli/internal/config"
	"github.com/mklfarha/metiche/cli/internal/secret"
)

const (
	envStart     = "# >>> metiche >>>"
	legacyHeader = "# metiche — created by install.sh"
	tokenVar     = "METICHE_TOKEN"
)

var tokenShape = regexp.MustCompile(`^[A-Za-z0-9._-]{16,}$`)

// ShapeOK is the installer's token_shape_ok.
func ShapeOK(v string) bool { return tokenShape.MatchString(v) }

// Candidate is one token and where it was found.
type Candidate struct {
	Source string // env | anchor_file | client
	Client string // claude | cursor | windsurf | codex, for Source client
	Path   string
	Token  secret.Secret
}

// AnchorFile is what ~/.metiche/env holds, values never exposed.
type AnchorFile struct {
	Path      string
	Exists    bool
	Managed   bool // starts with the managed block or the legacy header
	Token     secret.Secret
	ClientKey bool // still exports METICHE_CLIENT_KEY (pre per-agent tokens)
	Mode      os.FileMode
	DirMode   os.FileMode
}

// ReadAnchorFile parses ~/.metiche/env as text.
func ReadAnchorFile(p config.Paths) AnchorFile {
	af := AnchorFile{Path: p.EnvFile}
	if st, err := os.Stat(p.MeticheDir); err == nil {
		af.DirMode = st.Mode().Perm()
	}
	st, err := os.Lstat(p.EnvFile)
	if err != nil || !st.Mode().IsRegular() {
		return af
	}
	af.Exists = true
	af.Mode = st.Mode().Perm()
	b, err := os.ReadFile(p.EnvFile)
	if err != nil {
		return af
	}
	af.Token = envFileToken(b)
	first, _, _ := bytes.Cut(b, []byte("\n"))
	af.Managed = strings.HasPrefix(string(first), envStart) || strings.HasPrefix(string(first), legacyHeader)
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "METICHE_CLIENT_KEY=") ||
			strings.HasPrefix(strings.TrimSpace(line), "export METICHE_CLIENT_KEY=") {
			af.ClientKey = true
		}
	}
	return af
}

// envFileToken is install.sh's env_file_token: the first METICHE_TOKEN= line,
// if its value is token-shaped.
func envFileToken(b []byte) secret.Secret {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, tokenVar+"=") {
			v := strings.TrimPrefix(line, tokenVar+"=")
			if ShapeOK(v) {
				return secret.New(v)
			}
			return secret.Secret{}
		}
	}
	return secret.Secret{}
}

// EnvToken is $METICHE_TOKEN when set and token-shaped.
func EnvToken(env config.Env) (secret.Secret, bool) {
	v := strings.TrimSpace(env(tokenVar))
	if v == "" {
		return secret.Secret{}, false
	}
	if !ShapeOK(v) {
		return secret.Secret{}, true // set, but not a token
	}
	return secret.New(v), true
}

// InBackups reports whether tok is the token in a backup of ~/.metiche/env:
// <env>.metiche-backup-* or ~/.metiche.metiche-backup-*/env.
func InBackups(p config.Paths, tok secret.Secret) bool {
	if tok.Empty() {
		return false
	}
	var files []string
	a, _ := filepath.Glob(p.EnvFile + ".metiche-backup-*")
	b, _ := filepath.Glob(p.MeticheDir + ".metiche-backup-*/env")
	files = append(append(files, a...), b...)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if t := envFileToken(raw); !t.Empty() && t.Equal(tok) {
			return true
		}
	}
	return false
}

// Clients are the four clients, in the installer's order.
var Clients = []string{"claude", "cursor", "windsurf", "codex"}

// Titles for output.
var Titles = map[string]string{"claude": "Claude Code", "cursor": "Cursor", "windsurf": "Windsurf", "codex": "Codex"}

// ClientPath is where each client keeps its metiche entry.
func ClientPath(p config.Paths, client string) string {
	switch client {
	case "claude":
		return p.Claude
	case "cursor":
		return p.Cursor
	case "windsurf":
		return p.Windsurf
	case "codex":
		return p.Codex
	}
	return ""
}

// Entry is one client's metiche registration as read from its config.
type Entry struct {
	Client      string
	Path        string
	FileExists  bool
	ParseError  string
	Present     bool
	URL         string
	HeaderNames []string
	Token       secret.Secret // a literal Bearer value of token shape
	EnvBased    bool          // the Authorization value expands an environment variable
	OtherNames  []string      // other entries whose name or URL mentions metiche
	Mode        os.FileMode
}

type jsonServer struct {
	Type      string            `json:"type"`
	URL       string            `json:"url"`
	ServerURL string            `json:"serverUrl"`
	Headers   map[string]string `json:"headers"`
}

// ReadEntry reads one client's config.
func ReadEntry(p config.Paths, client string) Entry {
	e := Entry{Client: client, Path: ClientPath(p, client)}
	st, err := os.Stat(e.Path)
	if err != nil {
		return e
	}
	e.FileExists = true
	e.Mode = st.Mode().Perm()
	raw, err := os.ReadFile(e.Path)
	if err != nil {
		e.ParseError = err.Error()
		return e
	}
	var servers map[string]json.RawMessage
	if client == "codex" {
		var doc struct {
			MCPServers map[string]map[string]any `toml:"mcp_servers"`
		}
		if err := toml.Unmarshal(raw, &doc); err != nil {
			e.ParseError = firstLine(err.Error())
			return e
		}
		servers = map[string]json.RawMessage{}
		for name, tbl := range doc.MCPServers {
			s := jsonServer{Headers: map[string]string{}}
			if v, ok := tbl["url"].(string); ok {
				s.URL = v
			}
			if h, ok := tbl["http_headers"].(map[string]any); ok {
				for k, v := range h {
					if vs, ok := v.(string); ok {
						s.Headers[k] = vs
					}
				}
			}
			if v, ok := tbl["bearer_token_env_var"].(string); ok && v != "" {
				s.Headers["Authorization"] = "Bearer ${" + v + "}"
			}
			b, _ := json.Marshal(s)
			servers[name] = b
		}
	} else {
		var doc struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			e.ParseError = firstLine(err.Error())
			return e
		}
		servers = doc.MCPServers
	}
	for name, rawSrv := range servers {
		var s jsonServer
		_ = json.Unmarshal(rawSrv, &s)
		u := s.URL
		if u == "" {
			u = s.ServerURL
		}
		if name != config.ServerName {
			if strings.Contains(strings.ToLower(name), "metiche") || strings.Contains(strings.ToLower(u), "metiche") {
				e.OtherNames = append(e.OtherNames, name)
			}
			continue
		}
		e.Present = true
		e.URL = u
		for k, v := range s.Headers {
			e.HeaderNames = append(e.HeaderNames, k)
			if strings.EqualFold(k, "Authorization") {
				if strings.Contains(v, "${") {
					e.EnvBased = true
				}
				tok := strings.TrimSpace(v)
				if len(tok) > 7 && strings.EqualFold(tok[:7], "bearer ") {
					tok = strings.TrimSpace(tok[7:])
				}
				if ShapeOK(tok) {
					e.Token = secret.New(tok)
				}
			} else {
				secret.Default.Add(v)
			}
		}
	}
	return e
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
