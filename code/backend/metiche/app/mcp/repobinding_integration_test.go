package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/mklfarha/metiche/backend/app/coordination"
	project_types "github.com/mklfarha/metiche/backend/core/module/project/types"
	project_entity "github.com/mklfarha/metiche/backend/entity/project"
	"github.com/mklfarha/metiche/backend/enums"
)

// Putting a repository's work on a team's board is the person's call. These
// run start_session through timeTool, the wrapper that writes the
// "mcp tool failed" line, so they also prove the refusal is not a failure.

type teamWrites struct{ projects, sessions, events, sequence int }

func (hs *harness) writes(t *testing.T) teamWrites {
	t.Helper()
	db := hs.core.DB()
	id := hs.teamID.String()
	return teamWrites{
		projects: countRows(t, db, "SELECT COUNT(*) FROM `project` WHERE `team_uuid` = ?", id),
		sessions: countRows(t, db, "SELECT COUNT(*) FROM `session` WHERE `team_uuid` = ?", id),
		events:   countRows(t, db, "SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", id),
		sequence: countRows(t, db, "SELECT `sequence` FROM `team` WHERE `id` = ?", id),
	}
}

// loggedStart is start_session wrapped exactly as addTool wraps it for timing.
// It returns the result text and whether the result was marked IsError.
func loggedStart(hs *harness) (func(ctx context.Context, args StartSessionParams) (string, bool, error), *observer.ObservedLogs) {
	obs, logs := observer.New(zapcore.DebugLevel)
	wrapped := timeTool(zap.New(obs), "start_session", hs.h.StartSession)
	return func(ctx context.Context, args StartSessionParams) (string, bool, error) {
		res, _, err := wrapped(ctx, nil, args)
		if err != nil {
			return "", false, err
		}
		text := ""
		if res != nil && len(res.Content) > 0 {
			if tc, ok := res.Content[0].(*mcp.TextContent); ok {
				text = tc.Text
			}
		}
		return text, res != nil && res.IsError, nil
	}, logs
}

func assertNoFailureLogged(t *testing.T, logs *observer.ObservedLogs) {
	t.Helper()
	for _, e := range logs.All() {
		if e.Level >= zapcore.WarnLevel {
			t.Errorf("logged at %v: %q %v", e.Level, e.Message, e.ContextMap())
		}
	}
}

func decodeText(t *testing.T, text string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(text), into); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, text)
	}
}

func TestIntegrationRepoBindingAsksBeforeCreatingAProject(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")
	call, logs := loggedStart(hs)
	slug := hs.teamSlug(t)

	args := StartSessionParams{
		RepoURL:        "git@github.com:acme/media.git",
		Branch:         "main",
		Goal:           "cut the trailer",
		IdempotencyKey: "bind-media-1",
	}
	before := hs.writes(t)
	text, isErr, err := call(ana.ctx, args)
	if err != nil || isErr {
		t.Fatalf("an unconfirmed new repository must not fail the call: err=%v isError=%v", err, isErr)
	}
	t.Logf("refusal: %s", text)
	var ref RepoBindingRefusal
	decodeText(t, text, &ref)
	if ref.OK || ref.Code != "confirm_repo_binding" {
		t.Fatalf("ok=%v code=%q, want ok=false code=confirm_repo_binding", ref.OK, ref.Code)
	}
	if ref.TeamSlug != slug || ref.TeamName != "Test team" || ref.Repo != "github.com/acme/media" || ref.ProjectKey != "media" {
		t.Errorf("refusal names team %q/%q repo %q key %q", ref.TeamSlug, ref.TeamName, ref.Repo, ref.ProjectKey)
	}
	if ref.ExistingProjectKeys == nil || len(ref.ExistingProjectKeys) != 0 || !strings.Contains(text, `"existing_project_keys":[]`) {
		t.Errorf("existing_project_keys = %v, want an empty list", ref.ExistingProjectKeys)
	}
	for _, must := range []string{
		"Should work in github.com/acme/media go on team Test team's board, where its members can see it?",
		`confirm_new_project: \"person\"`, ".metiche", `team = ` + slug, `project = media`, "create_team",
	} {
		if !strings.Contains(text, must) {
			t.Errorf("refusal note is missing %q", must)
		}
	}
	if after := hs.writes(t); after != before {
		t.Fatalf("a refused start_session wrote something: before %+v, after %+v", before, after)
	}
	assertNoFailureLogged(t, logs)

	// The person said yes. Same idempotency key: the refusal stored nothing.
	args.ConfirmNewProject = "person"
	first, isErr, err := call(ana.ctx, args)
	if err != nil || isErr {
		t.Fatalf("confirmed start_session: err=%v isError=%v", err, isErr)
	}
	var env Envelope
	decodeText(t, first, &env)
	if !env.OK || env.Key == "" || env.ProjectKey != "media" {
		t.Fatalf("confirmed start did not start a session on media: %s", first)
	}
	if !strings.Contains(env.Note, `project "media" created`) {
		t.Errorf("note does not say the project was created: %s", env.Note)
	}
	afterYes := hs.writes(t)
	if afterYes.projects != 1 || afterYes.sessions != 1 || afterYes.events != before.events+1 {
		t.Fatalf("after yes: %+v (before %+v), want one project, one session, one event", afterYes, before)
	}

	// A replay of the confirmed start is byte-identical and writes nothing.
	replay, _, err := call(ana.ctx, args)
	if err != nil || replay != first {
		t.Fatalf("replay differs (err=%v):\nfirst:  %s\nreplay: %s", err, first, replay)
	}
	// So is the same key without the confirmation: it is a replay, not a new ask.
	args.ConfirmNewProject = ""
	if again, _, err := call(ana.ctx, args); err != nil || again != first {
		t.Fatalf("unconfirmed replay of a started session differs (err=%v):\n%s", err, again)
	}
	if got := hs.writes(t); got != afterYes {
		t.Fatalf("replays wrote something: %+v, want %+v", got, afterYes)
	}

	// Now the repository is bound: another agent, another key for it, no ask.
	bob := hs.join(t, "Bob", "client-b")
	text, _, err = call(bob.ctx, StartSessionParams{ProjectKey: "media-tool", RepoURL: "https://github.com/Acme/media"})
	if err != nil {
		t.Fatal(err)
	}
	decodeText(t, text, &env)
	if !env.OK || env.ProjectKey != "media" {
		t.Fatalf("an existing project matched by repo_url asked again or missed: %s", text)
	}
	assertNoFailureLogged(t, logs)
}

func TestIntegrationRepoBindingExistingProjectsNeverAsk(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")

	// A project created before confirmation existed, by key alone.
	legacy := project_entity.Project{
		ID: uuid.Must(uuid.NewV4()), TeamUUID: hs.teamID, Key: "legacy", Name: "legacy",
		IgnorePatterns: coordination.DefaultIgnorePatterns(), HotspotPatterns: coordination.DefaultHotspotPatterns(),
		CaseInsensitivePaths: true, Cadence: enums.PROJECT_CADENCE_HACKATHON, Status: enums.RECORD_STATUS_ACTIVE,
	}
	if _, err := hs.core.Project().Insert(context.Background(), project_types.UpsertRequest{Project: legacy}); err != nil {
		t.Fatalf("seeding a legacy project: %v", err)
	}
	// No confirmation on either call: startOn would add one, so these do not use it.
	env := startUnconfirmed(t, hs, ana, StartSessionParams{ProjectKey: "legacy", RepoURL: "https://github.com/acme/legacy"})
	if env.ProjectKey != "legacy" {
		t.Fatalf("legacy project by key: landed on %q", env.ProjectKey)
	}
	// Its repo_url was backfilled; a different key for that repository still lands there.
	env = startUnconfirmed(t, hs, ana, StartSessionParams{ProjectKey: "legacy_app", RepoURL: "git@github.com:acme/legacy.git"})
	if env.ProjectKey != "legacy" {
		t.Fatalf("legacy project by repo_url: landed on %q", env.ProjectKey)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `project` WHERE `team_uuid` = ?", hs.teamID.String()); n != 1 {
		t.Errorf("%d projects, want only the legacy one", n)
	}
}

// startUnconfirmed runs start_session exactly as given and fails on an error
// or on a confirm_repo_binding refusal.
func startUnconfirmed(t *testing.T, hs *harness, c *caller, args StartSessionParams) Envelope {
	t.Helper()
	res, _, err := hs.h.StartSession(c.ctx, nil, args)
	if err != nil {
		t.Fatalf("start_session: %v", err)
	}
	var env Envelope
	decodeResult(t, res, &env)
	if !env.OK || env.Key == "" {
		t.Fatalf("start_session asked or refused for an existing project: %s", resultText(t, res))
	}
	return env
}

func TestIntegrationRepoBindingMeticheFileAndInvalidValues(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")
	slug := hs.teamSlug(t)
	before := hs.writes(t)

	for _, bad := range []string{"yes", "true", "metiche"} {
		_, _, err := hs.h.StartSession(ana.ctx, nil, StartSessionParams{RepoURL: "https://github.com/acme/web", ConfirmNewProject: bad})
		if err == nil || !strings.Contains(err.Error(), "confirm_new_project must be") ||
			!strings.Contains(err.Error(), `"person"`) || !strings.Contains(err.Error(), `"metiche_file"`) {
			t.Errorf("confirm_new_project %q: err = %v", bad, err)
		}
	}
	_, _, err := hs.h.StartSession(ana.ctx, nil, StartSessionParams{RepoURL: "https://github.com/acme/web", ConfirmNewProject: "metiche_file"})
	if err == nil || !strings.Contains(err.Error(), "team_slug") {
		t.Errorf("metiche_file without team_slug: err = %v", err)
	}
	if after := hs.writes(t); after != before {
		t.Fatalf("invalid confirmations wrote something: %+v -> %+v", before, after)
	}

	// The refusal lists the team's existing projects.
	startOn(t, hs, ana, StartSessionParams{RepoURL: "https://github.com/acme/api", ConfirmNewProject: "person"})
	res, _, err := hs.h.StartSession(ana.ctx, nil, StartSessionParams{RepoURL: "https://github.com/acme/web"})
	if err != nil {
		t.Fatal(err)
	}
	var ref RepoBindingRefusal
	decodeResult(t, res, &ref)
	if ref.Code != "confirm_repo_binding" || len(ref.ExistingProjectKeys) != 1 || ref.ExistingProjectKeys[0] != "api" {
		t.Errorf("refusal on a team with a project: %s", resultText(t, res))
	}

	env, _ := startOn(t, hs, ana, StartSessionParams{
		TeamSlug: slug, ProjectKey: "web", RepoURL: "https://github.com/acme/web", ConfirmNewProject: "metiche_file",
	})
	if env.ProjectKey != "web" || !strings.Contains(env.Note, `project "web" created`) {
		t.Fatalf("metiche_file confirmation did not create web: %+v", env)
	}
}
