package cmd

import (
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mklfarha/metiche/cli/internal/config"
	"github.com/mklfarha/metiche/cli/internal/wire"
)

// signinFileSeconds is how long the redirect file stays on disk after the
// opener returns (docs/CLI.md §1.5, the installer's BOARD_FILE_SECONDS).
const signinFileSeconds = 10

func (a *app) cmdOpen(args []string) error {
	fs := a.flags("open")
	printOnly := fs.Bool("print", false, "print the address (or, for a private team, a sign-in link) and open nothing")
	signin := fs.Bool("signin", false, "sign this browser in even for a public team")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return fail(exitUsage, "usage", "usage: metiche open [<slug>] [--print] [--signin]")
	}
	s, err := a.connect(false)
	if err != nil {
		return err
	}
	defer s.close()
	ref, err := a.resolveTeam(s, first(pos))
	if err != nil {
		return err
	}
	var st wire.TeamState
	if err := s.c.Call(a.ctx(), "get_team_state", map[string]any{"team_slug": ref.slug, "scope": "projects", "limit": 1}, &st); err != nil {
		return a.refused(a.mapErr(err), ref)
	}
	if st.Team == nil {
		return fail(exitError, "server_too_old", "this metiche server does not say whether a team is public; see `metiche status --team %s`", ref.slug)
	}
	opener := a.opener()
	noOpen := *printOnly || a.json || opener == "" || a.env("SSH_CONNECTION") != "" || a.env("SSH_TTY") != ""

	if st.Team.Visibility == "public" && !*signin {
		url := config.BoardURL(config.BoardBase(a.env), ref.slug)
		if a.json {
			d := doc("open")
			d["team"], d["visibility"], d["board_url"], d["login_url"], d["login_expires_at"] = ref.slug, "public", url, nil, nil
			a.writeJSON(d)
			return nil
		}
		if noOpen {
			a.out("%s", url)
			return nil
		}
		if err := runOpener(opener, url); err != nil {
			a.out("%s", url)
			return nil
		}
		a.out("opened %s in your browser.", url)
		return nil
	}

	as := s
	if s.who.TokenScope != "agent" {
		as, err = a.connect(true)
		if err != nil {
			return err
		}
		defer as.close()
	}
	var ob wire.OpenBoard
	if err := as.c.Call(a.ctx(), "open_board", map[string]any{"team_slug": ref.slug, "requested_via": "cli"}, &ob); err != nil {
		me := a.mapErr(err).(*cliError)
		switch me.code {
		case "not_permitted":
			return fail(exitNoCred, "no_agent_token", "open_board needs a client's own token; re-run the installer: curl -fsSL https://metiche.xyz/install.sh | sh")
		case "server_too_old":
			return fail(exitError, "server_too_old", "this metiche server cannot sign a browser in; see `metiche status --team %s`", ref.slug)
		}
		return a.refused(me, ref)
	}
	expires := ob.LoginExpiresAt.UTC().Format("15:04 UTC")
	if a.json {
		d := doc("open")
		d["team"], d["visibility"], d["board_url"], d["login_url"], d["login_expires_at"] = ref.slug, st.Team.Visibility, ob.BoardURL, ob.LoginURL, ob.LoginExpiresAt.UTC().Format(time.RFC3339)
		d["note"] = "login_url is a live sign-in link: it signs one browser in, works once, and expires at login_expires_at. Do not share it."
		a.writeJSON(d)
		return nil
	}
	if noOpen {
		a.out("%s", ob.LoginURL)
		a.out("That link signs ONE browser in to %s, works once, and expires at %s (10 minutes). Do not share it.", ref.slug, expires)
		a.out("The board's plain address: %s", ob.BoardURL)
		return nil
	}
	from := ""
	if ref.source == "binding" {
		from = " (from " + ref.path + ")"
	}
	if st.Team.Visibility == "private" {
		a.out("%s is private%s: opening its board signed in.", ref.slug, from)
	}
	if err := a.openSignin(opener, ob.LoginURL); err != nil {
		return fail(exitError, "open_failed", "could not open the browser (%v). Run: metiche open --print (a new link, which works once, for 10 minutes)", err)
	}
	a.out("opened %s in your browser.", ob.BoardURL)
	a.out("If nothing opened: metiche open --print (a new link, which works once, for 10 minutes).")
	return nil
}

// opener is `open` on darwin and `xdg-open` on linux, from PATH.
func (a *app) opener() string {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

func runOpener(opener, arg string) error {
	c := exec.Command(opener, arg)
	c.Env = envWithoutToken()
	return c.Run()
}

func envWithoutToken() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "METICHE_TOKEN=") {
			out = append(out, kv)
		}
	}
	return out
}

// openSignin hands the link to the browser through a 0600 file in a fresh
// 0700 directory, never on argv, and removes the directory after the browser
// has had time to read it.
func (a *app) openSignin(opener, loginURL string) error {
	dir, err := os.MkdirTemp("", "metiche-open-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	file := filepath.Join(dir, "metiche-signin.html")
	esc := html.EscapeString(loginURL)
	js := strconv.Quote(loginURL)
	page := fmt.Sprintf("<!doctype html>\n<meta charset=\"utf-8\">\n<meta name=\"referrer\" content=\"no-referrer\">\n"+
		"<meta http-equiv=\"refresh\" content=\"0; url=%s\">\n<title>metiche: signing in</title>\n<script>location.replace(%s)</script>\n", esc, js)
	if err := os.WriteFile(file, []byte(page), 0o600); err != nil {
		return err
	}
	if err := runOpener(opener, file); err != nil {
		return err
	}
	wait := signinFileSeconds
	if v, err := strconv.Atoi(a.env("METICHE_CLI_SIGNIN_FILE_SECONDS")); err == nil && v >= 1 && v < wait {
		wait = v
	}
	time.Sleep(time.Duration(wait) * time.Second)
	return nil
}
