package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mklfarha/metiche/cli/internal/binding"
	"github.com/mklfarha/metiche/cli/internal/gitx"
	"github.com/mklfarha/metiche/cli/internal/wire"
)

// metiche init (docs/CLI.md §1.10): bind a directory — by default the git root
// — to a team and a project, once, so no agent in it has to ask or guess.
//
// On a terminal with none of --team, --project, --create, --yes, --dry-run or
// --json it asks: which of your teams, or a new one; which project key; and
// whether to write the file. With any of those flags it never asks, and a
// choice it cannot make without asking exits 2 with the options.

type initState struct {
	repo         gitx.Repo
	dir          string
	target       string
	placement    string
	writeProject bool
	existing     *binding.File
	existingRaw  []byte

	slug, name, teamSource, teamFrom string
	created                          *wire.CreateTeam
	teamIsNew                        bool

	projectKey, projectSource string
	projectExists             bool

	warnings []string
}

func (a *app) cmdInit(args []string) error {
	fs := a.flags("init")
	teamFlag := fs.String("team", "", "the team to bind to, by slug")
	projectFlag := fs.String("project", "", "the project key to write")
	createFlag := fs.String("create", "", "create a new team with this name and bind to it")
	allowDup := fs.Bool("allow-duplicate-name", false, "with --create: allow a name you already have")
	here := fs.Bool("here", false, "write in the current directory, even below the git root")
	parent := fs.Bool("parent", false, "write in the directory containing the git root (team only)")
	yes := fs.Bool("yes", false, "do not ask; write without confirmation")
	dry := fs.Bool("dry-run", false, "resolve everything, print the file, write nothing")
	force := fs.Bool("force", false, "replace a different binding")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fail(exitUsage, "usage", "init takes no arguments (did you mean --team %s?)", pos[0])
	}
	if *here && *parent {
		return fail(exitUsage, "usage", "--here and --parent cannot be used together")
	}
	if *teamFlag != "" && *createFlag != "" {
		return fail(exitUsage, "usage", "--team and --create cannot be used together")
	}
	interactive := a.tty && !a.json && !*yes && !*dry && *teamFlag == "" && *projectFlag == "" && *createFlag == ""

	st := &initState{}
	cwd := a.cwd
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	st.repo = gitx.Detect(a.ctx(), cwd)
	switch {
	case *parent:
		if st.repo.Root == "" {
			return fail(exitUsage, "usage", "not in a git repository, so there is no parent of a git root: use --here")
		}
		if *projectFlag != "" {
			return fail(exitUsage, "usage", "--parent never writes project: a .metiche above a repository would bind every repository beneath it to one project")
		}
		st.dir, st.placement = filepath.Dir(st.repo.Root), "parent"
	case *here:
		st.dir, st.placement = cwd, "here"
		st.writeProject = st.repo.Root != "" || *projectFlag != ""
	case st.repo.Root != "":
		st.dir, st.placement, st.writeProject = st.repo.Root, "git_root", true
	default:
		st.dir, st.placement = cwd, "cwd"
		st.writeProject = *projectFlag != ""
	}
	st.target = filepath.Join(st.dir, binding.FileName)
	if f, err := binding.Parse(st.target); err == nil {
		if f.NotRegular {
			return fail(exitError, "refused", "%s is not a regular file (a symlink or something else); remove it first", st.target)
		}
		st.existing = &f
		st.existingRaw, _ = os.ReadFile(st.target)
	}
	inherited := binding.Resolve(binding.Chain(filepath.Dir(st.dir)), st.repo.Root)

	if st.repo.Root != "" {
		origin := "no repository URL"
		if st.repo.RepoURL != "" {
			origin = st.repo.Remote + " " + st.repo.RepoURL
		} else if st.repo.Note != "" {
			origin = st.repo.Note
		}
		a.out("repository  %s  (%s)", st.repo.Root, origin)
	} else {
		a.out("no git repository here: the .metiche goes in %s, and there is no repository to match a project by", st.dir)
	}

	s, err := a.connect(*createFlag != "")
	if err != nil {
		return err
	}
	defer func() { s.close() }()
	lt, err := a.listTeams(s)
	if err != nil {
		return err
	}

	// ── team ────────────────────────────────────────────────────────────────
	switch {
	case *createFlag != "":
		name := strings.TrimSpace(*createFlag)
		if slugBase(name) == "" {
			return fail(exitUsage, "usage", "--create needs a name with at least one letter or digit")
		}
		res, err := a.createTeam(s, name, *allowDup, *dry)
		if err != nil {
			return err
		}
		st.teamSource, st.teamIsNew = "created", true
		if *dry {
			st.slug, st.name = slugBase(name), name
			st.warnings = append(st.warnings, "dry run: the team is not created, so its slug is shown as "+st.slug+"; the server may add a suffix")
		} else {
			st.slug, st.name, st.created = res.TeamSlug, res.TeamName, &res
			if !a.json {
				a.printCreated(res)
				a.out("")
			}
		}
	case *teamFlag != "":
		st.slug, st.teamSource = strings.ToLower(strings.TrimSpace(*teamFlag)), "flag"
	case interactive:
		current := ""
		if st.existing != nil {
			current = st.existing.Team
		}
		if err := a.chooseTeam(&s, lt, current, st); err != nil {
			return err
		}
	case st.existing != nil && st.existing.HasTeam:
		st.slug, st.teamSource, st.teamFrom = st.existing.Team, "inherited", st.target
		a.out("team        %s (from %s)", st.slug, st.target)
	case inherited.Team != "":
		st.slug, st.teamSource, st.teamFrom = inherited.Team, "inherited", inherited.TeamPath
		a.out("team        %s (inherited from %s)", st.slug, inherited.TeamPath)
	default:
		switch len(lt.Teams) {
		case 0:
			return fail(exitError, "no_teams", "you are not on any team: `metiche teams create <name>`, `metiche init --create <name>`, or join one with the installer and a join code")
		case 1:
			st.slug, st.teamSource = lt.Teams[0].Slug, "only_team"
			a.out("team        you are on one team, %s; binding to it", st.slug)
		default:
			slugs := make([]string, 0, len(lt.Teams))
			for _, t := range lt.Teams {
				slugs = append(slugs, t.Slug)
			}
			e := fail(exitUsage, "ambiguous_team",
				"you are on %d teams (%s) and metiche init will not guess which one this repository belongs to.\n"+
					"Run it in a terminal to choose, or say which:\n  metiche init --team <slug>\n  metiche init --create <name>",
				len(slugs), strings.Join(slugs, ", "))
			e.extra = map[string]any{"candidates": slugs, "options": []string{"--team <slug>", "--create <name>"}}
			return e
		}
	}
	if !st.teamIsNew {
		found := false
		for _, t := range lt.Teams {
			if t.Slug == st.slug {
				found, st.name = true, t.Name
			}
		}
		if !found {
			msg := fmt.Sprintf("%s is not one of your teams", st.slug)
			if st.teamFrom != "" {
				msg = fmt.Sprintf("%s names team %s, which is not one of your teams. Join it with a code from one of its members, or rebind: metiche init --team <slug> --force", st.teamFrom, st.slug)
			}
			return fail(exitRefused, "not_found", "%s", msg)
		}
	}

	// ── project ─────────────────────────────────────────────────────────────
	if st.writeProject {
		if err := a.resolveProject(s, st, strings.TrimSpace(*projectFlag), interactive); err != nil {
			return err
		}
	} else {
		st.projectSource = "none"
	}

	// ── compose and compare ─────────────────────────────────────────────────
	var content []byte
	action := "created"
	if st.existing != nil {
		if st.existing.Team == st.slug && st.existing.Project == st.projectKey {
			return a.initDone(st, "unchanged", false, *dry)
		}
		content = binding.Rewrite(st.existingRaw, st.slug, st.projectKey)
		action = "updated"
		a.printDiff(st)
		if len(st.existing.Unknown) > 0 {
			st.warnings = append(st.warnings, fmt.Sprintf("%s has keys metiche does not know (%s); they are kept", st.target, strings.Join(st.existing.Unknown, ", ")))
		}
		if !*force {
			e := fail(exitConflict, "different_binding", "%s already binds this directory differently. Nothing was written. Re-run with --force to replace it.", st.target)
			e.extra = map[string]any{"path": st.target, "previous": map[string]any{"team": st.existing.Team, "project": nullable(st.existing.Project)},
				"wanted": map[string]any{"team": st.slug, "project": nullable(st.projectKey)}}
			return e
		}
	} else {
		content = binding.Render(st.slug, st.projectKey)
	}
	if *dry {
		if !a.json {
			a.out("would write %s:", st.target)
			for _, l := range strings.Split(strings.TrimRight(string(content), "\n"), "\n") {
				a.out("  %s", l)
			}
		}
		return a.initDone(st, "would_"+strings.TrimSuffix(action, "d"), false, true)
	}
	if interactive {
		a.out("")
		a.out("%s will say:", st.target)
		for _, l := range strings.Split(strings.TrimRight(string(content), "\n"), "\n") {
			a.out("  %s", l)
		}
		if !a.confirm(fmt.Sprintf("Write %s?", st.target)) {
			a.out("Nothing was written.")
			return nil
		}
	}
	if err := binding.Write(st.dir, content); err != nil {
		if errors.Is(err, binding.ErrRefused) {
			return fail(exitError, "refused", "%v", err)
		}
		return fail(exitError, "write_failed", "could not write %s: %v", st.target, err)
	}
	return a.initDone(st, action, interactive, false)
}

// chooseTeam is the interactive team question: one of your teams, or a new
// one. No default: Enter alone asks again.
func (a *app) chooseTeam(sp **session, lt wire.ListTeams, current string, st *initState) error {
	for {
		n := len(lt.Teams)
		if n == 0 {
			a.out("You are not on any team yet.")
		} else {
			a.out("You are on %s. Which one does this repository belong to?", plural(n, "team", "teams"))
		}
		rows := [][]string{}
		for i, t := range lt.Teams {
			extra := ""
			if t.ActiveSession {
				extra = "you are live here"
			}
			if t.Slug == current {
				extra = strings.TrimSpace(extra + "  bound here now")
			}
			rows = append(rows, []string{fmt.Sprintf("  %d)", i+1), t.Slug, t.Name, t.Role, plural(t.Members, "member", "members"), extra})
		}
		rows = append(rows, []string{fmt.Sprintf("  %d)", n+1), "create a new team", "", "", "", ""})
		a.table(rows)
		choice, err := a.choose("team", n+1)
		if err != nil {
			return err
		}
		if choice <= n {
			t := lt.Teams[choice-1]
			st.slug, st.name, st.teamSource = t.Slug, t.Name, "prompt"
			return nil
		}
		name, err := a.ask("team name", "")
		if err != nil {
			return err
		}
		if slugBase(name) == "" {
			a.out("  a team name needs at least one letter or digit")
			continue
		}
		if dups := sameNamed(lt.Teams, name); len(dups) > 0 {
			a.out("You are already on a team named %q: %s. Nothing was created.", dups[0].Name, dups[0].Slug)
			if a.confirm(fmt.Sprintf("Use %s instead?", dups[0].Slug)) {
				st.slug, st.name, st.teamSource = dups[0].Slug, dups[0].Name, "prompt"
				return nil
			}
			continue
		}
		if (*sp).who.TokenScope != "agent" {
			s2, err := a.connect(true)
			if err != nil {
				return err
			}
			(*sp).close()
			*sp = s2
		}
		res, err := a.createTeam(*sp, name, false, false)
		if err != nil {
			var ce *cliError
			if errors.As(err, &ce) && ce.exit == exitConflict {
				a.out("%s", ce.detail)
				continue
			}
			return err
		}
		a.printCreated(res)
		a.out("")
		st.slug, st.name, st.teamSource, st.created, st.teamIsNew = res.TeamSlug, res.TeamName, "created", &res, true
		return nil
	}
}

// resolveProject is §1.10.2's project half.
func (a *app) resolveProject(s *session, st *initState, flagKey string, interactive bool) error {
	var projects []wire.StateProject
	if !(st.teamIsNew && st.created == nil) {
		ts, err := a.teamState(s, st.slug, "projects", 500)
		if err != nil {
			return err
		}
		projects = ts.Projects
	}
	byKey := map[string]wire.StateProject{}
	var matches []wire.StateProject
	for _, p := range projects {
		byKey[p.Key] = p
		if st.repo.RepoURL != "" && gitx.NormalizeRepoURL(p.RepoURL) == st.repo.RepoURL {
			matches = append(matches, p)
		}
	}
	otherRepo := func(key string) string {
		p, ok := byKey[key]
		if !ok || p.RepoURL == "" || st.repo.RepoURL == "" {
			return ""
		}
		if n := gitx.NormalizeRepoURL(p.RepoURL); n != st.repo.RepoURL {
			return n
		}
		return ""
	}
	checkKey := func(key string) error {
		if !binding.KeyPattern.MatchString(key) {
			return fail(exitUsage, "usage", "project key %q must match %s", key, binding.KeyPattern.String())
		}
		if o := otherRepo(key); o != "" {
			return fail(exitConflict, "project_other_repository", "project %s on %s belongs to %s; choose another key", key, st.slug, o)
		}
		return nil
	}

	switch {
	case flagKey != "":
		if err := checkKey(flagKey); err != nil {
			return err
		}
		st.projectKey, st.projectSource = flagKey, "flag"
		if len(matches) == 1 && matches[0].Key != flagKey {
			st.warnings = append(st.warnings, fmt.Sprintf("this repository is already project %s on %s; agents will land there, because the repository wins over .metiche", matches[0].Key, st.slug))
		}
	case len(matches) == 1:
		st.projectKey, st.projectSource = matches[0].Key, "server_repo_match"
	case len(matches) > 1:
		keys := make([]string, 0, len(matches))
		for _, m := range matches {
			keys = append(keys, m.Key)
		}
		if !interactive {
			e := fail(exitUsage, "ambiguous_project", "this repository is %d projects on %s (%s); choose one with --project <key>, and run `metiche doctor`, which reports the split",
				len(keys), st.slug, strings.Join(keys, ", "))
			e.extra = map[string]any{"candidates": keys}
			return e
		}
		a.out("This repository is %d projects on %s (run `metiche doctor` afterwards: it reports the split):", len(keys), st.slug)
		for i, m := range matches {
			a.out("  %d) %s   %d live   last activity %s", i+1, m.Key, m.LiveSessions, a.ago(m.LastActivityAt))
		}
		c, err := a.choose("project", len(matches))
		if err != nil {
			return err
		}
		st.projectKey, st.projectSource = matches[c-1].Key, "prompt"
	case st.repo.RepoURL != "":
		st.projectKey, st.projectSource = gitx.ProjectKeyFromRepoURL(st.repo.RepoURL), "derived_remote"
	default:
		st.projectKey, st.projectSource = gitx.KeyFromDirName(filepath.Base(st.repo.Root)), "derived_dirname"
	}

	if interactive && len(matches) <= 1 {
		for {
			key, err := a.ask("project", st.projectKey)
			if err != nil {
				return err
			}
			key = strings.TrimSpace(key)
			if err := checkKey(key); err != nil {
				a.out("  %s", err.Error())
				continue
			}
			if len(matches) == 1 && key != matches[0].Key {
				a.out("  this repository is already project %s on %s, and the repository wins: agents land on %s whatever .metiche says", matches[0].Key, st.slug, matches[0].Key)
				continue
			}
			if key != st.projectKey {
				st.projectSource = "prompt"
			}
			st.projectKey = key
			break
		}
	} else if st.projectKey != "" && !binding.KeyPattern.MatchString(st.projectKey) {
		// A derived key the file format cannot hold (for example upper case on
		// a case-sensitive forge): lower it and say so.
		lowered := gitx.KeyFromDirName(st.projectKey)
		st.warnings = append(st.warnings, fmt.Sprintf("the derived key %q is written as %q; pass --project to choose", st.projectKey, lowered))
		st.projectKey = lowered
	}
	if o := otherRepo(st.projectKey); o != "" && st.projectSource != "flag" {
		return fail(exitConflict, "project_other_repository", "project %s on %s belongs to %s; choose another key with --project", st.projectKey, st.slug, o)
	}

	p, exists := byKey[st.projectKey]
	st.projectExists = exists
	switch {
	case st.projectSource == "server_repo_match":
		a.out("project     %s: already a project on %s for this repository", st.projectKey, st.slug)
	case exists && p.RepoURL == "":
		a.out("project     %s: a project on %s with no repository recorded; the next start_session records it", st.projectKey, st.slug)
	case exists:
		a.out("project     %s: already a project on %s", st.projectKey, st.slug)
	default:
		a.out("project     %s: new on %s; the first start_session in this repository creates it", st.projectKey, st.slug)
	}
	return nil
}

func (a *app) printDiff(st *initState) {
	if a.json || st.existing == nil {
		return
	}
	a.out("%s already binds this directory:", st.target)
	if st.existing.Team != st.slug {
		a.out("  - team = %s", st.existing.Team)
		a.out("  + team = %s", st.slug)
	} else {
		a.out("    team = %s", st.slug)
	}
	switch {
	case st.existing.Project == st.projectKey && st.projectKey != "":
		a.out("    project = %s", st.projectKey)
	case st.existing.Project != st.projectKey:
		if st.existing.Project != "" {
			a.out("  - project = %s", st.existing.Project)
		}
		if st.projectKey != "" {
			a.out("  + project = %s", st.projectKey)
		}
	}
}

// initDone reports parents, the commit suggestion and the JSON document.
func (a *app) initDone(st *initState, action string, interactive, dry bool) error {
	cwd := a.cwd
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	written := action == "created" || action == "updated"
	if action == "unchanged" {
		a.out("already bound: %s names team %s%s; nothing was written.", st.target, st.slug, map[bool]string{true: " and project " + st.projectKey, false: ""}[st.projectKey != ""])
	} else if written {
		a.out("wrote       %s", st.target)
		a.out("  team = %s", st.slug)
		if st.projectKey != "" {
			a.out("  project = %s", st.projectKey)
		}
	}

	overrides := []map[string]any{}
	for _, f := range binding.Chain(filepath.Dir(st.dir)) {
		if f.HasTeam && f.Team != st.slug {
			overrides = append(overrides, map[string]any{"path": f.Path, "team": f.Team})
			st.warnings = append(st.warnings, fmt.Sprintf("this file overrides %s (%s) for %s and below", f.Path, f.Team, st.dir))
		} else if f.HasTeam && st.placement != "parent" {
			st.warnings = append(st.warnings, fmt.Sprintf("%s also names %s: redundant for you, but this file binds every clone of the repository once committed", f.Path, f.Team))
		}
		if f.HasProject && st.repo.Root != "" {
			st.warnings = append(st.warnings, fmt.Sprintf("%s sets project = %s above this repository; agents ignore it", f.Path, f.Project))
		}
	}
	if st.placement == "parent" && st.repo.Root != "" {
		if f, err := binding.Parse(filepath.Join(st.repo.Root, binding.FileName)); err == nil && f.HasTeam && f.Team != st.slug {
			st.warnings = append(st.warnings, fmt.Sprintf("%s names %s, and the repository's own file still wins inside it", f.Path, f.Team))
		}
	}
	eff := binding.Resolve(binding.Chain(cwd), st.repo.Root)
	for _, w := range st.warnings {
		a.out("note: %s", w)
	}

	commitHint := false
	if !dry && st.placement != "parent" && st.repo.Root != "" {
		tracked := gitx.Tracked(a.ctx(), st.dir)
		ignored := gitx.Ignored(a.ctx(), st.dir)
		switch {
		case ignored:
			a.out(".metiche is git-ignored here; teammates' agents will not see it.")
		case !tracked:
			commitHint = true
			show := true
			if interactive {
				show = a.confirm("Show the git command that shares this binding with your teammates?")
			}
			if show {
				a.out("Commit it so every clone binds the same way:")
				a.out("  git add .metiche && git commit -m \"Bind to metiche team %s\"", st.slug)
				a.out("If this repository is public, committing publishes the team slug. A slug grants no access, but if you would rather not publish it, add .metiche to .git/info/exclude.")
			}
		}
	}

	if a.json {
		d := doc("init")
		var prev any
		if st.existing != nil && action != "unchanged" {
			prev = map[string]any{"team": st.existing.Team, "project": nullable(st.existing.Project)}
		}
		var repo any
		if st.repo.Root != "" {
			repo = map[string]any{"root": st.repo.Root, "remote": nullable(st.repo.Remote), "repo_url": nullable(st.repo.RepoURL)}
		}
		d["path"], d["placement"], d["action"], d["dry_run"] = st.target, st.placement, action, dry
		team := map[string]any{"slug": st.slug, "name": st.name, "source": st.teamSource}
		d["team"] = team
		if st.created != nil {
			d["created_team"] = map[string]any{"slug": st.created.TeamSlug, "join_code": st.created.JoinCode, "join_code_note": st.created.JoinCodeNote}
		}
		if st.projectSource == "none" {
			d["project"] = map[string]any{"key": nil, "source": "none", "server_project_exists": false}
		} else {
			d["project"] = map[string]any{"key": st.projectKey, "source": st.projectSource, "server_project_exists": st.projectExists}
		}
		d["repository"], d["previous"] = repo, prev
		d["effective"] = map[string]any{"path": nullable(eff.TeamPath), "team": nullable(eff.Team), "project": nullable(eff.Project)}
		d["overrides"], d["warnings"], d["commit_hint"] = overrides, nonNil(st.warnings), commitHint
		a.writeJSON(d)
	}
	return nil
}
