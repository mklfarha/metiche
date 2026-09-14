// Package gitx learns the repository from git (docs/CLI.md §1.10.1) and
// normalizes its remote exactly as the server does (app/mcp/repourl.go), so
// `metiche init` resolves the project start_session will resolve. A shared
// vector file (testdata/repourl_vectors.json) guards drift on both sides.
//
// A remote can carry a credential (https://user:token@host/…). The raw value
// seeds the redactor and is never printed or written; only the normalized,
// credential-free identity leaves this package.
package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mklfarha/metiche/cli/internal/execx"
	"github.com/mklfarha/metiche/cli/internal/secret"
)

// Repo is what init and doctor know about the directory.
type Repo struct {
	// Root is the git root, or "" outside a repository.
	Root string
	// Remote is the remote's name (origin, or the only remote).
	Remote string
	// RepoURL is the normalized identity (github.com/owner/repo), or "".
	RepoURL string
	// NoGitBinary is true when git is not on PATH (root found by walking up).
	NoGitBinary bool
	// Note explains a missing URL (several remotes and no origin).
	Note string
}

// Detect inspects dir.
func Detect(ctx context.Context, dir string) Repo {
	var r Repo
	res, err := execx.Run(ctx, dir, "git", "rev-parse", "--show-toplevel")
	var ee *exec.Error
	switch {
	case errors.As(err, &ee):
		r.NoGitBinary = true
		r.Root = walkUpForGit(dir)
		return r
	case err != nil || res.ExitCode != 0:
		return r
	}
	r.Root = strings.TrimSpace(res.Stdout)
	if real, err := filepath.EvalSymlinks(r.Root); err == nil {
		r.Root = real
	}

	raw := ""
	if res, err := execx.Run(ctx, r.Root, "git", "config", "--get", "remote.origin.url"); err == nil && res.ExitCode == 0 {
		raw, r.Remote = strings.TrimSpace(res.Stdout), "origin"
	} else if res, err := execx.Run(ctx, r.Root, "git", "remote"); err == nil && res.ExitCode == 0 {
		names := strings.Fields(res.Stdout)
		switch {
		case len(names) == 1:
			if res, err := execx.Run(ctx, r.Root, "git", "config", "--get", "remote."+names[0]+".url"); err == nil && res.ExitCode == 0 {
				raw, r.Remote = strings.TrimSpace(res.Stdout), names[0]
			}
		case len(names) > 1:
			r.Note = "several remotes and none is origin, so there is no repository URL"
		}
	}
	if raw != "" {
		seedRedactor(raw)
		r.RepoURL = NormalizeRepoURL(raw)
	}
	return r
}

// seedRedactor registers the raw remote and its userinfo, if any.
func seedRedactor(raw string) {
	secret.Default.Add(raw)
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if at := strings.LastIndex(s, "@"); at > 0 {
		userinfo := s[:at]
		if slash := strings.Index(userinfo, "/"); slash >= 0 && !strings.Contains(userinfo[:slash], ":") {
			return
		}
		secret.Default.Add(userinfo)
		if _, pass, ok := strings.Cut(userinfo, ":"); ok {
			secret.Default.Add(pass)
		}
	}
}

func walkUpForGit(dir string) string {
	d, _ := filepath.Abs(dir)
	for {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// Tracked: git ls-files --error-unmatch .metiche in dir.
func Tracked(ctx context.Context, dir string) bool {
	res, err := execx.Run(ctx, dir, "git", "ls-files", "--error-unmatch", ".metiche")
	return err == nil && res.ExitCode == 0
}

// Ignored: git check-ignore -q .metiche in dir.
func Ignored(ctx context.Context, dir string) bool {
	res, err := execx.Run(ctx, dir, "git", "check-ignore", "-q", ".metiche")
	return err == nil && res.ExitCode == 0
}

// ── normalization: a copy of app/mcp/repourl.go ─────────────────────────────

var caseInsensitiveRepoHosts = map[string]bool{"github.com": true, "gitlab.com": true, "bitbucket.org": true}

// NormalizeRepoURL is the server's normalizeRepoURL.
func NormalizeRepoURL(raw string) string {
	host, path := parseRepoURL(raw)
	switch {
	case host == "":
		return path
	case path == "":
		return host
	default:
		return host + "/" + path
	}
}

// CanonicalRepoURL is the server's canonicalRepoURL: the stored form.
func CanonicalRepoURL(raw string) string {
	host, path := parseRepoURL(raw)
	if host == "" {
		return ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if path == "" {
		return "https://" + host
	}
	return "https://" + host + "/" + path
}

// ProjectKeyFromRepoURL is the key start_session derives from a repo_url.
func ProjectKeyFromRepoURL(repoURL string) string {
	host, path := parseRepoURL(repoURL)
	if path == "" {
		return host
	}
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// KeyFromDirName is the derived key with no remote: the git root's directory
// name, lowercased, every run of characters outside [a-z0-9._-] replaced by
// '-', trimmed to start with a letter or digit, at most 64 characters.
func KeyFromDirName(name string) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(name) {
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-'
		if ok {
			b.WriteRune(c)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.TrimLeft(b.String(), "._-")
	out = strings.TrimRight(out, "-")
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func parseRepoURL(raw string) (host, path string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ""
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.ReplaceAll(s, `\`, "/")
	if i := strings.Index(s, "://"); i >= 0 {
		scheme := strings.ToLower(s[:i])
		rest := stripUserinfo(s[i+len("://"):])
		if scheme == "file" {
			return "", cleanRepoPath(rest, false, true)
		}
		host = rest
		if slash := strings.Index(rest, "/"); slash >= 0 {
			host, path = rest[:slash], rest[slash:]
		}
		host = stripPort(host)
	} else {
		rest := stripUserinfo(s)
		slash := strings.Index(rest, "/")
		colon := strings.Index(rest, ":")
		switch {
		case strings.HasPrefix(rest, "[") && strings.Contains(rest, "]:"):
			end := strings.Index(rest, "]:")
			host, path = rest[1:end], rest[end+2:]
		case colon > 1 && (slash < 0 || colon < slash):
			host, path = rest[:colon], rest[colon+1:]
		case slash > 0 && looksLikeHost(rest[:slash]):
			host, path = rest[:slash], rest[slash:]
		default:
			return "", cleanRepoPath(rest, false, true)
		}
	}
	host = strings.ToLower(strings.Trim(host, "."))
	if host == "" {
		return "", cleanRepoPath(path, false, true)
	}
	return host, cleanRepoPath(path, caseInsensitiveRepoHosts[host], false)
}

func looksLikeHost(seg string) bool {
	if seg == "" || !strings.Contains(seg, ".") {
		return false
	}
	c := seg[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func stripUserinfo(s string) string {
	if at := strings.LastIndex(s, "@"); at >= 0 {
		return s[at+1:]
	}
	return s
}

func stripPort(host string) string {
	if strings.HasPrefix(host, "[") {
		if end := strings.Index(host, "]"); end >= 0 {
			return host[1:end]
		}
		return host
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		return host[:i]
	}
	return host
}

func cleanRepoPath(p string, fold, keepRoot bool) string {
	rooted := keepRoot && strings.HasPrefix(p, "/")
	kept := make([]string, 0, 4)
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." {
			continue
		}
		kept = append(kept, part)
	}
	for len(kept) > 0 {
		last := kept[len(kept)-1]
		switch {
		case strings.EqualFold(last, ".git"):
			kept = kept[:len(kept)-1]
			continue
		case len(last) > len(".git") && strings.EqualFold(last[len(last)-len(".git"):], ".git"):
			kept[len(kept)-1] = last[:len(last)-len(".git")]
			continue
		}
		break
	}
	out := strings.Join(kept, "/")
	if fold {
		out = strings.ToLower(out)
	}
	if rooted && out != "" {
		out = "/" + out
	}
	return out
}
