package mcp

import (
	"fmt"
	"strings"

	team_entity "github.com/mklfarha/metiche/backend/entity/team"
)

// Binding a repository to a team.
//
// A team's board is visible to every member of that team. When start_session
// would CREATE a project (no active project on the team has this repository
// or this key), the work of a repository nobody on the team has seen before is
// about to appear there. With one team, RequireTeam picks it without a
// team_slug, so without this check an unrelated repository lands on the only
// board the person has, silently. docs/PLAN.md: never guess.
//
// So creating a project needs a confirmation, and without one start_session
// answers with a refusal that is NOT a tool failure: nothing went wrong, the
// agent has a question to put to its person. The confirmation is not stored;
// once the project exists the repository is bound and nobody is asked again.

const (
	// confirmByPerson: the person said yes to the question in the refusal.
	confirmByPerson = "person"
	// confirmByMeticheFile: a .metiche at or above the git root names this
	// team. A committed file means teammates' agents need not ask.
	confirmByMeticheFile = "metiche_file"

	codeConfirmRepoBinding = "confirm_repo_binding"
)

// parseConfirmNewProject validates confirm_new_project. "" is valid and means
// "not confirmed".
func parseConfirmNewProject(raw, teamSlug string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return false, nil
	case confirmByPerson:
		return true, nil
	case confirmByMeticheFile:
		if strings.TrimSpace(teamSlug) == "" {
			return false, fmt.Errorf(
				"confirm_new_project %q means a .metiche file names the team, so pass that team as team_slug; nothing was started",
				confirmByMeticheFile)
		}
		return true, nil
	default:
		return false, fmt.Errorf(
			"confirm_new_project must be %q (your person said yes) or %q (a .metiche at or above the git root names this team), or omitted (got %q); nothing was started",
			confirmByPerson, confirmByMeticheFile, truncate(raw, 40))
	}
}

// repoBindingRequired is what resolveProject returns instead of creating an
// unconfirmed project. Returned from inside Apply, so commit rolls the
// transaction back and no project, session or event is written.
type repoBindingRequired struct {
	// Repo is the repository's normalized identity, or the key when there is
	// no usable remote.
	Repo     string
	Key      string
	Existing []string
}

func newRepoBindingRequired(in projectInput, active []teamProject) *repoBindingRequired {
	repo := normalizeRepoURL(in.RepoURL)
	if repo == "" {
		repo = in.Key
	}
	existing := make([]string, 0, len(active))
	for _, p := range active {
		existing = append(existing, p.Key)
	}
	return &repoBindingRequired{Repo: repo, Key: in.Key, Existing: existing}
}

func (e *repoBindingRequired) Error() string {
	return fmt.Sprintf("%s: %s would be a new project on this team; confirm_new_project is required", codeConfirmRepoBinding, e.Repo)
}

// RepoBindingRefusal is start_session's answer when it needs the person's
// confirmation. ok is false and code says why; it is returned as a normal
// (non-error) result because the call did not fail.
type RepoBindingRefusal struct {
	OK                  bool     `json:"ok"`
	Code                string   `json:"code"`
	TeamSlug            string   `json:"team_slug"`
	TeamName            string   `json:"team_name"`
	Repo                string   `json:"repo"`
	ProjectKey          string   `json:"project_key"`
	ExistingProjectKeys []string `json:"existing_project_keys"`
	Note                string   `json:"note"`
}

func repoBindingRefusal(team team_entity.Team, e *repoBindingRequired) RepoBindingRefusal {
	question := fmt.Sprintf("Should work in %s go on team %s's board, where its members can see it?", e.Repo, team.Name)
	return RepoBindingRefusal{
		OK:                  false,
		Code:                codeConfirmRepoBinding,
		TeamSlug:            team.Slug,
		TeamName:            team.Name,
		Repo:                e.Repo,
		ProjectKey:          e.Key,
		ExistingProjectKeys: e.Existing,
		Note: fmt.Sprintf(
			"Nothing was created. This repository is not a project on team %s yet. Ask your person, verbatim: %q "+
				"If yes: call start_session again with the same arguments plus confirm_new_project: %q, "+
				"and write a .metiche file at the git root with the lines \"team = %s\" and \"project = %s\". "+
				"If no: do not use metiche in this repository, or create_team for this work and start_session with that team_slug.",
			team.Slug, question, confirmByPerson, team.Slug, e.Key),
	}
}
