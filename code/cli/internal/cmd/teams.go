package cmd

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mklfarha/metiche/cli/internal/binding"
	"github.com/mklfarha/metiche/cli/internal/config"
	"github.com/mklfarha/metiche/cli/internal/gitx"
	"github.com/mklfarha/metiche/cli/internal/mcpclient"
	"github.com/mklfarha/metiche/cli/internal/wire"
)

// subcommand splits off the first positional argument when it names one of
// subs, skipping global flags and their values.
func subcommand(args []string, subs ...string) (string, []string) {
	for i := 0; i < len(args); i++ {
		x := args[i]
		if x == "--url" || x == "--timeout" {
			i++
			continue
		}
		if strings.HasPrefix(x, "-") {
			continue
		}
		for _, s := range subs {
			if x == s {
				return s, append(append([]string{}, args[:i]...), args[i+1:]...)
			}
		}
		return "", args
	}
	return "", args
}

func (a *app) cmdTeams(args []string) error {
	sub, rest := subcommand(args, "create", "show", "rename", "leave")
	switch sub {
	case "create":
		return a.teamsCreate(rest)
	case "show":
		return a.teamsShow(rest)
	case "rename", "leave":
		return fail(exitUsage, "not_built", "`metiche teams %s` is not built yet: it needs the %s_team tool, which comes with the team_renamed and member_left event kinds (docs/CLI.md §4.10-§4.11)", sub, sub)
	}
	fs := a.flags("teams")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fail(exitUsage, "usage", "unknown teams subcommand %q (create, show)", pos[0])
	}
	s, err := a.connect(false)
	if err != nil {
		return err
	}
	defer s.close()
	lt, err := a.listTeams(s)
	if err != nil {
		return err
	}
	dups := duplicateNames(lt.Teams)
	if a.json {
		d := doc("teams")
		teams := []map[string]any{}
		for _, t := range lt.Teams {
			teams = append(teams, map[string]any{"slug": t.Slug, "name": t.Name, "role": t.Role, "members": t.Members,
				"active_session": t.ActiveSession, "duplicate_of": nonNil(dups[t.Slug])})
		}
		d["teams"], d["note"] = teams, lt.Note
		a.writeJSON(d)
		return nil
	}
	if len(lt.Teams) == 0 {
		a.out("%s", lt.Note)
		return nil
	}
	rows := [][]string{{"SLUG", "NAME", "ROLE", "MEMBERS", "YOU LIVE", ""}}
	for _, t := range lt.Teams {
		mark := ""
		if len(dups[t.Slug]) > 0 {
			mark = "! same name as " + strings.Join(dups[t.Slug], ", ")
		}
		rows = append(rows, []string{t.Slug, t.Name, t.Role, fmt.Sprint(t.Members), yesNo(t.ActiveSession), mark})
	}
	a.table(rows)
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// duplicateNames maps each slug to the other slugs whose names normalize the
// same (the server's slugKey(name, 40)).
func duplicateNames(teams []wire.TeamChoice) map[string][]string {
	byBase := map[string][]string{}
	for _, t := range teams {
		byBase[slugBase(t.Name)] = append(byBase[slugBase(t.Name)], t.Slug)
	}
	out := map[string][]string{}
	for _, t := range teams {
		for _, other := range byBase[slugBase(t.Name)] {
			if other != t.Slug {
				out[t.Slug] = append(out[t.Slug], other)
			}
		}
	}
	return out
}

// ── teams create ────────────────────────────────────────────────────────────

func newIdempotencyKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// sameNamed is the CLI's duplicate check, before any write (§1.4.1 step 1).
func sameNamed(teams []wire.TeamChoice, name string) []wire.TeamChoice {
	var out []wire.TeamChoice
	for _, t := range teams {
		if slugBase(t.Name) == slugBase(name) {
			out = append(out, t)
		}
	}
	return out
}

func duplicateTeamError(name string, dups []wire.TeamChoice) error {
	var parts, slugs []string
	for _, t := range dups {
		parts = append(parts, fmt.Sprintf("%s (%s, %s)", t.Slug, t.Role, plural(t.Members, "member", "members")))
		slugs = append(slugs, t.Slug)
	}
	e := fail(exitConflict, "already_exists",
		"You are already on a team named %q: %s.\nNothing was created. Use it in a repository:  metiche init --team %s\n"+
			"To create a second team with the same name anyway:  metiche teams create %q --allow-duplicate-name",
		dups[0].Name, strings.Join(parts, ", "), dups[0].Slug, name)
	e.extra = map[string]any{"existing": slugs}
	return e
}

// createTeam runs §1.4.1's steps 1-3: duplicate check, server version check,
// one create_team with a fresh idempotency key and no identity parameters.
func (a *app) createTeam(s *session, name string, allowDup, dry bool) (wire.CreateTeam, error) {
	lt, err := a.listTeams(s)
	if err != nil {
		return wire.CreateTeam{}, err
	}
	if dups := sameNamed(lt.Teams, name); len(dups) > 0 && !allowDup {
		return wire.CreateTeam{}, duplicateTeamError(name, dups)
	}
	required, found, err := s.c.RequiredParams(a.ctx(), "create_team")
	if err != nil {
		return wire.CreateTeam{}, a.mapErr(err)
	}
	if !found {
		return wire.CreateTeam{}, fail(exitError, "server_too_old", "this metiche server has no create_team tool")
	}
	for _, r := range required {
		if r == "member_name" || r == "client_key" || r == "agent_label" {
			return wire.CreateTeam{}, fail(exitError, "server_too_old",
				"this metiche server is too old for `teams create`; create the team with the installer (METICHE_TEAM_NAME)")
		}
	}
	if dry {
		return wire.CreateTeam{}, nil
	}
	args := map[string]any{"team_name": name, "idempotency_key": newIdempotencyKey()}
	if allowDup {
		args["allow_duplicate_name"] = true
	}
	var out wire.CreateTeam
	err = s.c.Call(a.ctx(), "create_team", args, &out)
	var me *mcpclient.Error
	if errors.As(err, &me) && me.Kind == mcpclient.KindUnreachable {
		// Retry once with the SAME key: a lost answer comes back as a replay.
		err = s.c.Call(a.ctx(), "create_team", args, &out)
	}
	if err != nil {
		return wire.CreateTeam{}, a.mapErr(err)
	}
	if !out.Token.Empty() || !out.TokenKept {
		return wire.CreateTeam{}, fail(exitError, "token_minted",
			"create_team answered with a new token, which is a bug: it was not saved or shown. Run `metiche doctor`.")
	}
	return out, nil
}

func (a *app) teamsCreate(args []string) error {
	fs := a.flags("teams create")
	allowDup := fs.Bool("allow-duplicate-name", false, "create a second team with a name you already have")
	quiet := fs.Bool("quiet", false, "print only the new team's slug")
	dry := fs.Bool("dry-run", false, "check, print what would be called, create nothing")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || slugBase(pos[0]) == "" {
		return fail(exitUsage, "usage", "usage: metiche teams create <name> [--allow-duplicate-name] [--quiet] [--dry-run]")
	}
	name := strings.TrimSpace(pos[0])
	s, err := a.connect(true)
	if err != nil {
		return err
	}
	defer s.close()
	res, err := a.createTeam(s, name, *allowDup, *dry)
	if err != nil {
		return err
	}
	if *dry {
		if a.json {
			d := doc("teams.create")
			d["dry_run"], d["would_call"] = true, map[string]any{"tool": "create_team", "team_name": name, "allow_duplicate_name": *allowDup}
			a.writeJSON(d)
			return nil
		}
		a.out("dry run: no team named %q on your account; would call create_team with team_name=%q and a fresh idempotency_key (no client_key, member_name or agent_label).", name, name)
		return nil
	}
	if a.json {
		d := doc("teams.create")
		d["team"] = map[string]any{"slug": res.TeamSlug, "name": res.TeamName, "visibility": res.Visibility}
		d["created"], d["join_code"], d["join_code_note"] = res.Created, res.JoinCode, res.JoinCodeNote
		a.writeJSON(d)
		return nil
	}
	if *quiet {
		a.out("%s", res.TeamSlug)
		return nil
	}
	a.printCreated(res)
	return nil
}

func (a *app) printCreated(res wire.CreateTeam) {
	verb := "created team"
	if !res.Created {
		verb = "team already created by this request:"
	}
	a.out("%s %s (%q) · %s · you are its owner", verb, res.TeamSlug, res.TeamName, res.Visibility)
	a.out("")
	a.out("  join code   %s   (shown once: it never expires and has no use limit)", res.JoinCode)
	a.out("")
	a.out("  For a narrower code:  metiche invite create --team %s --max-uses 5 --expires 48h", res.TeamSlug)
	a.out("  then revoke this one: metiche invite list --team %s; metiche invite revoke <invite-id>", res.TeamSlug)
	a.out("  Bind a repository:    cd <repo> && metiche init --team %s", res.TeamSlug)
}

// ── team resolution (§1.5) ──────────────────────────────────────────────────

type teamRef struct {
	slug   string
	source string // argument | binding | only_team
	path   string // the .metiche that named it
}

// resolveTeam: the argument, then the nearest .metiche, then your only team.
// Never guessed.
func (a *app) resolveTeam(s *session, arg string) (teamRef, error) {
	if arg = strings.TrimSpace(arg); arg != "" {
		return teamRef{slug: strings.ToLower(arg), source: "argument"}, nil
	}
	repo := gitx.Detect(a.ctx(), a.cwd)
	eff := binding.Resolve(binding.Chain(a.cwd), repo.Root)
	if eff.Team != "" {
		return teamRef{slug: eff.Team, source: "binding", path: eff.TeamPath}, nil
	}
	lt, err := a.listTeams(s)
	if err != nil {
		return teamRef{}, err
	}
	switch len(lt.Teams) {
	case 0:
		return teamRef{}, fail(exitError, "no_teams", "you are not on any team: `metiche teams create <name>`, or join one with the installer and a join code")
	case 1:
		return teamRef{slug: lt.Teams[0].Slug, source: "only_team"}, nil
	}
	slugs := make([]string, 0, len(lt.Teams))
	for _, t := range lt.Teams {
		slugs = append(slugs, t.Slug)
	}
	e := fail(exitUsage, "ambiguous_team", "you are on %d teams (%s); name one with --team <slug>, or bind this repository once with `metiche init`",
		len(slugs), strings.Join(slugs, ", "))
	e.extra = map[string]any{"candidates": slugs}
	return teamRef{}, e
}

// refused explains a not-found team named by a .metiche file.
func (a *app) refused(err error, ref teamRef) error {
	var ce *cliError
	if errors.As(err, &ce) && ce.exit == exitRefused && ref.source == "binding" {
		ce.detail = fmt.Sprintf("%s names team %s, which is not one of your teams (%s). Join that team with a code from one of its members, or rebind: metiche init --force",
			ref.path, ref.slug, ce.detail)
	}
	return err
}

// ── teams show ──────────────────────────────────────────────────────────────

func (a *app) teamsShow(args []string) error {
	fs := a.flags("teams show")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return fail(exitUsage, "usage", "usage: metiche teams show [<slug>]")
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
	members, err := a.teamState(s, ref.slug, "members", 500)
	if err != nil {
		return a.refused(err, ref)
	}
	projects, err := a.teamState(s, ref.slug, "projects", 500)
	if err != nil {
		return err
	}
	sessions, err := a.teamState(s, ref.slug, "sessions", 200)
	if err != nil {
		return err
	}
	team := members.Team
	if team == nil {
		return fail(exitError, "server_too_old", "this metiche server does not return the team block on get_team_state")
	}
	role := ""
	for _, m := range members.Members {
		if m.Mine {
			role = m.Role
		}
	}
	bindingPath := ""
	repo := gitx.Detect(a.ctx(), a.cwd)
	if eff := binding.Resolve(binding.Chain(a.cwd), repo.Root); eff.Team == team.Slug {
		bindingPath = eff.TeamPath
	}
	var boardURL any
	if team.Visibility == "public" {
		boardURL = config.BoardURL(config.BoardBase(a.env), team.Slug)
	}
	sameRepo := sameRepository(projects.Projects)

	if a.json {
		d := doc("teams.show")
		d["team"], d["role"], d["binding"], d["board_url"] = team, role, nullable(bindingPath), boardURL
		ps := []map[string]any{}
		for _, p := range projects.Projects {
			ps = append(ps, map[string]any{"key": p.Key, "name": p.Name, "repo_url": nullable(p.RepoURL), "live_sessions": p.LiveSessions,
				"last_activity_at": nullable(p.LastActivityAt), "same_repository_as": nonNil(sameRepo[p.Key])})
		}
		d["members"], d["projects"], d["sessions"] = members.Members, ps, nonNilSessions(sessions.Sessions)
		a.writeJSON(d)
		return nil
	}
	head := fmt.Sprintf("%s  %q  %s · you are %s", team.Slug, team.Name, team.Visibility, role)
	if bindingPath != "" {
		head += " · bound here by " + bindingPath
	}
	a.out("%s", head)
	if team.Visibility == "public" {
		a.out("board     %s", boardURL)
	} else {
		a.out("board     %s   private: metiche open signs this browser in", config.BoardURL(config.BoardBase(a.env), team.Slug))
	}
	for i, m := range members.Members {
		label := "members "
		if i > 0 {
			label = "        "
		}
		who := m.DisplayName + " (" + m.Role
		if m.Mine {
			who += ", you"
		}
		who += ")"
		var agents []string
		for _, ag := range m.Agents {
			when := "no session here"
			if ag.LastSessionAt != "" {
				when = a.ago(ag.LastSessionAt)
			}
			agents = append(agents, fmt.Sprintf("%s (%s)", ag.Label, when))
		}
		a.out("%s  %s   joined %s   agents: %s", label, who, first([]string{m.JoinedAt[:min(10, len(m.JoinedAt))]}), orNone(strings.Join(agents, ", ")))
	}
	if len(projects.Projects) == 0 {
		a.out("projects  (none yet)")
	}
	for i, p := range projects.Projects {
		label := "projects"
		if i > 0 {
			label = "        "
		}
		repoTxt := "(no repository recorded)"
		if p.RepoURL != "" {
			repoTxt = gitx.NormalizeRepoURL(p.RepoURL)
		}
		line := fmt.Sprintf("%s  %s   %s   %d live   last activity %s", label, p.Key, repoTxt, p.LiveSessions, a.ago(p.LastActivityAt))
		if len(sameRepo[p.Key]) > 0 {
			line += "   ! same repository as " + strings.Join(sameRepo[p.Key], ", ")
		}
		a.out("%s", line)
	}
	a.printSessions(sessions.Sessions)
	return nil
}

func (a *app) printSessions(ss []wire.StateSession) {
	if len(ss) == 0 {
		a.out("live      (nobody)")
		return
	}
	for i, x := range ss {
		label := "live    "
		if i > 0 {
			label = "        "
		}
		mine := ""
		if x.Mine {
			mine = "mine"
		}
		a.out("%s  %s  %s · %s  %s  %s  %q  %s  %s", label, x.Key, x.Member, x.Agent, x.Project, x.Branch, x.StatusLine, a.ago(x.LastSeen), mine)
	}
}

func nonNilSessions(s []wire.StateSession) []wire.StateSession {
	if s == nil {
		return []wire.StateSession{}
	}
	return s
}

// sameRepository maps each project key to the other keys whose repo_url
// normalizes the same: the split from the incident, seen from the team side.
func sameRepository(ps []wire.StateProject) map[string][]string {
	by := map[string][]string{}
	for _, p := range ps {
		if n := gitx.NormalizeRepoURL(p.RepoURL); n != "" {
			by[n] = append(by[n], p.Key)
		}
	}
	out := map[string][]string{}
	for _, keys := range by {
		sort.Strings(keys)
		for _, k := range keys {
			for _, o := range keys {
				if o != k {
					out[k] = append(out[k], o)
				}
			}
		}
	}
	return out
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
