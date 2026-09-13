package mcp

import (
	"strings"
)

// A project is a REPOSITORY, and the one thing two agents in the same
// repository can both report without agreeing on anything is its remote.
// project_key is a name a model picks: one agent started above the git root
// picked "taqueria_tracker", another inside it picked "taqueria", and because
// claims are scoped per project the two of them edited app/rest.go at the
// same time with nobody told. The remote URL is what ties them together.
//
// But the remote is not one string either. The same repository reaches the
// server as https or ssh, scp-style or not, with or without .git, with or
// without a trailing slash, in whatever case the person typed the owner — and
// sometimes with a credential embedded in it. parseRepoURL folds all of those
// into one credential-free host and path, and two spellings are the same
// repository exactly when normalizeRepoURL gives the same string for both.

// caseInsensitiveRepoHosts are forges whose owner and repository names are
// case-insensitive, so MkLFarha/Taqueria and mklfarha/taqueria are one repo.
// A self-hosted forge may well be case-sensitive, so only the host is folded
// there.
var caseInsensitiveRepoHosts = map[string]bool{
	"github.com":    true,
	"gitlab.com":    true,
	"bitbucket.org": true,
}

// normalizeRepoURL returns the scheme-less, credential-free identity of a git
// remote, or "" when there is nothing usable in it. It is what projects are
// COMPARED by.
//
//	https://github.com/Owner/Repo.git       -> github.com/owner/repo
//	git@github.com:owner/repo.git           -> github.com/owner/repo
//	ssh://git@github.com:22/owner/repo      -> github.com/owner/repo
//	https://user:secret@git.example/x/y/    -> git.example/x/y
//	/home/me/src/repo/.git                  -> /home/me/src/repo
//
// It is pure and idempotent, and it must stay both: it runs inside the team
// lock, and it is applied to every STORED repo_url when projects are
// compared, so a row written before normalization existed still matches.
func normalizeRepoURL(raw string) string {
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

// canonicalRepoURL is the form a repo_url is STORED in: https://host/path.
//
// project.repo_url is a url-typed column and the generated validation accepts
// only http and https, so the identity cannot be stored bare; prefixing the
// one scheme keeps stored values comparable byte for byte and still
// normalizes back to the identity. A remote with no host — a local path used
// as a remote — has no URL form at all and returns "": such a checkout is
// identified by its project_key, exactly as before repo_url mattered.
func canonicalRepoURL(raw string) string {
	host, path := parseRepoURL(raw)
	if host == "" {
		return ""
	}
	if strings.Contains(host, ":") { // an IPv6 literal
		host = "[" + host + "]"
	}
	if path == "" {
		return "https://" + host
	}
	return "https://" + host + "/" + path
}

// parseRepoURL splits a remote into a lowercased host and a cleaned path.
// host is "" for a local path.
//
// Credentials are dropped by cutting at the LAST '@' rather than by parsing
// the authority, because a password is not always percent-encoded, and one
// containing a '/' would otherwise end the authority early and survive into
// the path. A repository path containing an '@' is vanishingly rare; a token
// surviving into a stored, team-visible column is not an acceptable failure.
// For the same reason the query string and fragment are dropped outright.
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
			// scp-like with a bracketed IPv6 host: [::1]:owner/repo.
			end := strings.Index(rest, "]:")
			host, path = rest[1:end], rest[end+2:]
		case colon > 1 && (slash < 0 || colon < slash):
			// scp-like: [user@]host:owner/repo. colon > 1 so a Windows drive
			// letter (C:/src/repo) is a local path and not a host called "c".
			host, path = rest[:colon], rest[colon+1:]
		case slash > 0 && looksLikeHost(rest[:slash]):
			// A bare host/owner/repo — which is also normalizeRepoURL's own
			// output, so normalizing twice is a no-op.
			host, path = rest[:slash], rest[slash:]
		default:
			// A local path used as a remote. There is no host to fold, and
			// the userinfo cut above is harmless on a path with no '@'.
			return "", cleanRepoPath(rest, false, true)
		}
	}

	host = strings.ToLower(strings.Trim(host, "."))
	if host == "" {
		return "", cleanRepoPath(path, false, true)
	}
	return host, cleanRepoPath(path, caseInsensitiveRepoHosts[host], false)
}

// looksLikeHost says whether the first segment of a scheme-less remote is a
// host (github.com) rather than a relative path (../repo, ./repo).
func looksLikeHost(seg string) bool {
	if seg == "" || !strings.Contains(seg, ".") {
		return false
	}
	c := seg[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// stripUserinfo drops everything up to the last '@'. See parseRepoURL for
// why this is deliberately greedy.
func stripUserinfo(s string) string {
	if at := strings.LastIndex(s, "@"); at >= 0 {
		return s[at+1:]
	}
	return s
}

// stripPort removes a :port from a host. https on 443 and ssh on 22 (or on a
// forge's 2222) are the same repository, so the port is never identity.
func stripPort(host string) string {
	if strings.HasPrefix(host, "[") { // [::1]:22
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

// cleanRepoPath drops empty and "." segments and a trailing .git (as a suffix
// or as the checkout's own .git directory), and lowercases when the forge is
// case-insensitive. keepRoot preserves a leading '/' for a local absolute
// path, so /a/repo and a/repo stay different repositories.
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

// projectKeyFromRepoURL is the key a project gets when the agent sent a
// repo_url and no project_key: the repository's own name, which is what the
// basename of the git root almost always is. It takes any form — identity or
// canonical — and reads the last path segment of its identity.
func projectKeyFromRepoURL(repoURL string) string {
	host, path := parseRepoURL(repoURL)
	if path == "" {
		return host
	}
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}
