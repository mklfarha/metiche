package execx

import (
	"context"
	"strings"
	"testing"
)

func TestAllowlist(t *testing.T) {
	yes := [][]string{
		{"git", "rev-parse", "--show-toplevel"}, {"git", "remote"}, {"git", "config", "--get", "remote.origin.url"},
		{"git", "config", "--get", "remote.upstream-2.url"}, {"git", "check-ignore", "-q", ".metiche"},
		{"git", "ls-files", "--error-unmatch", ".metiche"},
	}
	no := [][]string{
		{"git", "add", ".metiche"}, {"git", "commit", "-m", "x"}, {"git", "config", "--get", "remote.a;rm -rf.url"},
		{"git", "config", "user.name", "x"}, {"git", "fetch"}, {"sh", "-c", "x"}, {"claude", "mcp", "add"}, {"git", "remote", "-v"},
	}
	for _, a := range yes {
		if !Allowed(a) {
			t.Errorf("%v should be allowed", a)
		}
	}
	for _, a := range no {
		if Allowed(a) {
			t.Errorf("%v must not be allowed", a)
		}
		if _, err := Run(context.Background(), ".", a...); err != ErrNotAllowed {
			t.Errorf("Run(%v) = %v, want ErrNotAllowed", a, err)
		}
	}
}

func TestChildEnvCarriesNoToken(t *testing.T) {
	t.Setenv("METICHE_TOKEN", "mtk_CANARY_child_env_0123456789")
	for _, kv := range childEnv() {
		if strings.HasPrefix(kv, "METICHE_TOKEN=") {
			t.Fatal("a child process would receive METICHE_TOKEN")
		}
	}
}
