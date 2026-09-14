package main

// End to end: the real metiche binary against a locally built backend on a
// fresh database, with scratch HOMEs whose configs the real install.sh wrote.
// Nothing here contacts production.
//
// Gated, because it needs docker and a MySQL container of its own:
//
//	METICHE_CLI_E2E=1
//	METICHE_CLI_E2E_DB_CONTAINER=cli-db       a mysql:8.4 container whose
//	                                          /root/.my.cnf lets `mysql` in
//	METICHE_CLI_E2E_DB_PORT=33490             its published 127.0.0.1 port
//	METICHE_CLI_E2E_DB_PASSWORD_FILE=<0600>   root's password, never printed
//
//	go test -run TestE2E -v -count=1 .

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

type e2e struct {
	t         *testing.T
	root      string // scratch dir, 0700
	repo      string // the metiche repository
	bin       string // the CLI binary
	fakebin   string
	openLog   string
	mcpURL    string
	boardBase string
	db        string
	container string

	mu      sync.Mutex
	outputs []recorded
	machine map[string]string // home -> METICHE_MACHINE_ID its installer used
}

type recorded struct {
	label       string
	text        string
	signinAsked bool // open --print / --json: a sign-in link is expected
}

var tokenShaped = regexp.MustCompile(`mtk_[A-Za-z0-9_-]{16,}`)

func TestE2E(t *testing.T) {
	if os.Getenv("METICHE_CLI_E2E") != "1" {
		t.Skip("set METICHE_CLI_E2E=1 (and the METICHE_CLI_E2E_DB_* variables) to run the end-to-end test")
	}
	e := setupE2E(t)

	ana := e.home("ana")
	e.installer(ana, "install-ana", map[string]string{"METICHE_TEAM_NAME": "E2E Alpha", "METICHE_MEMBER_NAME": "Ana", "METICHE_MACHINE_ID": "ana-laptop"}, true)
	anaCursor := e.cursorToken(ana)

	t.Run("version and uninstall", func(t *testing.T) {
		out, _, code := e.run(ana, nil, "version")
		expect(t, code == 0 && strings.Contains(out, "metiche e2e"), "version: %d %q", code, out)
		out, _, code = e.run(ana, nil, "uninstall")
		expect(t, code == 0 && strings.Contains(out, "sh -s -- --uninstall"), "uninstall: %d %q", code, out)
	})

	t.Run("status", func(t *testing.T) {
		out, errOut, code := e.run(ana, nil, "status")
		t.Logf("metiche status\n%s%s", out, errOut)
		expect(t, code == 0 && strings.Contains(out, "e2e-alpha") && strings.Contains(out, "Cursor") && strings.Contains(out, "ana-laptop-cursor"), "status: %d", code)
		d := e.jsonRun(ana, nil, "status", "--json")
		expect(t, d["schema"] == "metiche.cli.status/1" && d["ok"] == true, "status --json schema: %v", d["schema"])
		cred := d["credential"].(map[string]any)
		expect(t, cred["source"] == "anchor_file" && cred["token_scope"] == "agent", "credential = %v", cred)
		teams := d["teams"].([]any)
		expect(t, len(teams) == 1 && teams[0].(map[string]any)["visibility"] == "private" && teams[0].(map[string]any)["board_url"] == nil, "teams = %v", teams)
	})

	t.Run("teams and teams create", func(t *testing.T) {
		out, _, code := e.run(ana, nil, "teams")
		t.Logf("metiche teams\n%s", out)
		expect(t, code == 0 && strings.Contains(out, "SLUG") && strings.Contains(out, "e2e-alpha"), "teams: %d", code)

		agentsBefore := e.sqlInt("SELECT COUNT(*) FROM agent")
		out, errOut, code := e.run(ana, nil, "teams", "create", "e2e  ALPHA")
		t.Logf("metiche teams create \"e2e  ALPHA\" -> exit %d\n%s%s", code, out, errOut)
		expect(t, code == 6 && strings.Contains(errOut, "already on a team named") && strings.Contains(errOut, "e2e-alpha"), "duplicate: exit %d", code)
		d := e.jsonRunCode(ana, nil, 6, "teams", "create", "E2E Alpha", "--json")
		expect(t, d["schema"] == "metiche.cli.error/1" && d["error"] == "already_exists", "duplicate --json = %v", d)

		out, errOut, code = e.run(ana, nil, "teams", "create", "E2E Beta")
		t.Logf("metiche teams create \"E2E Beta\" -> exit %d\n%s%s", code, redactCodes(out), errOut)
		expect(t, code == 0 && strings.Contains(out, "created team e2e-beta"), "create: exit %d", code)
		codeRe := regexp.MustCompile(`join code   ([0-9A-Z]{10})`)
		m := codeRe.FindStringSubmatch(out)
		expect(t, m != nil && strings.Count(out, m[1]) == 1, "the join code is shown exactly once")
		expect(t, e.sqlInt("SELECT COUNT(*) FROM agent") == agentsBefore, "teams create minted an agent")

		d = e.jsonRun(ana, nil, "teams", "create", "E2E Json", "--json")
		team := d["team"].(map[string]any)
		expect(t, d["schema"] == "metiche.cli.teams.create/1" && team["slug"] == "e2e-json" && len(d["join_code"].(string)) == 10 && d["created"] == true, "create --json = %v", team)

		out, _, code = e.run(ana, nil, "teams", "create", "E2E Quiet", "--quiet")
		expect(t, code == 0 && strings.TrimSpace(out) == "e2e-quiet", "--quiet printed %q", out)

		out, _, code = e.run(ana, nil, "teams", "create", "E2E Dry", "--dry-run")
		expect(t, code == 0 && strings.Contains(out, "dry run") && e.sqlInt("SELECT COUNT(*) FROM team WHERE slug='e2e-dry'") == 0, "dry run: %d %q", code, out)
		expect(t, e.sqlInt("SELECT COUNT(*) FROM agent") == agentsBefore, "agents changed")

		d = e.jsonRun(ana, nil, "teams", "--json")
		expect(t, d["schema"] == "metiche.cli.teams/1" && len(d["teams"].([]any)) == 4, "teams --json = %v", d["teams"])
	})

	t.Run("teams show", func(t *testing.T) {
		out, errOut, code := e.run(ana, nil, "teams", "show", "e2e-alpha")
		t.Logf("metiche teams show e2e-alpha\n%s%s", out, errOut)
		expect(t, code == 0 && strings.Contains(out, "Ana (owner, you)") && strings.Contains(out, "private"), "show: %d", code)
		d := e.jsonRun(ana, nil, "teams", "show", "e2e-alpha", "--json")
		expect(t, d["schema"] == "metiche.cli.teams.show/1" && d["board_url"] == nil && d["role"] == "owner", "show --json = %v", d)
		_, errOut, code = e.run(ana, nil, "teams", "show", "no-such-team")
		expect(t, code == 5, "show of a team you are not on: exit %d %s", code, errOut)
	})

	var bob string
	t.Run("invite create list revoke", func(t *testing.T) {
		out, errOut, code := e.run(ana, nil, "invite", "create", "--team", "e2e-alpha", "--label", "for Bob", "--max-uses", "2", "--expires", "48h")
		t.Logf("metiche invite create -> exit %d\n%s%s", code, redactCodes(out), errOut)
		m := regexp.MustCompile(`join code   ([0-9A-Z]{10})`).FindStringSubmatch(out)
		expect(t, code == 0 && m != nil && strings.Count(out, m[1]) == 1, "invite create: exit %d", code)
		joinCode := m[1]
		idm := regexp.MustCompile(`created invite ([0-9a-f]{8})`).FindStringSubmatch(out)
		expect(t, idm != nil, "no invite id")

		d := e.jsonRun(ana, nil, "invite", "create", "--team", "e2e-alpha", "--json")
		expect(t, d["schema"] == "metiche.cli.invite.create/1" && len(d["join_code"].(string)) == 10, "create --json = %v", d["schema"])
		code2 := d["join_code"].(string)

		out, _, code = e.run(ana, nil, "invite", "list", "--team", "e2e-alpha")
		t.Logf("metiche invite list --team e2e-alpha\n%s", out)
		expect(t, code == 0 && strings.Contains(out, idm[1]) && !strings.Contains(out, joinCode) && !strings.Contains(out, code2), "list shows a code or misses the invite")

		bob = e.home("bob")
		e.installer(bob, "install-bob", map[string]string{"METICHE_JOIN_CODE": joinCode, "METICHE_MEMBER_NAME": "Bob", "METICHE_MACHINE_ID": "bob-laptop"}, true)

		out, errOut, code = e.run(ana, nil, "invite", "revoke", idm[1], "--team", "e2e-alpha")
		t.Logf("metiche invite revoke %s -> exit %d\n%s%s", idm[1], code, out, errOut)
		expect(t, code == 0 && strings.Contains(out, "revoked invite "+idm[1]) && strings.Contains(out, "1 of 2 uses spent"), "revoke: %d", code)
		out, _, code = e.run(ana, nil, "invite", "revoke", idm[1], "--team", "e2e-alpha")
		expect(t, code == 0 && strings.Contains(out, "already revoked"), "revoke again: %d %q", code, out)

		d = e.jsonRun(ana, nil, "invite", "list", "--team", "e2e-alpha", "--all", "--json")
		raw, _ := json.Marshal(d)
		expect(t, strings.Contains(string(raw), `"state":"revoked"`) && !strings.Contains(string(raw), joinCode) && !strings.Contains(string(raw), code2), "list --all --json")

		carol := e.home("carol")
		e.installer(carol, "install-carol-after-revoke", map[string]string{"METICHE_JOIN_CODE": joinCode, "METICHE_MEMBER_NAME": "Test", "METICHE_MACHINE_ID": "test-laptop"}, false, "that join code is not valid")

		// A member revoking the owner's invite is refused (exit 5).
		_, errOut, code = e.run(bob, nil, "invite", "revoke", strings.Split(fmt.Sprint(d["invites"].([]any)[0].(map[string]any)["invite_id"]), "-")[0], "--team", "e2e-alpha")
		expect(t, code == 5, "a member revoking the owner's invite: exit %d %s", code, errOut)
	})

	t.Run("open", func(t *testing.T) {
		_ = os.RemoveAll(e.openLog)
		_ = os.MkdirAll(e.openLog, 0o700)
		out, errOut, code := e.run(ana, nil, "open", "e2e-alpha")
		t.Logf("metiche open e2e-alpha (private) -> exit %d\n%s%s", code, out, errOut)
		expect(t, code == 0 && strings.Contains(out, "is private") && !strings.Contains(out, "mbl_") && !strings.Contains(out, "signin"), "open printed a link without being asked")
		argv, _ := os.ReadFile(filepath.Join(e.openLog, "argv"))
		arg := strings.TrimSpace(string(argv))
		mode, _ := os.ReadFile(filepath.Join(e.openLog, "mode"))
		copyOf, _ := os.ReadFile(filepath.Join(e.openLog, "file"))
		expect(t, strings.HasSuffix(arg, ".html") && !strings.Contains(arg, "mbl_"), "the opener got %q, want a file path", arg)
		expect(t, strings.TrimSpace(string(mode)) == "600", "the sign-in file mode was %q", mode)
		expect(t, strings.Contains(string(copyOf), "/signin#mbl_"), "the sign-in file holds no link")
		_, statErr := os.Stat(filepath.Dir(arg))
		expect(t, os.IsNotExist(statErr), "the sign-in directory still exists after exit")

		out, _, code = e.run(ana, nil, "open", "e2e-alpha", "--print")
		e.markSignin(len(e.outputs) - 1)
		expect(t, code == 0 && strings.Contains(out, e.boardBase+"/signin#mbl_") && strings.Contains(out, "works once"), "open --print: %d", code)
		d := e.jsonRun(ana, nil, "open", "e2e-alpha", "--json")
		e.markSignin(len(e.outputs) - 1)
		expect(t, d["schema"] == "metiche.cli.open/1" && d["visibility"] == "private" && strings.Contains(fmt.Sprint(d["login_url"]), "#mbl_"), "open --json = %v", d["visibility"])

		e.sql("UPDATE team SET visibility = 2 WHERE slug = 'e2e-alpha'")
		out, _, code = e.run(ana, nil, "open", "e2e-alpha", "--print")
		t.Logf("metiche open e2e-alpha --print (public) -> exit %d\n%s", code, out)
		expect(t, code == 0 && strings.TrimSpace(out) == e.boardBase+"/t/e2e-alpha", "public --print = %q", out)
		_ = os.RemoveAll(e.openLog)
		_ = os.MkdirAll(e.openLog, 0o700)
		out, _, code = e.run(ana, nil, "open", "e2e-alpha")
		argv, _ = os.ReadFile(filepath.Join(e.openLog, "argv"))
		expect(t, code == 0 && strings.TrimSpace(string(argv)) == e.boardBase+"/t/e2e-alpha", "public open gave the opener %q", argv)
		e.sql("UPDATE team SET visibility = 1 WHERE slug = 'e2e-alpha'")
	})

	t.Run("init non-interactive", func(t *testing.T) {
		repo := e.gitRepo("demo", "https://github.com/example/demo.git")
		out, errOut, code := e.runIn(ana, repo, nil, "init")
		t.Logf("metiche init (no TTY, 4 teams) -> exit %d\n%s%s", code, out, errOut)
		expect(t, code == 2 && strings.Contains(errOut, "--team <slug>") && strings.Contains(errOut, "e2e-alpha"), "no-TTY refusal: exit %d", code)
		expect(t, !exists(filepath.Join(repo, ".metiche")), "the refusal wrote a file")
		d := e.jsonRunIn(ana, repo, nil, 2, "init", "--json")
		expect(t, d["error"] == "ambiguous_team" && len(d["candidates"].([]any)) == 4, "ambiguous --json = %v", d)

		out, errOut, code = e.runIn(ana, repo, nil, "init", "--team", "e2e-alpha")
		t.Logf("metiche init --team e2e-alpha -> exit %d\n%s%s", code, out, errOut)
		file := filepath.Join(repo, ".metiche")
		body, _ := os.ReadFile(file)
		expect(t, code == 0 && strings.HasSuffix(string(body), "team = e2e-alpha\nproject = demo\n"), "wrote %q", body)
		st1, _ := os.Stat(file)
		expect(t, st1.Mode().Perm() == 0o644, "mode %v", st1.Mode())

		time.Sleep(1100 * time.Millisecond)
		out, _, code = e.runIn(ana, filepath.Join(repo, "sub"), nil, "init", "--team", "e2e-alpha")
		st2, _ := os.Stat(file)
		expect(t, code == 0 && strings.Contains(out, "already bound") && st2.ModTime().Equal(st1.ModTime()), "re-run from a subdirectory: %d %q", code, out)
		expect(t, !exists(filepath.Join(repo, "sub", ".metiche")), "a second .metiche was written below the root")

		out, errOut, code = e.runIn(ana, repo, nil, "init", "--team", "e2e-beta")
		t.Logf("metiche init --team e2e-beta (different binding) -> exit %d\n%s%s", code, out, errOut)
		after, _ := os.ReadFile(file)
		expect(t, code == 6 && strings.Contains(out, "- team = e2e-alpha") && bytes.Equal(after, body), "different binding: exit %d", code)

		d = e.jsonRunIn(ana, repo, nil, 0, "init", "--team", "e2e-beta", "--force", "--dry-run", "--json")
		after, _ = os.ReadFile(file)
		expect(t, d["schema"] == "metiche.cli.init/1" && d["action"] == "would_update" && d["dry_run"] == true && bytes.Equal(after, body), "dry-run --json = %v", d["action"])

		const canary = "CANARYremoteSecret0123"
		crepo := e.gitRepo("canary", "https://someone:"+canary+"@github.com/example/canary.git")
		cout, cerr, ccode := e.runIn(ana, crepo, nil, "init", "--team", "e2e-alpha", "--json")
		cbody, _ := os.ReadFile(filepath.Join(crepo, ".metiche"))
		expect(t, ccode == 0 && !strings.Contains(cout+cerr+string(cbody), canary) && strings.Contains(string(cbody), "project = canary"), "canary remote: exit %d", ccode)
		for _, w := range []string{"://", "@", "mtk_"} {
			expect(t, !strings.Contains(string(cbody), w), ".metiche contains %q", w)
		}
	})

	t.Run("init interactive", func(t *testing.T) {
		repo := e.gitRepo("two", "git@github.com:example/two.git")
		p := e.pty(ana, repo, "init")
		menu := p.expect("team [1-")
		p.send("")
		p.expect("team [1-") // Enter alone asks again: no default
		p.send(menuNumber(t, p.text(), "e2e-alpha"))
		p.expect("project [two]: ")
		p.send("")
		p.expect("? [y/N]: ")
		p.send("n")
		p.expect("Nothing was written.")
		code := p.wait()
		t.Logf("pty transcript (existing team, declined):\n%s", p.text())
		expect(t, code == 0 && !exists(filepath.Join(repo, ".metiche")), "declined init wrote a file (exit %d)", code)
		_ = menu

		p = e.pty(ana, repo, "init")
		p.expect("team [1-")
		p.send(menuNumber(t, p.text(), "e2e-alpha"))
		p.expect("project [two]: ")
		p.send("")
		p.expect("? [y/N]: ")
		p.send("y")
		p.expect("Show the git command")
		p.send("y")
		p.expect("git add .metiche")
		code = p.wait()
		t.Logf("pty transcript (existing team, confirmed):\n%s", p.text())
		body, _ := os.ReadFile(filepath.Join(repo, ".metiche"))
		expect(t, code == 0 && strings.HasSuffix(string(body), "team = e2e-alpha\nproject = two\n"), "confirmed init wrote %q (exit %d)", body, code)

		repo3 := e.gitRepo("three", "https://github.com/example/three.git")
		p = e.pty(ana, repo3, "init")
		p.expect("team [1-")
		p.send(menuNumber(t, p.text(), "create a new team"))
		p.expect("team name: ")
		p.send("E2E Alpha")
		p.expect("Use e2e-alpha instead? [y/N]: ")
		p.send("n")
		p.expectAfter("create a new team", "team [1-")
		p.send(menuNumber(t, p.lastMenu(), "create a new team"))
		p.expect("team name: ")
		p.send("E2E Gamma")
		p.expect("project [three]: ")
		p.send("")
		p.expect("? [y/N]: ")
		p.send("yes")
		p.expect("Show the git command")
		p.send("n")
		code = p.wait()
		transcript := p.text()
		t.Logf("pty transcript (create a team):\n%s", redactCodes(transcript))
		body, _ = os.ReadFile(filepath.Join(repo3, ".metiche"))
		m := regexp.MustCompile(`join code   ([0-9A-Z]{10})`).FindStringSubmatch(transcript)
		expect(t, code == 0 && m != nil && strings.Count(transcript, m[1]) == 1, "create path: exit %d, code shown once", code)
		expect(t, strings.HasSuffix(string(body), "team = e2e-gamma\nproject = three\n"), "create path wrote %q", body)
		expect(t, e.sqlInt("SELECT COUNT(*) FROM team WHERE name = 'E2E Alpha'") == 1, "the duplicate name was created")
	})

	t.Run("doctor", func(t *testing.T) {
		out, errOut, code := e.run(ana, nil, "doctor", "--client", "cursor")
		t.Logf("metiche doctor --client cursor (healthy) -> exit %d\n%s%s", code, out, errOut)
		expect(t, code == 0 && strings.Contains(out, "✔ anchor.token") && strings.Contains(out, "✔ token") && strings.Contains(out, "summary: 0 errors · 0 warnings"), "healthy doctor: exit %d", code)

		stale := "mtk_" + strings.Repeat("S", 43)
		backup := filepath.Join(ana, ".metiche", "env.metiche-backup-20260101-000000")
		mustWrite(t, backup, "METICHE_TOKEN="+stale+"\n", 0o600)
		d := e.jsonRunIn(ana, "", map[string]string{"METICHE_TOKEN": stale}, 0, "doctor", "--client", "cursor", "--json")
		c := findCheck(d, "machine.env.shell")
		expect(t, c != nil && c["status"] == "warn" && strings.Contains(fmt.Sprint(c["summary"]), "stale"), "stale METICHE_TOKEN check = %v", c)
		out, errOut, code = e.run(ana, map[string]string{"METICHE_TOKEN": stale}, "status", "--no-clients")
		expect(t, code == 0 && strings.Contains(errOut, "stale token from before the last install"), "status with a stale shell token: %d %s", code, errOut)
		t.Logf("metiche status --no-clients with a stale METICHE_TOKEN -> exit %d; stderr: %s", code, errOut)

		dead := "mtk_" + strings.Repeat("D", 43)
		d = e.jsonRunIn(ana, "", map[string]string{"METICHE_TOKEN": dead}, 1, "doctor", "--client", "cursor", "--json")
		c = findCheck(d, "machine.env.shell")
		expect(t, c != nil && c["status"] == "error" && strings.Contains(fmt.Sprint(c["summary"]), "rejects"), "dead shell token check = %v", c)

		deadHome := e.home("dead")
		copyTree(t, ana, deadHome)
		e.machine[deadHome] = e.machine[ana]
		mustWrite(t, filepath.Join(deadHome, ".metiche", "env"), "# >>> metiche >>> managed by install.sh\nMETICHE_TOKEN="+dead+"\nexport METICHE_TOKEN\n# <<< metiche <<<\n", 0o600)
		out, errOut, code = e.run(deadHome, nil, "doctor", "--client", "cursor")
		t.Logf("metiche doctor with a dead anchor -> exit %d\n%s%s", code, out, errOut)
		expect(t, code == 1 && strings.Contains(out, "✘ anchor.token") && strings.Contains(out, "rejects the token"), "dead anchor: exit %d", code)
		out, errOut, code = e.run(deadHome, nil, "status", "--no-clients")
		expect(t, code == 0 && strings.Contains(errOut, "credential: Cursor's token (the anchor was rejected"), "fallback: %d %s", code, errOut)
	})

	t.Run("exit codes", func(t *testing.T) {
		empty := e.home("empty")
		_, errOut, code := e.run(empty, map[string]string{"METICHE_TOKEN": "mtk_" + strings.Repeat("U", 43)}, "status")
		expect(t, code == 3 && strings.Contains(errOut, "no usable metiche token"), "unknown token: exit %d %s", code, errOut)
		_, errOut, code = e.run(ana, map[string]string{"METICHE_MCP_URL": strings.TrimSuffix(e.mcpURL, "/v1/mcp") + "/mcp"}, "status")
		expect(t, code == 4 && strings.Contains(errOut, "/v1/mcp"), "wrong path: exit %d %s", code, errOut)
		_, _, code = e.run(ana, nil, "teams", "rename", "x")
		expect(t, code == 2, "teams rename: exit %d", code)
	})

	t.Run("no token in any output", func(t *testing.T) {
		secrets := []string{anaCursor, e.cursorToken(bob)}
		for _, h := range []string{ana, bob} {
			if raw, err := os.ReadFile(filepath.Join(h, ".metiche", "env")); err == nil {
				if m := regexp.MustCompile(`METICHE_TOKEN=(\S+)`).FindSubmatch(raw); m != nil {
					secrets = append(secrets, string(m[1]))
				}
			}
		}
		n, signin := 0, 0
		for _, o := range e.outputs {
			n++
			if m := tokenShaped.FindString(o.text); m != "" {
				t.Errorf("%s printed a token-shaped string", o.label)
			}
			for _, s := range secrets {
				if s != "" && strings.Contains(o.text, s) {
					t.Errorf("%s printed a real token", o.label)
				}
			}
			if strings.Contains(o.text, "mbl_") {
				if !o.signinAsked {
					t.Errorf("%s printed a sign-in link without --print or --json", o.label)
				}
				signin++
			}
		}
		t.Logf("scanned %d CLI outputs (stdout+stderr, pty transcripts included): 0 token-shaped strings, 0 real tokens; sign-in links only in the %d outputs that asked for one", n, signin)
	})
}

// ── harness ─────────────────────────────────────────────────────────────────

func expect(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Errorf(format, args...)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func mustWrite(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(p, mode)
}

var codeLike = regexp.MustCompile(`\b[0-9A-HJKMNP-TV-Z]{10}\b`)

// redactCodes keeps join codes out of the test log.
func redactCodes(s string) string { return codeLike.ReplaceAllString(s, "<join-code>") }

func setupE2E(t *testing.T) *e2e {
	container := os.Getenv("METICHE_CLI_E2E_DB_CONTAINER")
	port := os.Getenv("METICHE_CLI_E2E_DB_PORT")
	passFile := os.Getenv("METICHE_CLI_E2E_DB_PASSWORD_FILE")
	if container == "" || port == "" || passFile == "" {
		t.Fatal("METICHE_CLI_E2E_DB_CONTAINER, METICHE_CLI_E2E_DB_PORT and METICHE_CLI_E2E_DB_PASSWORD_FILE are required")
	}
	pass, err := os.ReadFile(passFile)
	if err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	e := &e2e{t: t, repo: filepath.Clean(filepath.Join(cwd, "..", "..")), container: container, db: "metiche_e2e"}
	e.root = t.TempDir()
	_ = os.Chmod(e.root, 0o700)

	// A fresh database: schema and the default plan, as the smoke script does.
	e.dockerSQL("", "DROP DATABASE IF EXISTS "+e.db+"; CREATE DATABASE "+e.db+" CHARACTER SET utf8mb4;")
	for _, f := range []string{"code/backend/metiche/core/repository/sql/schema/create.sql", "deploy/sql/seed-default-plan.sql"} {
		b, err := os.ReadFile(filepath.Join(e.repo, f))
		if err != nil {
			t.Fatal(err)
		}
		e.dockerSQL(e.db, string(b))
	}

	bins := filepath.Join(e.root, "bin")
	_ = os.MkdirAll(bins, 0o700)
	build := func(dir, out string, args ...string) {
		c := exec.Command("go", append([]string{"build", "-o", out}, args...)...)
		c.Dir = dir
		if b, err := c.CombinedOutput(); err != nil {
			t.Fatalf("go build in %s: %v\n%s", dir, err, b)
		}
	}
	build(filepath.Join(e.repo, "code", "backend", "metiche"), filepath.Join(bins, "backend"), ".")
	e.bin = filepath.Join(bins, "metiche")
	build(filepath.Join(e.repo, "code", "cli"), e.bin, "-ldflags", "-X github.com/mklfarha/metiche/cli/internal/buildinfo.Version=e2e", ".")

	httpPort := freePort(t)
	e.mcpURL = fmt.Sprintf("http://127.0.0.1:%d/v1/mcp", httpPort)
	e.boardBase = "http://localhost:8799"
	cfg := filepath.Join(e.root, "backend.yaml")
	mustWrite(t, cfg, fmt.Sprintf("ports:\n  http: %q\ndb:\n  - name: "+e.db+"\n    host: 127.0.0.1\n    port: %q\n    user: root\n    pswd: %q\n    params: \"parseTime=true&interpolateParams=true&charset=utf8mb4&loc=UTC\"\n    driver: \"mysql\"\nmonitoring:\n  enabled: false\n",
		strconv.Itoa(httpPort), port, strings.TrimSpace(string(pass))), 0o600)
	logf, _ := os.Create(filepath.Join(e.root, "backend.log"))
	backend := exec.Command(filepath.Join(bins, "backend"))
	backend.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + e.root, "CONFIG=" + filepath.Join(e.repo, "code/backend/metiche/config") + "," + cfg,
		"METICHE_ROLE=all", "METICHE_BOARD_BASE_URL=" + e.boardBase, "METICHE_CREATE_TEAM_PER_HOUR=1000", "METICHE_JOIN_PER_HOUR=1000"}
	backend.Stdout, backend.Stderr = logf, logf
	if err := backend.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Process.Kill(); _, _ = backend.Process.Wait() })
	deadline := time.Now().Add(60 * time.Second)
	for {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", httpPort), 200*time.Millisecond); err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(filepath.Join(e.root, "backend.log"))
			t.Fatalf("the backend did not start:\n%s", tail(string(log), 20))
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("backend on %s (database %s), CLI %s", e.mcpURL, e.db, e.bin)

	e.fakebin = filepath.Join(e.root, "fakebin")
	e.openLog = filepath.Join(e.root, "openlog")
	_ = os.MkdirAll(e.fakebin, 0o700)
	_ = os.MkdirAll(e.openLog, 0o700)
	mustWrite(t, filepath.Join(e.fakebin, "open"), "#!/bin/sh\nd=\""+e.openLog+"\"\nprintf '%s\\n' \"$@\" > \"$d/argv\"\nfor a in \"$@\"; do if [ -f \"$a\" ]; then stat -f '%Lp' \"$a\" > \"$d/mode\"; cp \"$a\" \"$d/file\"; fi; done\nexit 0\n", 0o700)
	return e
}

func mustRead(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func tail(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return tokenShaped.ReplaceAllString(strings.Join(lines, "\n"), "mtk_<redacted>")
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func (e *e2e) dockerSQL(db, query string) string {
	args := []string{"exec", "-i", e.container, "mysql", "-h127.0.0.1", "-N", "-B"}
	if db != "" {
		args = append(args, db)
	}
	c := exec.Command("docker", args...)
	c.Stdin = strings.NewReader(query)
	out, err := c.CombinedOutput()
	if err != nil {
		e.t.Fatalf("SQL failed: %v\n%s", err, out)
	}
	return string(out)
}

func (e *e2e) sql(q string) string { return e.dockerSQL(e.db, q) }

func (e *e2e) sqlInt(q string) int {
	n, err := strconv.Atoi(strings.TrimSpace(e.sql(q)))
	if err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (e *e2e) home(name string) string {
	h := filepath.Join(e.root, "home-"+name)
	for _, d := range []string{".cursor", "tmp"} {
		_ = os.MkdirAll(filepath.Join(h, d), 0o700)
	}
	return h
}

func (e *e2e) installer(home, label string, env map[string]string, wantOK bool, mustSay ...string) {
	e.t.Helper()
	if e.machine == nil {
		e.machine = map[string]string{}
	}
	e.machine[home] = env["METICHE_MACHINE_ID"]
	jq, _ := exec.LookPath("jq")
	path := e.fakebin + ":" + filepath.Dir(jq) + ":/usr/bin:/bin"
	c := exec.Command("/bin/sh", filepath.Join(e.repo, "install.sh"), "--url", e.mcpURL, "--only", "cursor", "--no-open", "--no-cli", "--no-write-profile")
	c.Env = []string{"HOME=" + home, "PATH=" + path, "TMPDIR=" + filepath.Join(home, "tmp"), "LANG=C", "TERM=dumb", "SHELL=/bin/sh"}
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v)
	}
	c.Stdin = nil
	out, err := c.CombinedOutput()
	ok := err == nil
	if ok != wantOK {
		e.t.Fatalf("installer %s: ok=%v want %v\n%s", label, ok, wantOK, tail(redactCodes(string(out)), 25))
	}
	for _, want := range mustSay {
		if !strings.Contains(string(out), want) {
			e.t.Fatalf("installer %s did not say %q:\n%s", label, want, tail(redactCodes(string(out)), 25))
		}
	}
	e.t.Logf("installer %s: exit ok=%v (as expected); last lines:\n%s", label, ok, tail(redactCodes(string(out)), 4))
}

func (e *e2e) cursorToken(home string) string {
	raw, err := os.ReadFile(filepath.Join(home, ".cursor", "mcp.json"))
	if err != nil {
		return ""
	}
	var d struct {
		MCPServers map[string]struct {
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	_ = json.Unmarshal(raw, &d)
	return strings.TrimPrefix(d.MCPServers["metiche"].Headers["Authorization"], "Bearer ")
}

func (e *e2e) cliEnv(home string, extra map[string]string) []string {
	env := []string{"HOME=" + home, "PATH=" + e.fakebin + ":/usr/bin:/bin:/opt/homebrew/bin", "TMPDIR=" + filepath.Join(home, "tmp"),
		"METICHE_MCP_URL=" + e.mcpURL, "METICHE_BOARD_URL=" + e.boardBase, "METICHE_CLI_STRICT_DECODE=1", "METICHE_CLI_SIGNIN_FILE_SECONDS=1", "LANG=C"}
	if id := e.machine[home]; id != "" {
		env = append(env, "METICHE_MACHINE_ID="+id)
	} else {
		env = append(env, "METICHE_MACHINE_ID=test-box")
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

func (e *e2e) run(home string, extra map[string]string, args ...string) (string, string, int) {
	return e.runIn(home, "", extra, args...)
}

func (e *e2e) runIn(home, dir string, extra map[string]string, args ...string) (string, string, int) {
	if dir == "" {
		dir = home
	}
	_ = os.MkdirAll(dir, 0o755)
	c := exec.Command(e.bin, args...)
	c.Dir = dir
	c.Env = e.cliEnv(home, extra)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatalf("running metiche %v: %v", args, err)
	}
	e.record("metiche "+strings.Join(args, " "), out.String()+errb.String())
	return out.String(), errb.String(), code
}

func (e *e2e) record(label, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.outputs = append(e.outputs, recorded{label: label, text: text})
}

func (e *e2e) markSignin(i int) { e.outputs[i].signinAsked = true }

func (e *e2e) jsonRun(home string, extra map[string]string, args ...string) map[string]any {
	return e.jsonRunIn(home, "", extra, 0, args...)
}

func (e *e2e) jsonRunCode(home string, extra map[string]string, want int, args ...string) map[string]any {
	return e.jsonRunIn(home, "", extra, want, args...)
}

func (e *e2e) jsonRunIn(home, dir string, extra map[string]string, want int, args ...string) map[string]any {
	e.t.Helper()
	out, errOut, code := e.runIn(home, dir, extra, args...)
	var d map[string]any
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&d); err != nil {
		e.t.Fatalf("metiche %v: stdout is not JSON (exit %d): %q %s", args, code, out, errOut)
	}
	if _, err := dec.Token(); err != io.EOF {
		e.t.Errorf("metiche %v: stdout holds more than one JSON document", args)
	}
	if code != want {
		e.t.Errorf("metiche %v: exit %d, want %d (%s)", args, code, want, errOut)
	}
	schema, _ := d["schema"].(string)
	if !strings.HasPrefix(schema, "metiche.cli.") || !strings.HasSuffix(schema, "/1") {
		e.t.Errorf("metiche %v: schema %q", args, schema)
	}
	if _, ok := d["ok"].(bool); !ok {
		e.t.Errorf("metiche %v: no boolean ok", args)
	}
	return d
}

func findCheck(d map[string]any, id string) map[string]any {
	checks, _ := d["checks"].([]any)
	for _, c := range checks {
		m := c.(map[string]any)
		if m["id"] == id {
			return m
		}
	}
	return nil
}

func (e *e2e) gitRepo(name, remote string) string {
	dir := filepath.Join(e.root, "repos", name)
	_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			e.t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	real, _ := filepath.EvalSymlinks(dir)
	return real
}

func copyTree(t *testing.T, from, to string) {
	c := exec.Command("cp", "-Rp", from+"/.", to)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("cp: %v %s", err, out)
	}
}

// ── pty ─────────────────────────────────────────────────────────────────────

type ptyRun struct {
	t    *testing.T
	e    *e2e
	cmd  *exec.Cmd
	f    *os.File
	mu   sync.Mutex
	buf  bytes.Buffer
	done chan struct{}
	pos  int
}

func (e *e2e) pty(home, dir string, args ...string) *ptyRun {
	e.t.Helper()
	c := exec.Command(e.bin, args...)
	c.Dir = dir
	c.Env = append(e.cliEnv(home, nil), "TERM=dumb")
	f, err := pty.Start(c)
	if err != nil {
		e.t.Fatal(err)
	}
	p := &ptyRun{t: e.t, e: e, cmd: c, f: f, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		b := make([]byte, 4096)
		for {
			n, err := f.Read(b)
			if n > 0 {
				p.mu.Lock()
				p.buf.Write(b[:n])
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return p
}

func (p *ptyRun) text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.ReplaceAll(p.buf.String(), "\r\n", "\n")
}

// expect waits for s to appear after the last match.
func (p *ptyRun) expect(s string) string {
	p.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		txt := p.text()
		if i := strings.Index(txt[min(p.pos, len(txt)):], s); i >= 0 {
			p.pos = p.pos + i + len(s)
			return txt
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.t.Fatalf("pty: %q never appeared; transcript:\n%s", s, redactCodes(p.text()))
	return ""
}

// expectAfter waits for a then b, in order.
func (p *ptyRun) expectAfter(a, b string) {
	p.t.Helper()
	p.expect(a)
	p.expect(b)
}

func (p *ptyRun) lastMenu() string {
	txt := p.text()
	if i := strings.LastIndex(txt, "Which one does this repository belong to?"); i >= 0 {
		return txt[i:]
	}
	return txt
}

func (p *ptyRun) send(line string) {
	time.Sleep(50 * time.Millisecond)
	_, _ = p.f.Write([]byte(line + "\n"))
}

func (p *ptyRun) wait() int {
	err := p.cmd.Wait()
	<-p.done
	_ = p.f.Close()
	p.e.record("pty metiche "+strings.Join(p.cmd.Args[1:], " "), p.text())
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 0
}

// menuNumber finds the number in front of an entry in the LAST menu printed.
func menuNumber(t *testing.T, text, entry string) string {
	t.Helper()
	if i := strings.LastIndex(text, "Which one does this repository belong to?"); i >= 0 {
		text = text[i:]
	}
	m := regexp.MustCompile(`(\d+)\)\s+`+regexp.QuoteMeta(entry)+`(\s|$)`).FindAllStringSubmatch(text, -1)
	if len(m) == 0 {
		t.Fatalf("no menu entry %q in:\n%s", entry, text)
	}
	return m[len(m)-1][1]
}
