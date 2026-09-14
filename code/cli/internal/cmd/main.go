// Package cmd is the metiche command line: one dispatcher, stdlib flag, the
// exit-code mapping in one place (docs/CLI.md §1).
package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mklfarha/metiche/cli/internal/config"
	"github.com/mklfarha/metiche/cli/internal/secret"
)

// Exit codes (§1.2).
const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitNoCred   = 3
	exitNoServer = 4
	exitRefused  = 5
	exitConflict = 6
)

// cliError is every failure a command returns; run maps it to output and an
// exit code.
type cliError struct {
	// silent: the command already printed everything (doctor's report); only
	// the exit code remains.
	silent bool
	exit   int
	code   string // stable machine code for --json
	detail string
	extra  map[string]any
}

func (e *cliError) Error() string { return e.detail }

func fail(exit int, code, format string, args ...any) *cliError {
	return &cliError{exit: exit, code: code, detail: fmt.Sprintf(format, args...)}
}

// app is one invocation.
type app struct {
	stdin  io.Reader
	in     *bufio.Reader
	stdout io.Writer
	stderr io.Writer
	env    config.Env
	cwd    string

	json    bool
	urlFlag string
	timeout time.Duration
	noColor bool

	endpoint config.Endpoint
	paths    config.Paths
	tty      bool
	hostname func() (string, error)
	now      func() time.Time
}

// Main runs the CLI and returns the exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cwd, _ := os.Getwd()
	a := &app{
		stdin: stdin, in: bufio.NewReader(stdin), stdout: stdout, stderr: stderr,
		env: os.Getenv, cwd: cwd, timeout: 15 * time.Second,
		hostname: os.Hostname, now: time.Now,
	}
	a.tty = isTerminal(stdin) && isTerminal(stdout)
	return a.run(args)
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

type command struct {
	name    string
	summary string
	run     func(a *app, args []string) error
}

func commands() []command {
	return []command{
		{"teams", "list your teams; teams create | show", (*app).cmdTeams},
		{"version", "print the version", (*app).cmdVersion},
		{"uninstall", "print how to remove metiche (the installer does it)", (*app).cmdUninstall},
	}
}

func (a *app) usage(w io.Writer) {
	fmt.Fprintln(w, "metiche — a person's view of metiche")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Usage: metiche <command> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-10s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Global flags (every command):")
	fmt.Fprintln(w, "  --json            one JSON document on stdout")
	fmt.Fprintln(w, "  --url <url>       MCP endpoint (default: $METICHE_MCP_URL, else "+config.DefaultEndpoint+")")
	fmt.Fprintln(w, "  --timeout <dur>   per network call, default 15s")
	fmt.Fprintln(w, "  --no-color        plain output (also NO_COLOR)")
	fmt.Fprintln(w, "  --version         same as `metiche version`")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Tokens are never accepted as arguments: metiche reads them from $METICHE_TOKEN or ~/.metiche/env.")
}

func (a *app) run(args []string) int {
	// A bare --version or -h before any command.
	if len(args) == 0 {
		a.usage(a.stdout)
		return exitUsage
	}
	switch args[0] {
	case "-h", "--help", "help":
		a.usage(a.stdout)
		return exitOK
	case "--version", "-version":
		args = append([]string{"version"}, args[1:]...)
	}
	name := args[0]
	for _, c := range commands() {
		if c.name != name {
			continue
		}
		// --json is honoured even when flag parsing fails later.
		for _, x := range args[1:] {
			if x == "--json" || x == "-json" {
				a.json = true
			}
		}
		err := c.run(a, args[1:])
		return a.finish(err)
	}
	a.json = containsFlag(args, "--json")
	return a.finish(fail(exitUsage, "usage", "unknown command %q; run `metiche --help`", name))
}

func containsFlag(args []string, f string) bool {
	for _, x := range args {
		if x == f {
			return true
		}
	}
	return false
}

func (a *app) finish(err error) int {
	if err == nil {
		return exitOK
	}
	var ce *cliError
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if !errors.As(err, &ce) {
		ce = &cliError{exit: exitError, code: "error", detail: err.Error()}
	}
	ce.detail = secret.Default.Redact(ce.detail)
	if ce.silent {
		return ce.exit
	}
	if a.json {
		doc := map[string]any{"schema": "metiche.cli.error/1", "ok": false, "exit": ce.exit, "error": ce.code, "detail": ce.detail}
		for k, v := range ce.extra {
			doc[k] = v
		}
		a.writeJSON(doc)
	} else {
		fmt.Fprintln(a.stderr, "metiche: "+ce.detail)
	}
	return ce.exit
}

// globals registers the global flags on a command's FlagSet.
func (a *app) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("metiche "+name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	fs.BoolVar(&a.json, "json", a.json, "one JSON document on stdout")
	fs.StringVar(&a.urlFlag, "url", "", "MCP endpoint")
	fs.DurationVar(&a.timeout, "timeout", 15*time.Second, "per network call")
	fs.BoolVar(&a.noColor, "no-color", false, "plain output")
	return fs
}

// parse parses flags interspersed with positional arguments, and resolves the
// endpoint and paths.
func (a *app) parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, flag.ErrHelp
			}
			return nil, fail(exitUsage, "usage", "%v", err)
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	ep, err := config.ResolveEndpoint(a.urlFlag, a.env)
	if err != nil {
		return nil, fail(exitUsage, "usage", "%v", err)
	}
	a.endpoint = ep
	if ep.Warning != "" && !a.json {
		fmt.Fprintln(a.stderr, "metiche: warning: "+ep.Warning)
	}
	a.paths = config.ResolvePaths(a.env)
	return pos, nil
}

func (a *app) ctx() context.Context { return context.Background() }

// out prints human text; nothing with --json.
func (a *app) out(format string, args ...any) {
	if a.json {
		return
	}
	fmt.Fprintf(a.stdout, format+"\n", args...)
}

// note prints a diagnostic on stderr, redacted.
func (a *app) note(format string, args ...any) {
	fmt.Fprintln(a.stderr, secret.Default.Redact(fmt.Sprintf(format, args...)))
}

func (a *app) writeJSON(v any) {
	enc := json.NewEncoder(a.stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// doc starts a JSON document for a command.
func doc(schema string) map[string]any {
	return map[string]any{"schema": "metiche.cli." + schema + "/1", "ok": true}
}

func oneOf(v string, allowed ...string) bool {
	for _, x := range allowed {
		if strings.EqualFold(v, x) {
			return true
		}
	}
	return false
}
