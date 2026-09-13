package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/gofrs/uuid"
	"github.com/guregu/null/v6"

	"github.com/mklfarha/metiche/backend/app/coordination"
	projectmod "github.com/mklfarha/metiche/backend/core/module/project"
	project_types "github.com/mklfarha/metiche/backend/core/module/project/types"
	project_entity "github.com/mklfarha/metiche/backend/entity/project"
	"github.com/mklfarha/metiche/backend/enums"
)

// Which project does a start_session belong to?
//
// Claims and conflict detection are scoped per project, so this answer is the
// difference between two agents in one repository seeing each other and not.
// It used to be "whatever project_key the model sent", and two models in one
// repository sent two different keys. The repository's remote is the
// identity; the key is only a name for it.
//
// Resolved inside start_session's Apply, under the team row lock. That lock
// is what serializes two agents racing to create the first project for one
// repository: the second one's lookup sees the first one's insert. No unique
// index on repo_url is needed for that (and one could not be declared on the
// normalized value anyway, since normalization lives here, in code).

// maxTeamProjectsScanned bounds the repo_url scan inside the lock. A team's
// projects are its repositories — a handful, and capped by the plan — so this
// is a guard against a pathological team, not a working limit.
const maxTeamProjectsScanned = 500

// maxProjectKeysInNote bounds the list start_session reads back to a model
// when it creates a project on a team that already has some.
const maxProjectKeysInNote = 10

type projectInput struct {
	// Key is the project_key the agent sent, or the one derived from RepoURL.
	Key string
	// KeyDerived is true when the agent sent no project_key.
	KeyDerived bool
	// RepoURL is the canonical https form (canonicalRepoURL), or "" when none
	// was sent or the remote has no host.
	RepoURL string
	Name    string
	Branch  string
}

type projectResolution struct {
	Project project_entity.Project
	Created bool
	// ByRepo is true when (a) found the project by its repository, which is
	// the one step that can pick a project whose key the agent did not send.
	ByRepo bool
	// Others is the keys of the team's other active projects, oldest first,
	// set only when Created.
	Others []string
}

type teamProject struct {
	ID      uuid.UUID
	Key     string
	RepoURL string
}

// resolveProject picks, and if necessary creates, the project a session
// belongs to.
//
//	(a) an ACTIVE project in the team whose stored repo_url normalizes to this
//	    repo_url, whatever key it has and whatever key the agent sent;
//	(b) otherwise the project with this key — backfilling its repo_url when
//	    it has none, and refusing when it belongs to a DIFFERENT repository;
//	(c) otherwise a new project, carrying the normalized repo_url.
func (h *Handler) resolveProject(ctx context.Context, tc *TxContext, teamUUID uuid.UUID, in projectInput) (projectResolution, error) {
	var (
		active []teamProject
		loaded bool
	)
	load := func() error {
		if loaded {
			return nil
		}
		var err error
		if active, err = activeTeamProjects(ctx, tc, teamUUID); err != nil {
			return err
		}
		loaded = true
		return nil
	}

	// Compared by identity, so a legacy row stored as git@host:owner/repo.git
	// still matches the canonical https form.
	identity := normalizeRepoURL(in.RepoURL)

	// (a) The repository decides, not the name.
	if identity != "" {
		if err := load(); err != nil {
			return projectResolution{}, err
		}
		for _, p := range active {
			if p.RepoURL == "" || normalizeRepoURL(p.RepoURL) != identity {
				continue
			}
			proj, found, err := h.projectByKey(ctx, tc.Tx, teamUUID, p.Key)
			if err != nil {
				return projectResolution{}, err
			}
			if !found {
				// Read a moment ago on the same transaction, under the lock.
				return projectResolution{}, fmt.Errorf("project %q vanished while starting the session", p.Key)
			}
			if err := h.canonicalizeRepoURL(ctx, tc, &proj, in.RepoURL); err != nil {
				return projectResolution{}, err
			}
			return projectResolution{Project: proj, ByRepo: true}, nil
		}
	}

	// (b) Today's behaviour, now with the repository checked.
	proj, found, err := h.projectByKey(ctx, tc.Tx, teamUUID, in.Key)
	if err != nil {
		return projectResolution{}, err
	}
	if found {
		if identity == "" {
			return projectResolution{Project: proj}, nil
		}
		stored := normalizeRepoURL(proj.RepoURL.String)
		if stored != "" && stored != identity {
			// Not merged. Two repositories under one key would put two
			// unrelated codebases' claims in one namespace, and a path like
			// app/rest.go would collide across them.
			return projectResolution{}, projectKeyTakenError(in, stored)
		}
		// Either a project created by key alone before anyone sent its
		// repo_url (backfill), or an inactive project (a skips those) whose
		// stored spelling predates normalization.
		if err := h.canonicalizeRepoURL(ctx, tc, &proj, in.RepoURL); err != nil {
			return projectResolution{}, err
		}
		return projectResolution{Project: proj}, nil
	}

	// (c) Created on first use so joining a team and starting work is one
	// call each, not three. The defaults matter more here than anywhere:
	// nuzur generates most of the Go in this repo, and without the
	// generated-code ignore patterns every agent would collide with every
	// other agent on files nobody hand-edits.
	if err := load(); err != nil {
		return projectResolution{}, err
	}
	id, err := uuid.NewV4()
	if err != nil {
		return projectResolution{}, err
	}
	proj = project_entity.Project{
		ID:                   id,
		TeamUUID:             teamUUID,
		Key:                  in.Key,
		Name:                 truncate(firstNonEmpty(in.Name, in.Key), 120),
		RepoURL:              nullString(in.RepoURL),
		DefaultBranch:        nullString(truncate(in.Branch, 120)),
		IgnorePatterns:       coordination.DefaultIgnorePatterns(),
		HotspotPatterns:      coordination.DefaultHotspotPatterns(),
		CaseInsensitivePaths: true,
		Cadence:              enums.PROJECT_CADENCE_HACKATHON,
		Status:               enums.RECORD_STATUS_ACTIVE,
	}
	if _, err := h.core.Project().Insert(ctx,
		project_types.UpsertRequest{Project: proj}, projectmod.WithSQLTransaction(tc.Tx)); err != nil {
		return projectResolution{}, err
	}
	others := make([]string, 0, len(active))
	for _, p := range active {
		others = append(others, p.Key)
	}
	return projectResolution{Project: proj, Created: true, Others: others}, nil
}

// activeTeamProjects lists a team's active projects, oldest first, ties by
// key. Deterministic so that when legacy duplicates share a repository, (a)
// keeps picking the same one — and created_at has second granularity, so
// projects made in the same second would otherwise order by a random uuid.
//
// Bounded twice — by uq_project_team_key's team_uuid prefix and by
// maxTeamProjectsScanned — because it runs inside the team lock.
func activeTeamProjects(ctx context.Context, tc *TxContext, teamUUID uuid.UUID) ([]teamProject, error) {
	rows, err := tc.Tx.QueryContext(ctx,
		"SELECT `id`, `key`, COALESCE(`repo_url`, '') FROM `project` "+
			"WHERE `team_uuid` = ? AND `status` = ? ORDER BY `created_at` ASC, `key` ASC LIMIT ?",
		teamUUID.String(), enums.RECORD_STATUS_ACTIVE, maxTeamProjectsScanned)
	if err != nil {
		return nil, retryable(err, "listing the team's projects")
	}
	defer func() { _ = rows.Close() }()

	var out []teamProject
	for rows.Next() {
		var id string
		var p teamProject
		if err := rows.Scan(&id, &p.Key, &p.RepoURL); err != nil {
			return nil, err
		}
		if p.ID, err = uuid.FromString(id); err != nil {
			return nil, fmt.Errorf("project %q has a malformed id: %w", p.Key, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, retryable(err, "listing the team's projects")
	}
	return out, nil
}

// canonicalizeRepoURL stores the canonical repo_url on a project whose stored
// value is missing or spelled differently. Rewriting a legacy spelling
// is not cosmetic: it is also what removes a credential somebody's remote
// carried before normalization existed.
//
// A targeted UPDATE rather than the generated full-row update, so nothing
// else on the row is written back. Every reader of project in this package
// skips the cache.
func (h *Handler) canonicalizeRepoURL(ctx context.Context, tc *TxContext, proj *project_entity.Project, canonical string) error {
	if canonical == "" || proj.RepoURL.String == canonical {
		return nil
	}
	if _, err := tc.Tx.ExecContext(ctx,
		"UPDATE `project` SET `repo_url` = ?, `updated_at` = ? WHERE `id` = ?",
		canonical, tc.Now, proj.ID.String()); err != nil {
		return retryable(err, "recording the project's repo_url")
	}
	proj.RepoURL = null.StringFrom(canonical)
	return nil
}

// projectKeyTakenError tells an agent its key names somebody else's
// repository. Both repositories in it are identities, so neither can carry a
// credential.
func projectKeyTakenError(in projectInput, stored string) error {
	mine := normalizeRepoURL(in.RepoURL)
	if in.KeyDerived {
		return fmt.Errorf(
			"the project key %q derived from repo_url %s is already taken by a different repository (%s) on this team — "+
				"pass project_key explicitly with a name for this repository that is not taken; nothing was started",
			in.Key, mine, stored)
	}
	return fmt.Errorf(
		"project_key %q is already taken by a different repository (%s), and your repo_url is %s — "+
			"use the name of your git root folder (basename of git rev-parse --show-toplevel) as project_key, "+
			"or omit project_key and send only repo_url; nothing was started",
		in.Key, stored, mine)
}

// keyOverrideNote tells an agent that its repository already had a project
// under a different key, so the key it sent was not used. Empty when there is
// nothing to say: the key matched, the project was not found by repository,
// or the agent sent no key at all (sending only repo_url is the right call,
// and "you sent project_key" would be untrue).
//
// Without it the agent keeps sending its own name for the repository, and a
// teammate reading its messages has no way to find that name on the board.
// Built inside Apply, like every note, so a replay repeats it.
func keyOverrideNote(in projectInput, res projectResolution) string {
	if !res.ByRepo || in.KeyDerived || res.Project.Key == in.Key {
		return ""
	}
	return fmt.Sprintf("you sent project_key %q; this repository is project %q — use that key (or send only repo_url)",
		in.Key, res.Project.Key)
}

// newProjectNote is what start_session says when it created a project. On a
// team that already has projects it names them, because an agent that
// guessed a key for a repository the team already tracks is exactly the case
// where a new project is a silent mistake: its claims would never meet its
// teammates'.
//
// Built inside Apply from rows read under the lock, so a replay says what was
// true when the session started.
func newProjectNote(key string, others []string, sessionKey string) string {
	note := fmt.Sprintf("project %q created", key)
	if len(others) == 0 {
		return note
	}
	shown, more := others, 0
	if len(shown) > maxProjectKeysInNote {
		shown, more = shown[:maxProjectKeysInNote], len(others)-maxProjectKeysInNote
	}
	quoted := make([]string, 0, len(shown))
	for _, k := range shown {
		quoted = append(quoted, fmt.Sprintf("%q", k))
	}
	list := strings.Join(quoted, ", ")
	if more > 0 {
		list += fmt.Sprintf(" and %d more", more)
	}
	return note + fmt.Sprintf(
		"; this team already has project(s) %s — if this is the same repository as one of those, "+
			"end_session %s and start_session again with repo_url (git remote get-url origin) or that project's key",
		list, sessionKey)
}
