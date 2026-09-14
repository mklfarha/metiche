// Package execx runs the few external commands the CLI may run, and nothing
// else (docs/CLI.md §5): a fixed argv allowlist, a timeout, stdin closed,
// output capped, and no metiche token in the child's environment.
package execx

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// allowed is every argv the binary may run. "<remote>" is a git remote name.
var allowed = [][]string{
	{"git", "rev-parse", "--show-toplevel"},
	{"git", "config", "--get", "remote.origin.url"},
	{"git", "config", "--get", "remote.<remote>.url"},
	{"git", "remote"},
	{"git", "check-ignore", "-q", ".metiche"},
	{"git", "ls-files", "--error-unmatch", ".metiche"},
}

var remoteName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ErrNotAllowed is a command outside the allowlist.
var ErrNotAllowed = errors.New("execx: command not on the allowlist")

// Allowed reports whether argv is on the allowlist.
func Allowed(argv []string) bool {
	for _, a := range allowed {
		if len(a) != len(argv) {
			continue
		}
		ok := true
		for i := range a {
			if a[i] == argv[i] {
				continue
			}
			if a[i] == "remote.<remote>.url" && strings.HasPrefix(argv[i], "remote.") && strings.HasSuffix(argv[i], ".url") &&
				remoteName.MatchString(strings.TrimSuffix(strings.TrimPrefix(argv[i], "remote."), ".url")) {
				continue
			}
			ok = false
			break
		}
		if ok {
			return true
		}
	}
	return false
}

const maxOutput = 1 << 20

// Result is a finished command.
type Result struct {
	Stdout   string
	ExitCode int
}

// Run runs an allowed command in dir. A missing binary is an *exec.Error.
func Run(ctx context.Context, dir string, argv ...string) (Result, error) {
	if !Allowed(argv) {
		return Result{}, ErrNotAllowed
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Dir = dir
	c.Stdin = nil
	c.Env = childEnv()
	var out capped
	c.Stdout = &out
	c.Stderr = nil
	err := c.Run()
	res := Result{Stdout: out.String()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, err
}

// childEnv never hands a metiche credential to a child process, and never lets
// git prompt.
func childEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "METICHE_TOKEN=") || strings.HasPrefix(kv, "GIT_TERMINAL_PROMPT=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0")
}

type capped struct{ bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := maxOutput - c.Len(); room > 0 {
		if len(p) > room {
			c.Buffer.Write(p[:room])
		} else {
			c.Buffer.Write(p)
		}
	}
	return len(p), nil
}
