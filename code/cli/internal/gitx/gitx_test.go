package gitx

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mklfarha/metiche/cli/internal/secret"
)

// TestRepoURLVectors is the CLI side of the shared vector file; the backend
// runs the same file (app/mcp/repourl_vectors_test.go).
func TestRepoURLVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "repourl_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Vectors []struct{ In, Normalized, Canonical, Key string }
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, v := range doc.Vectors {
		if got := NormalizeRepoURL(v.In); got != v.Normalized {
			t.Errorf("NormalizeRepoURL(%q) = %q, want %q", v.In, got, v.Normalized)
		}
		if got := CanonicalRepoURL(v.In); got != v.Canonical {
			t.Errorf("CanonicalRepoURL(%q) = %q, want %q", v.In, got, v.Canonical)
		}
		if got := ProjectKeyFromRepoURL(v.In); got != v.Key {
			t.Errorf("ProjectKeyFromRepoURL(%q) = %q, want %q", v.In, got, v.Key)
		}
	}
}

func TestKeyFromDirName(t *testing.T) {
	for in, want := range map[string]string{"taqueria": "taqueria", "Taqueria Tracker": "taqueria-tracker", "_private.repo": "private.repo", "My App!!": "my-app"} {
		if got := KeyFromDirName(in); got != want {
			t.Errorf("KeyFromDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDetectStripsAndRedactsACredentialRemote: userinfo never reaches the
// normalized URL, and the raw remote seeds the redactor.
func TestDetectStripsAndRedactsACredentialRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	const canary = "CANARYuserinfo0123456789"
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://someone:" + canary + "@github.com/Example/Demo.git"}} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	sub := filepath.Join(dir, "a", "b")
	_ = os.MkdirAll(sub, 0o755)
	r := Detect(context.Background(), sub)
	real, _ := filepath.EvalSymlinks(dir)
	if r.Root != real || r.Remote != "origin" || r.RepoURL != "github.com/example/demo" {
		t.Errorf("Detect = %+v", r)
	}
	if strings.Contains(r.RepoURL, canary) {
		t.Error("the credential reached RepoURL")
	}
	if got := secret.Default.Redact("fatal: could not read https://someone:" + canary + "@github.com/x"); strings.Contains(got, canary) {
		t.Errorf("the redactor was not seeded: %s", got)
	}
	if Tracked(context.Background(), dir) || Ignored(context.Background(), dir) {
		t.Error("an absent .metiche is neither tracked nor ignored")
	}
	outside := t.TempDir()
	if r := Detect(context.Background(), outside); r.Root != "" && !strings.HasPrefix(outside, r.Root) {
		t.Errorf("outside a repository Detect = %+v", r)
	}
}
