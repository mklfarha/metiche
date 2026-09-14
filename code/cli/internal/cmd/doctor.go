package cmd

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mklfarha/metiche/cli/internal/binding"
	"github.com/mklfarha/metiche/cli/internal/config"
	"github.com/mklfarha/metiche/cli/internal/credential"
	"github.com/mklfarha/metiche/cli/internal/gitx"
	"github.com/mklfarha/metiche/cli/internal/mcpclient"
	"github.com/mklfarha/metiche/cli/internal/secret"
	"github.com/mklfarha/metiche/cli/internal/wire"
)

// metiche doctor (docs/CLI.md §2). Every check says how it knows: through
// (the client did it), server (the metiche server saw it) or around (the CLI
// checked a file or a token on its own). An around check that passes is never
// reported as "the client works". Doctor never modifies anything.

type check struct {
	ID          string            `json:"id"`
	Client      string            `json:"client"`
	Mode        string            `json:"mode"`
	Status      string            `json:"status"` // ok | warn | error | skip | info
	Summary     string            `json:"summary"`
	Evidence    map[string]string `json:"evidence,omitempty"`
	Remediation string            `json:"remediation,omitempty"`
}

type doctor struct {
	a       *app
	checks  []check
	health  *wire.Health
	healthE error
	offline bool
	machine string
	teams   *wire.ListTeams
}

const installCmd = "curl -fsSL https://metiche.xyz/install.sh | sh"

func (d *doctor) add(c check) {
	c.Summary = secret.Default.Redact(c.Summary)
	c.Remediation = secret.Default.Redact(c.Remediation)
	d.checks = append(d.checks, c)
}

func (a *app) cmdDoctor(args []string) error {
	fs := a.flags("doctor")
	clientsFlag := fs.String("client", "", "comma-separated subset of claude,cursor,windsurf,codex")
	offline := fs.Bool("offline", false, "file checks only, no network")
	fs.Bool("no-exec", false, "run no client CLI (this version runs none)")
	prove := fs.String("prove", "", "interactive delivery proof for a GUI client")
	strict := fs.Bool("strict", false, "warnings also exit 1")
	verbose := fs.Bool("verbose", false, "show evidence")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	if *prove != "" {
		return fail(exitUsage, "not_built", "--prove is not built yet (docs/CLI.md §2.6)")
	}
	clients := credential.Clients
	if *clientsFlag != "" {
		clients = nil
		for _, c := range strings.Split(*clientsFlag, ",") {
			c = strings.TrimSpace(c)
			if _, ok := credential.Titles[c]; !ok {
				return fail(exitUsage, "usage", "--client: unknown client %q (claude, cursor, windsurf, codex)", c)
			}
			clients = append(clients, c)
		}
	}
	d := &doctor{a: a, offline: *offline, machine: config.MachineID(a.env, a.hostname)}

	d.machineChecks()
	tokens := map[string]wire.Whoami{} // source -> whoami, accepted tokens only
	anchor := d.anchorChecks(tokens)
	d.shellChecks(anchor)
	for _, c := range clients {
		d.clientChecks(c, tokens)
	}
	d.identityChecks(tokens)
	d.bindingChecks(tokens)
	d.add(check{ID: "doctor.coverage", Client: "machine", Mode: "around", Status: "info",
		Summary: "not checked by this version: client probes (claude mcp get, cursor-agent), server delivery evidence per client, Codex's log, the Claude Code plugin cache, Cursor's IDE log, Codex AGENTS.md, --prove"})

	sum := map[string]int{}
	for _, c := range d.checks {
		sum[c.Status]++
	}
	exit := exitOK
	if sum["error"] > 0 || (*strict && sum["warn"] > 0) {
		exit = exitError
	}
	if a.json {
		doc := doc("doctor")
		doc["ok"] = exit == exitOK
		doc["endpoint"], doc["machine_id"] = a.endpoint.URL, d.machine
		doc["checks"] = d.checks
		doc["summary"] = map[string]int{"errors": sum["error"], "warnings": sum["warn"], "ok": sum["ok"], "skipped": sum["skip"], "info": sum["info"]}
		doc["exit"] = exit
		a.writeJSON(doc)
	} else {
		d.render(*verbose)
		a.out("")
		a.out("summary: %d errors · %d warnings · %d ok · %d skipped   → exit %d", sum["error"], sum["warn"], sum["ok"], sum["skip"], exit)
	}
	if exit != exitOK {
		return &cliError{silent: true, exit: exit, code: "doctor_findings", detail: fmt.Sprintf("doctor found %d errors and %d warnings", sum["error"], sum["warn"])}
	}
	return nil
}

func (d *doctor) render(verbose bool) {
	a := d.a
	a.out("metiche doctor · endpoint %s · machine %s", a.endpoint.URL, d.machine)
	group := ""
	for _, c := range d.checks {
		if c.Client != group {
			group = c.Client
			title := group
			if t, ok := credential.Titles[group]; ok {
				title = t
			}
			a.out("")
			a.out("%s", title)
		}
		sym := map[string]string{"ok": "✔", "warn": "!", "error": "✘", "skip": "-", "info": "·"}[c.Status]
		name := c.ID
		if i := strings.LastIndex(name, "."); i >= 0 && strings.Count(name, ".") == 1 {
			name = name[i+1:]
		} else if i := strings.Index(name, "."); i >= 0 {
			name = name[i+1:]
		}
		a.out("  %s %-18s %s   %s", sym, name, c.Summary, c.Mode)
		if c.Remediation != "" {
			a.out("                       → %s", c.Remediation)
		}
		if verbose {
			keys := make([]string, 0, len(c.Evidence))
			for k := range c.Evidence {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				a.out("                       %s: %s", k, c.Evidence[k])
			}
		}
	}
}

// probeHealth caches the anonymous health call.
func (d *doctor) probeHealth() (*wire.Health, error) {
	if d.health == nil && d.healthE == nil {
		h, err := d.a.health()
		if err != nil {
			d.healthE = err
		} else {
			d.health = &h
		}
	}
	return d.health, d.healthE
}

func (d *doctor) serverUp() bool {
	if d.offline {
		return false
	}
	h, err := d.probeHealth()
	return err == nil && h.OK && h.Database == "reachable"
}

// tokenVerdict classifies a whoami with one token.
type tokenVerdict struct {
	who     wire.Whoami
	ok      bool
	dead    bool // 401 with the database reachable
	problem string
}

func (d *doctor) whoami(tok secret.Secret) tokenVerdict {
	c, w, err := d.a.whoamiWith(tok)
	if err == nil {
		c.Close()
		return tokenVerdict{who: w, ok: true}
	}
	var me *mcpclient.Error
	if errors.As(err, &me) && me.Kind == mcpclient.KindUnauthorized {
		if d.serverUp() {
			return tokenVerdict{dead: true, problem: "the server rejects it (HTTP 401)"}
		}
		return tokenVerdict{problem: "401, with the database down: not a dead token"}
	}
	return tokenVerdict{problem: secret.Default.Redact(err.Error())}
}

func (d *doctor) machineChecks() {
	a := d.a
	u, _ := url.Parse(a.endpoint.URL)
	if u != nil && u.Path != "/v1/mcp" {
		d.add(check{ID: "machine.endpoint.url", Client: "machine", Mode: "around", Status: "warn",
			Summary:     fmt.Sprintf("the endpoint path is %s", u.Path),
			Remediation: "metiche serves MCP at /v1/mcp (e.g. https://mcp.metiche.xyz/v1/mcp). Fix or unset METICHE_MCP_URL."})
	} else {
		d.add(check{ID: "machine.endpoint.url", Client: "machine", Mode: "around", Status: "ok", Summary: a.endpoint.URL + " (" + a.endpoint.Source + ")"})
	}
	if d.offline {
		d.add(check{ID: "machine.endpoint", Client: "machine", Mode: "server", Status: "skip", Summary: "--offline"})
		return
	}
	h, err := d.probeHealth()
	switch {
	case err != nil:
		var me *mcpclient.Error
		sum := err.Error()
		if errors.As(err, &me) && me.HTTPStatus == 404 {
			sum = "nothing answers MCP at " + a.endpoint.URL + " (HTTP 404). The path is /v1/mcp."
		}
		d.add(check{ID: "machine.endpoint", Client: "machine", Mode: "server", Status: "error", Summary: sum,
			Remediation: "Nothing else can be checked against the server until this passes."})
	case !h.OK || h.Database != "reachable":
		d.add(check{ID: "machine.endpoint", Client: "machine", Mode: "server", Status: "error",
			Summary: "metiche is up but its database is not", Remediation: "Retry in a few minutes; your configuration may be fine."})
	default:
		d.add(check{ID: "machine.endpoint", Client: "machine", Mode: "server", Status: "ok",
			Summary: fmt.Sprintf("/v1/mcp answers · server %s · database reachable", h.ProtocolVersion)})
	}
}

func perm(m os.FileMode) string { return fmt.Sprintf("%04o", m) }

func (d *doctor) anchorChecks(tokens map[string]wire.Whoami) credential.AnchorFile {
	a := d.a
	af := credential.ReadAnchorFile(a.paths)
	path := a.paths.Tilde(af.Path)
	switch {
	case !af.Exists:
		d.add(check{ID: "machine.anchor.file", Client: "machine", Mode: "around", Status: "warn", Summary: "no " + path,
			Remediation: "Run the installer: " + installCmd})
		return af
	case af.Mode&0o077 != 0:
		d.add(check{ID: "machine.anchor.file", Client: "machine", Mode: "around", Status: "error",
			Summary: fmt.Sprintf("%s is mode %s and holds a token", path, perm(af.Mode)), Remediation: "Run: chmod 600 " + path})
	case af.DirMode&0o077 != 0:
		d.add(check{ID: "machine.anchor.file", Client: "machine", Mode: "around", Status: "error",
			Summary: fmt.Sprintf("%s is mode %s", a.paths.Tilde(a.paths.MeticheDir), perm(af.DirMode)), Remediation: "Run: chmod 700 " + a.paths.Tilde(a.paths.MeticheDir)})
	case af.Token.Empty():
		d.add(check{ID: "machine.anchor.file", Client: "machine", Mode: "around", Status: "warn",
			Summary: path + " has no METICHE_TOKEN line of token shape", Remediation: "Re-run the installer: " + installCmd})
	case af.ClientKey:
		d.add(check{ID: "machine.anchor.file", Client: "machine", Mode: "around", Status: "warn",
			Summary:     path + " still exports METICHE_CLIENT_KEY (pre-per-agent install)",
			Remediation: "Re-run the installer; it rewrites this file without it."})
	default:
		d.add(check{ID: "machine.anchor.file", Client: "machine", Mode: "around", Status: "ok", Summary: path + " · mode 0600 · one METICHE_TOKEN"})
	}
	if af.Token.Empty() {
		return af
	}
	if d.offline {
		d.add(check{ID: "machine.anchor.token", Client: "machine", Mode: "around", Status: "skip", Summary: "--offline"})
		return af
	}
	v := d.whoami(af.Token)
	switch {
	case v.ok && v.who.TokenScope == "account":
		tokens["anchor"] = v.who
		d.add(check{ID: "machine.anchor.token", Client: "machine", Mode: "around", Status: "warn",
			Summary: "the anchor is a pre-per-agent account token", Remediation: "It still works; re-run the installer to convert it."})
	case v.ok:
		tokens["anchor"] = v.who
		d.add(check{ID: "machine.anchor.token", Client: "machine", Mode: "around", Status: "ok",
			Summary: fmt.Sprintf("accepted: agent %s (%s)", v.who.Agent.Key, v.who.Agent.ClientKey)})
	case v.dead:
		d.add(check{ID: "machine.anchor.token", Client: "machine", Mode: "around", Status: "error",
			Summary:     "the server rejects the token in " + path + " (HTTP 401)",
			Remediation: "Tokens do not survive a server reset. Re-run the installer with a join code from your team: " + installCmd})
	default:
		d.add(check{ID: "machine.anchor.token", Client: "machine", Mode: "around", Status: "skip", Summary: "could not check: " + v.problem})
	}
	return af
}

// shellChecks: a METICHE_TOKEN in this shell that is not the saved anchor, and
// a project .mcp.json that expands an unset ${METICHE_TOKEN}.
func (d *doctor) shellChecks(af credential.AnchorFile) {
	a := d.a
	envTok, set := credential.EnvToken(a.env)
	switch {
	case !set:
	case envTok.Empty():
		d.add(check{ID: "machine.env.shell", Client: "machine", Mode: "around", Status: "warn",
			Summary: "$METICHE_TOKEN is set in this shell but is not a metiche token", Remediation: "Unset it, or open a new terminal."})
	case !af.Token.Empty() && envTok.Equal(af.Token):
		d.add(check{ID: "machine.env.shell", Client: "machine", Mode: "around", Status: "ok", Summary: "$METICHE_TOKEN is the token in ~/.metiche/env"})
	case credential.InBackups(a.paths, envTok):
		d.add(check{ID: "machine.env.shell", Client: "machine", Mode: "around", Status: "warn",
			Summary:     "this shell's $METICHE_TOKEN is stale: it is the token from a backup of ~/.metiche/env, from before the last install",
			Remediation: "Open a new terminal (or run: . ~/.metiche/env). metiche uses the saved token meanwhile."})
	default:
		if d.offline {
			break
		}
		v := d.whoami(envTok)
		switch {
		case v.dead:
			d.add(check{ID: "machine.env.shell", Client: "machine", Mode: "around", Status: "error",
				Summary:     "this shell exports a METICHE_TOKEN the server rejects (HTTP 401), and it is not the one in ~/.metiche/env",
				Remediation: "Open a new terminal, or unset METICHE_TOKEN; it outranks ~/.metiche/env for metiche and for install.sh."})
		case v.ok:
			d.add(check{ID: "machine.env.shell", Client: "machine", Mode: "around", Status: "warn",
				Summary:     "this shell's $METICHE_TOKEN differs from ~/.metiche/env; it outranks the saved token",
				Remediation: "If you did not set it on purpose, open a new terminal."})
		}
	}
	for dir := a.cwd; ; {
		p := filepath.Join(dir, ".mcp.json")
		if raw, err := os.ReadFile(p); err == nil && strings.Contains(string(raw), "${METICHE_TOKEN}") && !set {
			d.add(check{ID: "machine.env.shell", Client: "machine", Mode: "around", Status: "warn",
				Summary:     p + " expands ${METICHE_TOKEN}, which is not set in this shell",
				Remediation: "Claude Code started from here gets an empty bearer. Load it: . ~/.metiche/env"})
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
}

func (d *doctor) clientChecks(client string, tokens map[string]wire.Whoami) {
	a := d.a
	e := credential.ReadEntry(a.paths, client)
	path := a.paths.Tilde(e.Path)
	if !e.FileExists {
		d.add(check{ID: client + ".config", Client: client, Mode: "around", Status: "skip", Summary: "not configured for metiche (" + path + " absent)"})
		return
	}
	if e.ParseError != "" {
		d.add(check{ID: client + ".config", Client: client, Mode: "around", Status: "error",
			Summary: path + " does not parse: " + e.ParseError, Remediation: "The client will load no MCP servers from it; restore it from its backup."})
		return
	}
	if e.Present && e.Mode&0o077 != 0 && !e.Token.Empty() {
		d.add(check{ID: "machine.files.perms", Client: client, Mode: "around", Status: "error",
			Summary: fmt.Sprintf("%s holds a metiche token and is readable by others (mode %s)", path, perm(e.Mode)), Remediation: "Run: chmod 600 " + path})
	}
	if !e.Present {
		st := "skip"
		sum := "no metiche entry in " + path
		if len(e.OtherNames) > 0 {
			st, sum = "error", fmt.Sprintf("no \"metiche\" entry; stale %s points at metiche", strings.Join(e.OtherNames, ", "))
		}
		d.add(check{ID: client + ".registrations", Client: client, Mode: "around", Status: st, Summary: sum, Remediation: map[bool]string{true: "Re-run the installer; it registers \"metiche\" with this client's own token.", false: ""}[st == "error"]})
		return
	}
	if len(e.OtherNames) > 0 {
		d.add(check{ID: client + ".registrations", Client: client, Mode: "around", Status: "error",
			Summary:     fmt.Sprintf("%d metiche registrations: metiche, %s", len(e.OtherNames)+1, strings.Join(e.OtherNames, ", ")),
			Remediation: "Re-run the installer for metiche; remove the other entries you do not want."})
	} else {
		d.add(check{ID: client + ".registrations", Client: client, Mode: "around", Status: "ok", Summary: "one entry, \"metiche\", in " + path})
	}
	names := append([]string(nil), e.HeaderNames...)
	sort.Strings(names)
	hasAuth, extra := false, []string{}
	for _, n := range names {
		if strings.EqualFold(n, "Authorization") {
			hasAuth = true
		} else {
			extra = append(extra, n)
		}
	}
	switch {
	case e.URL != a.endpoint.URL:
		d.add(check{ID: client + ".headers", Client: client, Mode: "around", Status: "error",
			Summary: fmt.Sprintf("the entry's URL is %s, not %s", e.URL, a.endpoint.URL), Remediation: "Re-run the installer."})
	case !hasAuth:
		d.add(check{ID: client + ".headers", Client: client, Mode: "around", Status: "error",
			Summary: "the entry sends no Authorization header, so every tool except health is refused", Remediation: "Re-run the installer."})
	case e.EnvBased && client == "codex":
		d.add(check{ID: client + ".headers", Client: client, Mode: "around", Status: "error",
			Summary:     "Codex reads metiche's token from an environment variable, which a Codex launched from the Dock does not have",
			Remediation: "Re-run the installer; it writes Codex's own token into http_headers."})
	case e.EnvBased:
		d.add(check{ID: client + ".headers", Client: client, Mode: "around", Status: "warn",
			Summary: "the token comes from an environment variable, which depends on how the client was launched", Remediation: "Re-run the installer, which writes the client's own token literally."})
	case len(extra) > 0:
		d.add(check{ID: client + ".headers", Client: client, Mode: "around", Status: "warn",
			Summary: "the entry also sends " + strings.Join(extra, ", "), Remediation: "Re-run the installer; only Authorization is needed."})
	default:
		d.add(check{ID: client + ".headers", Client: client, Mode: "around", Status: "ok", Summary: "URL matches · headers: Authorization"})
	}
	if e.Token.Empty() || d.offline {
		if d.offline {
			d.add(check{ID: client + ".token", Client: client, Mode: "around", Status: "skip", Summary: "--offline"})
		}
		return
	}
	v := d.whoami(e.Token)
	expected := d.machine + "-" + client
	switch {
	case v.dead:
		d.add(check{ID: client + ".token", Client: client, Mode: "around", Status: "error",
			Summary: "the server rejects the token in " + path + " (HTTP 401)", Remediation: "Tokens do not survive a server reset. Re-run the installer."})
	case !v.ok:
		d.add(check{ID: client + ".token", Client: client, Mode: "around", Status: "skip", Summary: "could not check: " + v.problem})
	case v.who.TokenScope == "account":
		tokens[client] = v.who
		d.add(check{ID: client + ".token", Client: client, Mode: "around", Status: "warn", Summary: "a pre-per-agent account token", Remediation: "Re-run the installer."})
	case v.who.Agent != nil && v.who.Agent.ClientKey != expected:
		tokens[client] = v.who
		d.add(check{ID: client + ".token", Client: client, Mode: "around", Status: "warn",
			Summary:     fmt.Sprintf("the token belongs to agent %s, not %s", v.who.Agent.ClientKey, expected),
			Remediation: "It was copied from another client or machine. Re-run the installer so this client gets its own."})
	default:
		tokens[client] = v.who
		d.add(check{ID: client + ".token", Client: client, Mode: "around", Status: "ok", Summary: fmt.Sprintf("accepted: agent %s (%s)", v.who.Agent.Key, v.who.Agent.ClientKey)})
	}
}

func (d *doctor) identityChecks(tokens map[string]wire.Whoami) {
	if d.offline || len(tokens) == 0 {
		return
	}
	byAgent := map[string][]string{}
	accounts := map[string][]string{}
	for src, w := range tokens {
		accounts[w.AccountKey] = append(accounts[w.AccountKey], src)
		if src != "anchor" && w.Agent != nil {
			byAgent[w.Agent.Key] = append(byAgent[w.Agent.Key], credential.Titles[src])
		}
	}
	collapsed := false
	for agent, cs := range byAgent {
		if len(cs) > 1 {
			sort.Strings(cs)
			collapsed = true
			d.add(check{ID: "identity.collapse", Client: "identity", Mode: "server", Status: "error",
				Summary:     fmt.Sprintf("%s carry the same token (agent %s), so they are ONE agent on the board", strings.Join(cs, " and "), agent),
				Remediation: "Re-run the installer; it gives each client its own."})
		}
	}
	if !collapsed && len(byAgent) > 0 {
		d.add(check{ID: "identity.collapse", Client: "identity", Mode: "server", Status: "ok", Summary: "every configured client is its own agent"})
	}
	if len(accounts) > 1 {
		d.add(check{ID: "identity.person", Client: "identity", Mode: "server", Status: "error",
			Summary:     fmt.Sprintf("the tokens on this machine belong to %d different accounts", len(accounts)),
			Remediation: "A join without your anchor created a second person. Re-run the installer with METICHE_TOKEN set to the token of the account you want to keep."})
	} else {
		d.add(check{ID: "identity.person", Client: "identity", Mode: "server", Status: "ok", Summary: "every accepted token is one account"})
	}
	if w, ok := tokens["anchor"]; ok && w.Agent != nil {
		used := false
		for src, cw := range tokens {
			if src != "anchor" && cw.Agent != nil && cw.Agent.Key == w.Agent.Key {
				used = true
			}
		}
		if !used && len(tokens) > 1 {
			d.add(check{ID: "identity.anchor", Client: "identity", Mode: "server", Status: "warn",
				Summary: fmt.Sprintf("~/.metiche/env names agent %s, which no client on this machine uses", w.Agent.ClientKey)})
		}
	}
}

// bindingChecks are §2.2.1, from the working directory.
func (d *doctor) bindingChecks(tokens map[string]wire.Whoami) {
	a := d.a
	repo := gitx.Detect(a.ctx(), a.cwd)
	chain := binding.Chain(a.cwd)
	eff := binding.Resolve(chain, repo.Root)
	if len(chain) > 0 {
		f := chain[0]
		switch {
		case f.NotRegular:
			d.add(check{ID: "binding.file", Client: "binding", Mode: "around", Status: "error", Summary: f.Path + " is not a regular file"})
		case len(f.CredentialLike) > 0:
			d.add(check{ID: "binding.file", Client: "binding", Mode: "around", Status: "error",
				Summary:     fmt.Sprintf("%s holds something that looks like a credential (%s)", f.Path, strings.Join(f.CredentialLike, ", ")),
				Remediation: "A .metiche must only name a team and a project. Remove it, and if it was ever committed, treat that credential as leaked."})
		case !f.HasTeam && eff.Team == "":
			d.add(check{ID: "binding.file", Client: "binding", Mode: "around", Status: "error", Summary: f.Path + " has no team = line, so agents ignore it", Remediation: "Run metiche init --force here."})
		case len(f.Duplicates) > 0:
			d.add(check{ID: "binding.file", Client: "binding", Mode: "around", Status: "warn", Summary: fmt.Sprintf("%s sets %s twice; the last one wins", f.Path, strings.Join(f.Duplicates, ", "))})
		default:
			d.add(check{ID: "binding.file", Client: "binding", Mode: "around", Status: "ok", Summary: fmt.Sprintf("%s names %s", f.Path, eff.Team)})
		}
	}
	for _, f := range eff.IgnoredProjects {
		d.add(check{ID: "binding.project_scope", Client: "binding", Mode: "around", Status: "warn",
			Summary:     fmt.Sprintf("%s is above this repository (%s) and sets project = %s, which agents ignore", f.Path, repo.Root, f.Project),
			Remediation: "Remove that line; bind the repository itself with metiche init."})
	}
	if d.offline || !d.serverUp() {
		return
	}
	s, err := a.connect(false)
	if err != nil {
		d.add(check{ID: "machine.binding", Client: "binding", Mode: "server", Status: "skip", Summary: "no usable token to check bindings with"})
		return
	}
	defer s.close()
	lt, err := a.listTeams(s)
	if err != nil {
		return
	}
	names := duplicateNames(lt.Teams)
	reported := map[string]bool{}
	for _, t := range lt.Teams {
		if len(names[t.Slug]) > 0 && !reported[slugBase(t.Name)] {
			reported[slugBase(t.Name)] = true
			all := append([]string{t.Slug}, names[t.Slug]...)
			d.add(check{ID: "account.duplicate_teams", Client: "account", Mode: "server", Status: "warn",
				Summary:     fmt.Sprintf("%d teams named %q: %s", len(all), t.Name, strings.Join(all, ", ")),
				Remediation: "Keep one: bind repositories with metiche init --team <slug>."})
		}
	}
	if eff.Team == "" {
		if len(lt.Teams) > 1 {
			d.add(check{ID: "machine.binding", Client: "binding", Mode: "server", Status: "info",
				Summary: fmt.Sprintf("no .metiche here and you are on %d teams; your agent will ask which one", len(lt.Teams)), Remediation: "Bind it once: metiche init"})
		}
		return
	}
	member := false
	for _, t := range lt.Teams {
		if t.Slug == eff.Team {
			member = true
		}
	}
	if !member {
		d.add(check{ID: "machine.binding", Client: "binding", Mode: "server", Status: "error",
			Summary:     fmt.Sprintf("%s binds this directory to team %s, which is not one of your teams", eff.TeamPath, eff.Team),
			Remediation: "Your agents here will stop at every call. Join it with a code from one of its members, or rebind: metiche init --force"})
		return
	}
	d.add(check{ID: "machine.binding", Client: "binding", Mode: "server", Status: "ok", Summary: ".metiche names " + eff.Team + ", one of your teams"})
	if repo.RepoURL == "" {
		d.add(check{ID: "binding.project", Client: "binding", Mode: "server", Status: "skip", Summary: "no repository URL to compare"})
		return
	}
	ts, err := a.teamState(s, eff.Team, "projects", 500)
	if err != nil {
		return
	}
	var matches []wire.StateProject
	for _, p := range ts.Projects {
		if gitx.NormalizeRepoURL(p.RepoURL) == repo.RepoURL {
			matches = append(matches, p)
		}
	}
	if len(matches) > 1 {
		var parts []string
		for _, m := range matches {
			parts = append(parts, fmt.Sprintf("%s (%d live, %s)", m.Key, m.LiveSessions, a.ago(m.LastActivityAt)))
		}
		d.add(check{ID: "binding.split", Client: "binding", Mode: "server", Status: "error",
			Summary:     fmt.Sprintf("this repository is %d projects on %s: %s", len(matches), eff.Team, strings.Join(parts, ", ")),
			Remediation: "Collisions between sessions in different projects are never detected. End the stray project's sessions and start again from the repository root."})
	} else {
		d.add(check{ID: "binding.split", Client: "binding", Mode: "server", Status: "ok", Summary: "this repository is at most one project on " + eff.Team})
	}
	switch {
	case eff.Project == "":
	case len(matches) >= 1 && !containsKey(matches, eff.Project):
		d.add(check{ID: "binding.project", Client: "binding", Mode: "server", Status: "warn",
			Summary:     fmt.Sprintf("%s says project %s, but this repository is project %s on %s", eff.ProjectPath, eff.Project, matches[0].Key, eff.Team),
			Remediation: "Agents land on " + matches[0].Key + " (the repository wins). Fix the file: metiche init --force"})
	case len(matches) == 0:
		other := ""
		for _, p := range ts.Projects {
			if p.Key == eff.Project && p.RepoURL != "" {
				other = gitx.NormalizeRepoURL(p.RepoURL)
			}
		}
		if other != "" {
			d.add(check{ID: "binding.project", Client: "binding", Mode: "server", Status: "error",
				Summary: fmt.Sprintf("project %s on %s belongs to %s, so start_session will refuse it here", eff.Project, eff.Team, other), Remediation: "Run metiche init --force."})
		} else {
			d.add(check{ID: "binding.project", Client: "binding", Mode: "server", Status: "info", Summary: fmt.Sprintf("project %s will be created on the first start_session", eff.Project)})
		}
	default:
		d.add(check{ID: "binding.project", Client: "binding", Mode: "server", Status: "ok", Summary: "project " + eff.Project + " is this repository on " + eff.Team})
	}
	_ = tokens
}

func containsKey(ps []wire.StateProject, key string) bool {
	for _, p := range ps {
		if p.Key == key {
			return true
		}
	}
	return false
}
