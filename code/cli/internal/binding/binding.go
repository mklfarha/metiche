// Package binding reads .metiche files nearest-wins and writes the one file
// the binary ever writes (docs/CLI.md §1.10, §7).
//
// Format (PLAN.md): `key = value` lines, `#` comments. `team` names the team;
// `project` names the project, and counts only from a file at or below the git
// root. Never a credential, a join code or a URL.
package binding

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// FileName is .metiche.
const FileName = ".metiche"

// KeyPattern is what a project key must look like to be written.
var KeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// File is one parsed .metiche.
type File struct {
	Path           string
	Dir            string
	Team           string
	Project        string
	HasTeam        bool
	HasProject     bool
	Duplicates     []string
	Unknown        []string
	CredentialLike []string // keys whose name or value looks like a secret; values never kept
	NotRegular     bool
}

var secretKey = regexp.MustCompile(`(?i)token|secret|code|key|password`)

// Parse reads one file. A symlink or non-regular file is reported, not read.
func Parse(path string) (File, error) {
	f := File{Path: path, Dir: filepath.Dir(path)}
	st, err := os.Lstat(path)
	if err != nil {
		return f, err
	}
	if !st.Mode().IsRegular() {
		f.NotRegular = true
		return f, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		k, v, ok := splitLine(sc.Text())
		if !ok {
			continue
		}
		if seen[k] {
			f.Duplicates = append(f.Duplicates, k)
		}
		seen[k] = true
		switch k {
		case "team":
			f.Team, f.HasTeam = v, v != ""
		case "project":
			f.Project, f.HasProject = v, v != ""
		default:
			f.Unknown = append(f.Unknown, k)
			if secretKey.MatchString(k) {
				f.CredentialLike = append(f.CredentialLike, k)
			}
		}
		if strings.Contains(v, "://") && strings.Contains(v, "@") {
			f.CredentialLike = append(f.CredentialLike, k)
		}
	}
	return f, nil
}

// splitLine returns key and value for a `key = value` line, dropping a comment.
func splitLine(line string) (string, string, bool) {
	if i := strings.Index(line, "#"); i >= 0 {
		line = line[:i]
	}
	k, v, ok := strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}
	k, v = strings.TrimSpace(k), strings.TrimSpace(v)
	if k == "" {
		return "", "", false
	}
	return strings.ToLower(k), v, true
}

// Chain is every .metiche from start up to /, nearest first.
func Chain(start string) []File {
	d, err := filepath.Abs(start)
	if err != nil {
		return nil
	}
	if real, err := filepath.EvalSymlinks(d); err == nil {
		d = real
	}
	var out []File
	for {
		// A DIRECTORY named .metiche is not a binding: ~/.metiche is the
		// installer's own directory, and every working directory under $HOME
		// walks past it.
		p := filepath.Join(d, FileName)
		if st, err := os.Lstat(p); err == nil && !st.IsDir() {
			if f, err := Parse(p); err == nil {
				out = append(out, f)
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return out
		}
		d = parent
	}
}

// Effective is the binding in force at a directory.
type Effective struct {
	Team        string
	TeamPath    string
	Project     string
	ProjectPath string
	// IgnoredProjects are files above the git root that set project.
	IgnoredProjects []File
}

// within reports whether dir is root or below it.
func within(dir, root string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Resolve applies nearest-wins: team inherits from any ancestor, project only
// from a file at or below gitRoot. Outside a repository, project counts from
// the nearest file that has one.
func Resolve(chain []File, gitRoot string) Effective {
	var e Effective
	for _, f := range chain {
		if f.NotRegular {
			continue
		}
		if e.TeamPath == "" && f.HasTeam {
			e.Team, e.TeamPath = f.Team, f.Path
		}
		if f.HasProject {
			if gitRoot == "" || within(f.Dir, gitRoot) {
				if e.ProjectPath == "" {
					e.Project, e.ProjectPath = f.Project, f.Path
				}
			} else {
				e.IgnoredProjects = append(e.IgnoredProjects, f)
			}
		}
	}
	return e
}

const header = "# metiche: which team (and project) agent sessions in this repository belong to.\n" +
	"# Safe to commit: it names a team and grants no access. Written by `metiche init`.\n"

// Render is a fresh file.
func Render(team, project string) []byte {
	var b bytes.Buffer
	b.WriteString(header)
	fmt.Fprintf(&b, "team = %s\n", team)
	if project != "" {
		fmt.Fprintf(&b, "project = %s\n", project)
	}
	return b.Bytes()
}

// Rewrite changes only the team and project lines of an existing file,
// keeping comments and unknown keys. An empty project removes the line.
func Rewrite(existing []byte, team, project string) []byte {
	lines := strings.SplitAfter(string(existing), "\n")
	var out strings.Builder
	wroteTeam, wroteProject := false, false
	for _, line := range lines {
		if line == "" {
			continue
		}
		k, _, ok := splitLine(line)
		switch {
		case ok && k == "team":
			if !wroteTeam {
				out.WriteString("team = " + team + "\n")
				wroteTeam = true
			}
		case ok && k == "project":
			if !wroteProject && project != "" {
				out.WriteString("project = " + project + "\n")
			}
			wroteProject = true
		default:
			if !strings.HasSuffix(line, "\n") {
				line += "\n"
			}
			out.WriteString(line)
		}
	}
	if !wroteTeam {
		out.WriteString("team = " + team + "\n")
	}
	if !wroteProject && project != "" {
		out.WriteString("project = " + project + "\n")
	}
	return []byte(out.String())
}

// ErrRefused is a write the package will not make.
var ErrRefused = errors.New("refused")

// Write puts content at dir/.metiche atomically, mode 0644. It refuses a
// target that is a symlink or not a regular file, and a directory not owned
// by the current user.
func Write(dir string, content []byte) error {
	target := filepath.Join(dir, FileName)
	if st, err := os.Lstat(target); err == nil && !st.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file (a symlink or something else); remove it first", ErrRefused, target)
	}
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("%w: %s is not owned by you", ErrRefused, dir)
	}
	tmp, err := os.CreateTemp(dir, ".metiche.tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, target)
}
