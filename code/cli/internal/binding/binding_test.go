package binding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParse(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, FileName)
	write(t, p, "# comment\nteam = hack-night        # required: the team slug\nproject = orbital\nproject = orbital-2\ntoken = x\nremote = https://u:p@host/x\n")
	f, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Team != "hack-night" || f.Project != "orbital-2" || len(f.Duplicates) != 1 || f.Duplicates[0] != "project" {
		t.Errorf("parsed %+v", f)
	}
	if strings.Join(f.CredentialLike, ",") != "token,remote" {
		t.Errorf("credential-like keys = %v", f.CredentialLike)
	}
}

// TestResolveIgnoresProjectAboveTheGitRoot: team inherits, project does not.
func TestResolveIgnoresProjectAboveTheGitRoot(t *testing.T) {
	top := t.TempDir()
	repo := filepath.Join(top, "repo")
	write(t, filepath.Join(top, FileName), "team = hackathon\nproject = everything\n")
	_ = os.MkdirAll(filepath.Join(repo, "app"), 0o755)
	e := Resolve(Chain(filepath.Join(repo, "app")), realpath(t, repo))
	if e.Team != "hackathon" || e.Project != "" || len(e.IgnoredProjects) != 1 {
		t.Errorf("resolved %+v", e)
	}
	write(t, filepath.Join(repo, FileName), "team = own\nproject = repo\n")
	e = Resolve(Chain(filepath.Join(repo, "app")), realpath(t, repo))
	if e.Team != "own" || e.Project != "repo" || !strings.HasSuffix(e.TeamPath, filepath.Join("repo", FileName)) {
		t.Errorf("nearest wins: %+v", e)
	}
}

// TestChainSkipsTheInstallersDirectory: ~/.metiche is a directory, never a
// binding, for every working directory under $HOME.
func TestChainSkipsTheInstallersDirectory(t *testing.T) {
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, FileName, "bin"), 0o700)
	_ = os.MkdirAll(filepath.Join(home, "work", "repo"), 0o755)
	for _, f := range Chain(filepath.Join(home, "work", "repo")) {
		if strings.HasPrefix(f.Path, realpath(t, home)) {
			t.Errorf("the directory %s was read as a binding: %+v", f.Path, f)
		}
	}
}

func realpath(t *testing.T, p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRenderAndRewrite(t *testing.T) {
	got := string(Render("taqueria-tracker", "taqueria"))
	if !strings.HasSuffix(got, "team = taqueria-tracker\nproject = taqueria\n") || strings.Count(got, "\n") != 4 {
		t.Errorf("render = %q", got)
	}
	if strings.Contains(string(Render("t", "")), "project =") {
		t.Error("an empty project must not be written")
	}
	old := "# mine\nteam = a   # was\nextra = keep\nproject = p\n"
	re := string(Rewrite([]byte(old), "b", "q"))
	if re != "# mine\nteam = b\nextra = keep\nproject = q\n" {
		t.Errorf("rewrite = %q", re)
	}
	if re := string(Rewrite([]byte(old), "b", "")); strings.Contains(re, "project") || !strings.Contains(re, "extra = keep") {
		t.Errorf("rewrite without project = %q", re)
	}
}

func TestWriteIsAtomicAndRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Render("t", "p")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v err %v", st.Mode(), err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".metiche.tmp-*"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left: %v", leftovers)
	}
	link := t.TempDir()
	_ = os.WriteFile(filepath.Join(link, "elsewhere"), []byte("x"), 0o644)
	if err := os.Symlink(filepath.Join(link, "elsewhere"), filepath.Join(link, FileName)); err != nil {
		t.Fatal(err)
	}
	if err := Write(link, Render("t", "p")); err == nil {
		t.Error("wrote through a symlinked .metiche")
	}
	if b, _ := os.ReadFile(filepath.Join(link, "elsewhere")); string(b) != "x" {
		t.Error("the symlink target changed")
	}
	_ = time.Now
}
