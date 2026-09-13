package mcp

import (
	"strings"
	"testing"
)

// TestNormalizeRepoURL pins the one function that decides whether two agents
// are in the same repository. Every spelling of mklfarha/taqueria below is
// one repository, and must come out as one string.
func TestNormalizeRepoURL(t *testing.T) {
	const taqueria = "github.com/mklfarha/taqueria"
	cases := []struct {
		in, want string
	}{
		// The two remotes from the production incident.
		{"https://github.com/mklfarha/taqueria.git", taqueria},
		{"git@github.com:mklfarha/taqueria.git", taqueria},

		// Every other way the same remote reaches the server.
		{"https://github.com/mklfarha/taqueria", taqueria},
		{"https://github.com/mklfarha/taqueria/", taqueria},
		{"https://github.com/mklfarha/taqueria.git/", taqueria},
		{"http://github.com/mklfarha/taqueria", taqueria},
		{"git://github.com/mklfarha/taqueria.git", taqueria},
		{"ssh://git@github.com/mklfarha/taqueria.git", taqueria},
		{"ssh://git@github.com:22/mklfarha/taqueria", taqueria},
		{"git+ssh://git@github.com/mklfarha/taqueria.git", taqueria},
		{"github.com:mklfarha/taqueria", taqueria},
		{"HTTPS://GitHub.COM/MKLFarha/Taqueria.GIT", taqueria},
		{"  https://github.com//mklfarha/./taqueria.git  \n", taqueria},
		{"https://github.com:443/mklfarha/taqueria.git", taqueria},
		{"https://github.com/mklfarha/taqueria.git#readme", taqueria},

		// Idempotent: the stored form normalizes to itself.
		{taqueria, taqueria},

		// Other case-insensitive forges fold owner and repo too.
		{"https://gitlab.com/Group/Sub/Project.git", "gitlab.com/group/sub/project"},
		{"git@bitbucket.org:Owner/Repo.git", "bitbucket.org/owner/repo"},

		// A self-hosted forge may be case-sensitive: only the host folds.
		{"https://Git.Example.com:8443/Team/Repo.git", "git.example.com/Team/Repo"},
		{"ssh://git@git.example.com:2222/Team/Repo.git", "git.example.com/Team/Repo"},
		{"git.example.com:Team/Repo.git", "git.example.com/Team/Repo"},
		{"git@[::1]:Team/Repo.git", "::1/Team/Repo"},
		{"ssh://git@[::1]:22/Team/Repo.git", "::1/Team/Repo"},

		// A local path used as a remote keeps its root and its case.
		{"/Users/me/src/Shop/.git", "/Users/me/src/Shop"},
		{"/Users/me/src/Shop.git", "/Users/me/src/Shop"},
		{"file:///Users/me/src/Shop.git", "/Users/me/src/Shop"},
		{"../shop.git", "../shop"},

		// Nothing usable.
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		got := normalizeRepoURL(tc.in)
		if got != tc.want {
			t.Errorf("normalizeRepoURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if again := normalizeRepoURL(got); again != got {
			t.Errorf("not idempotent: normalizeRepoURL(%q) = %q, but normalizing that gives %q", tc.in, got, again)
		}
	}
}

// TestNormalizeRepoURLStripsCredentials: a remote copied out of a CI config
// can carry a credential, and repo_url is stored on a team-visible row. No
// piece of the credential may survive normalization — not the user, not the
// secret, not the '@' that separated them.
//
// The secrets here are deliberately not shaped like any real token format.
func TestNormalizeRepoURLStripsCredentials(t *testing.T) {
	const secret = "notarealsecret"
	const user = "someuser"
	cases := []struct {
		in, want string
	}{
		{"https://" + user + ":" + secret + "@github.com/mklfarha/taqueria.git", "github.com/mklfarha/taqueria"},
		{"https://" + secret + "@github.com/mklfarha/taqueria.git", "github.com/mklfarha/taqueria"},
		{"https://" + user + ":" + secret + "@git.example.com:8443/Team/Repo", "git.example.com/Team/Repo"},
		{"ssh://" + user + ":" + secret + "@git.example.com/Team/Repo.git", "git.example.com/Team/Repo"},
		{user + ":" + secret + "@github.com:mklfarha/taqueria.git", "github.com/mklfarha/taqueria"},
		// An unencoded '/' or '@' in the password must not end the authority
		// early and leak the rest of it into the path.
		{"https://" + user + ":" + secret + "/x@y@github.com/mklfarha/taqueria", "github.com/mklfarha/taqueria"},
		// A token passed as a query parameter.
		{"https://github.com/mklfarha/taqueria.git?access_token=" + secret, "github.com/mklfarha/taqueria"},
		{"file://" + user + ":" + secret + "@/srv/git/shop.git", "/srv/git/shop"},
	}
	for _, tc := range cases {
		got := normalizeRepoURL(tc.in)
		if got != tc.want {
			t.Errorf("normalizeRepoURL(<url with credentials>) = %q, want %q", got, tc.want)
		}
		for _, leaked := range []string{secret, user, "@", "access_token"} {
			if strings.Contains(got, leaked) {
				t.Errorf("credential material %q survived normalization: %q", leaked, got)
			}
		}
	}
}

func TestProjectKeyFromRepoURL(t *testing.T) {
	cases := map[string]string{
		"github.com/mklfarha/taqueria": "taqueria",
		"gitlab.com/group/sub/project": "project",
		"/Users/me/src/Shop":           "Shop",
		"github.com":                   "github.com",
		"":                             "",
	}
	for in, want := range cases {
		if got := projectKeyFromRepoURL(in); got != want {
			t.Errorf("projectKeyFromRepoURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCanonicalRepoURL: project.repo_url is a url column that accepts only
// http and https, so the stored form is the identity behind one fixed scheme,
// and it must normalize back to the identity it came from.
func TestCanonicalRepoURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"git@github.com:MKLFarha/Taqueria.git", "https://github.com/mklfarha/taqueria"},
		{"https://github.com/mklfarha/taqueria.git", "https://github.com/mklfarha/taqueria"},
		{"https://someuser:notarealsecret@git.example.com:8443/Team/Repo.git", "https://git.example.com/Team/Repo"},
		{"ssh://git@[::1]:22/Team/Repo.git", "https://[::1]/Team/Repo"},
		{"https://github.com", "https://github.com"},
		// No host, no URL: identified by project_key alone.
		{"/Users/me/src/Shop/.git", ""},
		{"file:///srv/git/shop.git", ""},
		{"", ""},
	}
	for _, tc := range cases {
		got := canonicalRepoURL(tc.in)
		if got != tc.want {
			t.Errorf("canonicalRepoURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got != "" && normalizeRepoURL(got) != normalizeRepoURL(tc.in) {
			t.Errorf("canonical %q normalizes to %q, but the input normalizes to %q",
				got, normalizeRepoURL(got), normalizeRepoURL(tc.in))
		}
		if canonicalRepoURL(got) != got {
			t.Errorf("canonicalRepoURL is not idempotent on %q", got)
		}
		if strings.Contains(got, "notarealsecret") || strings.Contains(got, "someuser") {
			t.Errorf("credential material survived into the stored form: %q", got)
		}
	}
	if k := projectKeyFromRepoURL("https://github.com/mklfarha/taqueria"); k != "taqueria" {
		t.Errorf("projectKeyFromRepoURL(canonical) = %q, want taqueria", k)
	}
}
