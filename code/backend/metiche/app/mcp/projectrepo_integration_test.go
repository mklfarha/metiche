package mcp

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/mklfarha/metiche/backend/enums"
)

// A project is a repository, not a name a model picked. These run against a
// real MySQL (see integration_test.go for how to point them at one), because
// what they prove — that two agents in one repository land on one project
// and therefore collide — is detection running under the team row lock.

// startOn runs start_session with exactly these arguments and fails the test
// on an error. Unlike startFor it supplies no default project_key: whether
// one was sent is the thing under test here. It does confirm a new project,
// as a person would (repobinding_integration_test.go tests the ask itself).
func startOn(t *testing.T, hs *harness, c *caller, args StartSessionParams) (Envelope, string) {
	t.Helper()
	if args.ConfirmNewProject == "" {
		args.ConfirmNewProject = "person"
	}
	res, _, err := hs.h.StartSession(c.ctx, nil, args)
	if err != nil {
		t.Fatalf("start_session(key=%q): %v", args.ProjectKey, err)
	}
	var env Envelope
	decodeResult(t, res, &env)
	if env.Key == "" {
		t.Fatalf("start_session returned no session key: %s", resultText(t, res))
	}
	return env, resultText(t, res)
}

// projectOf is the project a session landed on: its uuid, key and repo_url.
func projectOf(t *testing.T, hs *harness, sessionKey string) (id, key string, repoURL sql.NullString) {
	t.Helper()
	if err := hs.core.DB().QueryRow(
		"SELECT p.`id`, p.`key`, p.`repo_url` FROM `session` s JOIN `project` p ON p.`id` = s.`project_uuid` "+
			"WHERE s.`team_uuid` = ? AND s.`key` = ?", hs.teamID.String(), sessionKey).
		Scan(&id, &key, &repoURL); err != nil {
		t.Fatalf("reading the project of %s: %v", sessionKey, err)
	}
	return id, key, repoURL
}

// startedSummary is the summary on an agent's latest session_started event,
// the line the board's timeline shows, and that agent's label.
func startedSummary(t *testing.T, hs *harness, agentID string) (summary, label string) {
	t.Helper()
	var s sql.NullString
	if err := hs.core.DB().QueryRow(
		"SELECT e.`summary`, a.`label` FROM `team_event` e JOIN `agent` a ON a.`id` = e.`agent_uuid` "+
			"WHERE e.`team_uuid` = ? AND e.`kind` = ? AND e.`agent_uuid` = ? ORDER BY e.`sequence` DESC LIMIT 1",
		hs.teamID.String(), enums.EVENT_KIND_SESSION_STARTED, agentID).Scan(&s, &label); err != nil {
		t.Fatalf("reading the session_started summary of agent %s: %v", agentID, err)
	}
	return s.String, label
}

// TestIntegrationSameRepoDifferentKeysSameProject is the production incident.
// One person, two agents: Claude Code started above the git root with no
// branch and called the project "taqueria_tracker"; Codex called it
// "taqueria" and reported the ssh remote. Both claimed app/rest.go. Before
// the fix that was two projects and no conflict.
func TestIntegrationSameRepoDifferentKeysSameProject(t *testing.T) {
	hs := newHarness(t)
	withDetector(t, hs)

	claude := hs.join(t, "Maykel", "claude-code")
	codex := hs.rejoin(t, claude, "Maykel", "codex")
	if codex.account.ID != claude.account.ID || codex.agent.ID == claude.agent.ID {
		t.Fatalf("want two agents of one account: accounts equal=%v, agents equal=%v",
			codex.account.ID == claude.account.ID, codex.agent.ID == claude.agent.ID)
	}

	a, _ := startOn(t, hs, claude, StartSessionParams{
		ProjectKey: "taqueria_tracker",
		RepoURL:    "https://github.com/mklfarha/taqueria.git",
		Goal:       "wire the tracker endpoints",
	})
	b, _ := startOn(t, hs, codex, StartSessionParams{
		ProjectKey: "taqueria",
		RepoURL:    "git@github.com:mklfarha/taqueria.git",
		Branch:     "main",
		Goal:       "tidy the REST handlers",
	})
	t.Logf("claude note: %s", a.Note)
	t.Logf("codex note:  %s", b.Note)

	projA, keyA, repoA := projectOf(t, hs, a.Key)
	projB, _, _ := projectOf(t, hs, b.Key)
	if projA != projB {
		t.Fatalf("one repository, two projects: %s on %s, %s on %s", a.Key, projA, b.Key, projB)
	}
	t.Logf("both sessions on project %s (key %q, repo_url %q)", projA, keyA, repoA.String)
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `project` WHERE `team_uuid` = ?", hs.teamID.String()); n != 1 {
		t.Errorf("%d project rows, want 1", n)
	}
	if repoA.String != "https://github.com/mklfarha/taqueria" {
		t.Errorf("stored repo_url = %q, want the canonical https://github.com/mklfarha/taqueria", repoA.String)
	}
	if strings.Contains(b.Note, "created") {
		t.Errorf("the second agent was told a project was created: %s", b.Note)
	}

	// Both responses name the project the session is on, and the agent whose
	// key was not used is told so, and told what to send instead.
	if a.ProjectKey != keyA || b.ProjectKey != keyA {
		t.Errorf("response project_key: claude %q, codex %q, want both %q", a.ProjectKey, b.ProjectKey, keyA)
	}
	if strings.Contains(a.Note, "you sent project_key") {
		t.Errorf("the agent whose key was used was told it was overridden: %s", a.Note)
	}
	wantOverride := `you sent project_key "taqueria"; this repository is project "taqueria_tracker" — use that key (or send only repo_url); `
	if !strings.HasPrefix(b.Note, wantOverride) {
		t.Errorf("codex note does not start with the override:\n got: %s\nwant: %s...", b.Note, wantOverride)
	}

	// And the board's timeline names that project, not the key codex sent.
	for _, c := range []*caller{claude, codex} {
		summary, label := startedSummary(t, hs, c.agent.ID.String())
		if want := label + " started work on taqueria_tracker"; summary != want {
			t.Errorf("session_started summary = %q, want %q", summary, want)
		}
		t.Logf("timeline: %s", summary)
	}

	first := declare(t, hs, claude, DeclareIntentParams{
		SessionKey: a.Key,
		Summary:    "add the tracker routes to the REST router",
		Paths:      []string{"app/rest.go"},
		Mode:       "write",
	})
	if len(first.Conflicts) != 0 {
		t.Fatalf("the first claim on app/rest.go reported %d conflict(s)", len(first.Conflicts))
	}
	second := declare(t, hs, codex, DeclareIntentParams{
		SessionKey: b.Key,
		Summary:    "reorder the handlers in the REST router",
		Paths:      []string{"app/rest.go"},
		Mode:       "write",
	})

	// The conflict row, on the one project both sessions share.
	if n := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `conflict` WHERE `team_uuid` = ? AND `project_uuid` = ?",
		hs.teamID.String(), projA); n != 1 {
		t.Fatalf("%d conflict rows on the shared project, want exactly 1", n)
	}
	var conflictID string
	var severity int64
	if err := hs.core.DB().QueryRow(
		"SELECT `id`, `severity` FROM `conflict` WHERE `team_uuid` = ?", hs.teamID.String()).
		Scan(&conflictID, &severity); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `conflict_participant` WHERE `conflict_uuid` = ?", conflictID); n != 2 {
		t.Errorf("%d conflict participants, want 2", n)
	}

	// And the later claimant is told in its own response.
	if len(second.Conflicts) != 1 {
		t.Fatalf("codex was told about %d conflict(s), want 1: %+v", len(second.Conflicts), second.Conflicts)
	}
	notice := second.Conflicts[0]
	if notice.Kind != "path_overlap" || len(notice.Paths) != 1 || notice.Paths[0] != "app/rest.go" {
		t.Errorf("notice = %+v, want a path_overlap on app/rest.go", notice)
	}
	t.Logf("codex was told: [%s %s] with %s on %v — %s (stored severity %d)",
		notice.Key, notice.Severity, notice.With, notice.Paths, notice.SuggestedAction, severity)
}

// TestIntegrationProjectRepoURLBackfill: a project made by key alone, before
// anyone sent its remote, learns the remote from the first session that
// does — normalized — and from then on the remote finds it under any key.
func TestIntegrationProjectRepoURLBackfill(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")

	s1, _ := startOn(t, hs, ana, StartSessionParams{ProjectKey: "shop"})
	proj1, _, repo1 := projectOf(t, hs, s1.Key)
	if repo1.Valid {
		t.Fatalf("a project created with no repo_url stored %q", repo1.String)
	}

	s2, _ := startOn(t, hs, ana, StartSessionParams{
		ProjectKey: "shop", RepoURL: "https://GitHub.com/Acme/Shop.git/"})
	proj2, _, repo2 := projectOf(t, hs, s2.Key)
	if proj2 != proj1 {
		t.Fatalf("the same key with a repo_url landed on a different project: %s vs %s", proj2, proj1)
	}
	if repo2.String != "https://github.com/acme/shop" {
		t.Errorf("backfilled repo_url = %q (valid=%v), want https://github.com/acme/shop", repo2.String, repo2.Valid)
	}
	t.Logf("backfilled repo_url: %q", repo2.String)
	if strings.Contains(s2.Note, "created") {
		t.Errorf("a backfill was reported as a creation: %s", s2.Note)
	}

	// Now the remote is the identity: another agent with a guessed key and
	// the ssh spelling still lands here.
	bob := hs.join(t, "Bob", "client-b")
	s3, _ := startOn(t, hs, bob, StartSessionParams{
		ProjectKey: "shop_frontend", RepoURL: "git@github.com:acme/shop.git"})
	if proj3, _, _ := projectOf(t, hs, s3.Key); proj3 != proj1 {
		t.Errorf("after the backfill the remote did not find the project: %s vs %s", proj3, proj1)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `project`"); n != 1 {
		t.Errorf("%d projects, want 1", n)
	}
}

// TestIntegrationProjectKeyTakenByAnotherRepo: a key that already names a
// different repository is refused, not merged, and the refusal writes nothing.
func TestIntegrationProjectKeyTakenByAnotherRepo(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")
	startOn(t, hs, ana, StartSessionParams{ProjectKey: "shop", RepoURL: "https://github.com/acme/shop.git"})

	db := hs.core.DB()
	counts := func() [4]int {
		return [4]int{
			countRows(t, db, "SELECT COUNT(*) FROM `project`"),
			countRows(t, db, "SELECT COUNT(*) FROM `session`"),
			countRows(t, db, "SELECT COUNT(*) FROM `team_event`"),
			countRows(t, db, "SELECT COUNT(*) FROM `project` WHERE `repo_url` = ?", "https://github.com/acme/shop"),
		}
	}
	const secret = "notarealsecret"
	bob := hs.join(t, "Bob", "client-b")
	before := counts() // after join_team, which writes its own event
	_, _, err := hs.h.StartSession(bob.ctx, nil, StartSessionParams{
		ProjectKey:     "shop",
		RepoURL:        "https://someuser:" + secret + "@github.com/other/shop.git",
		IdempotencyKey: "taken-1",
	})
	if err == nil {
		t.Fatal("a key belonging to another repository was accepted")
	}
	t.Logf("explicit key refused: %v", err)
	for _, want := range []string{`project_key "shop" is already taken by a different repository`,
		"github.com/acme/shop", "github.com/other/shop", "git rev-parse --show-toplevel", "omit project_key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is missing %q: %v", want, err)
		}
	}
	for _, leaked := range []string{secret, "someuser"} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("the refusal repeats credential material %q", leaked)
		}
	}
	if after := counts(); after != before {
		t.Errorf("the refusal wrote something: before %v, after %v", before, after)
	}

	// The same refusal when the key was derived from repo_url.
	_, _, err = hs.h.StartSession(bob.ctx, nil, StartSessionParams{RepoURL: "git@github.com:other/shop.git"})
	if err == nil {
		t.Fatal("a derived key belonging to another repository was accepted")
	}
	t.Logf("derived key refused: %v", err)
	if !strings.Contains(err.Error(), `derived from repo_url github.com/other/shop is already taken`) ||
		!strings.Contains(err.Error(), "pass project_key explicitly") {
		t.Errorf("the derived-key refusal does not say what to do: %v", err)
	}
	if after := counts(); after != before {
		t.Errorf("the derived-key refusal wrote something: before %v, after %v", before, after)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `project` WHERE `repo_url` LIKE ?", "%"+secret+"%"); n != 0 {
		t.Errorf("a credential reached the project table")
	}
}

// TestIntegrationProjectKeyDerivedFromRepoURL: repo_url alone is enough, and
// with neither there is still nothing to scope a claim to.
func TestIntegrationProjectKeyDerivedFromRepoURL(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")

	env, _ := startOn(t, hs, ana, StartSessionParams{RepoURL: "git@github.com:Acme/Inventory.git"})
	_, key, repo := projectOf(t, hs, env.Key)
	if key != "inventory" || repo.String != "https://github.com/acme/inventory" {
		t.Errorf("derived project key=%q repo_url=%q, want inventory and https://github.com/acme/inventory", key, repo.String)
	}
	if !strings.Contains(env.Note, `project "inventory" created`) {
		t.Errorf("note = %q, want it to say the derived project was created", env.Note)
	}
	t.Logf("derived: key=%q repo_url=%q note=%s", key, repo.String, env.Note)

	_, _, err := hs.h.StartSession(ana.ctx, nil, StartSessionParams{Goal: "no repository at all"})
	if err == nil || !strings.Contains(err.Error(), "project_key is required") {
		t.Fatalf("with neither project_key nor repo_url: err = %v, want project_key is required", err)
	}
	t.Logf("neither: %v", err)

	// A local path used as a remote has no URL form (repo_url is a url
	// column), so it identifies nothing: with a key the key decides and
	// nothing is stored; without one it is the same as sending neither.
	local, _ := startOn(t, hs, ana, StartSessionParams{ProjectKey: "scratch", RepoURL: "/Users/me/src/scratch/.git"})
	if _, key, repo := projectOf(t, hs, local.Key); key != "scratch" || repo.Valid {
		t.Errorf("local remote: key=%q repo_url=%q (valid=%v), want scratch and NULL", key, repo.String, repo.Valid)
	}
	if _, _, err := hs.h.StartSession(ana.ctx, nil, StartSessionParams{RepoURL: "/Users/me/src/scratch/.git"}); err == nil ||
		!strings.Contains(err.Error(), "project_key is required") {
		t.Errorf("a local remote with no key: err = %v, want project_key is required", err)
	}
}

// TestIntegrationNewProjectNoteListsExisting: creating a project on a team
// that already has some is the moment a guessed key does damage, so the note
// names the projects that exist.
func TestIntegrationNewProjectNoteListsExisting(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")

	first, _ := startOn(t, hs, ana, StartSessionParams{ProjectKey: "shop", RepoURL: "https://github.com/acme/shop"})
	if strings.Contains(first.Note, "already has") {
		t.Errorf("the team's first project listed others: %s", first.Note)
	}
	startOn(t, hs, ana, StartSessionParams{ProjectKey: "api", RepoURL: "https://github.com/acme/api"})
	third, _ := startOn(t, hs, ana, StartSessionParams{ProjectKey: "shop_tracker"})
	t.Logf("third note: %s", third.Note)

	for _, want := range []string{
		`project "shop_tracker" created; this team already has project(s) "`,
		`"shop"`, `"api"`,
		"end_session " + third.Key,
		"repo_url (git remote get-url origin)",
		"heartbeat every ~60s",
	} {
		if !strings.Contains(third.Note, want) {
			t.Errorf("the note is missing %q:\n%s", want, third.Note)
		}
	}
}

// TestIntegrationStartSessionRepoReplayIsByteIdentical: the project a replay
// reports, and the note about the team's other projects, are the world as it
// was when the session started — even after the world moved.
func TestIntegrationStartSessionRepoReplayIsByteIdentical(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")
	startOn(t, hs, ana, StartSessionParams{ProjectKey: "api", RepoURL: "https://github.com/acme/api"})

	args := StartSessionParams{
		ProjectKey:     "taqueria_tracker",
		RepoURL:        "https://github.com/mklfarha/taqueria.git",
		Goal:           "replay me",
		IdempotencyKey: "repo-replay-1",
	}
	firstEnv, firstBytes := startOn(t, hs, ana, args)

	// Move the world: another project appears, and another agent joins the
	// same repository under a different key.
	bob := hs.join(t, "Bob", "client-b")
	startOn(t, hs, bob, StartSessionParams{ProjectKey: "web"})
	startOn(t, hs, bob, StartSessionParams{ProjectKey: "taqueria", RepoURL: "git@github.com:mklfarha/taqueria.git"})
	sessionsBefore := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `session`")

	_, replayBytes := startOn(t, hs, ana, args)
	t.Logf("first:  %s", firstBytes)
	t.Logf("replay: %s", replayBytes)
	if replayBytes != firstBytes {
		t.Fatalf("the replay differs from the original:\nfirst:  %s\nreplay: %s", firstBytes, replayBytes)
	}
	if !strings.Contains(firstEnv.Note, `this team already has project(s) "api"`) {
		t.Errorf("the original note did not list the existing project: %s", firstEnv.Note)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `session`"); n != sessionsBefore {
		t.Errorf("the replay started a session: %d -> %d", sessionsBefore, n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `session` WHERE `status` = ?", enums.SESSION_STATUS_LIVE); n != sessionsBefore {
		t.Errorf("%d live sessions, want %d", n, sessionsBefore)
	}
}

// TestIntegrationStartSessionKeyOverrideReplayIsByteIdentical: the resolved
// project_key and the override note are part of the stored response, so a
// retry repeats them exactly — even after the project was renamed — and the
// retry neither writes an event nor touches the one already written.
func TestIntegrationStartSessionKeyOverrideReplayIsByteIdentical(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")
	bob := hs.join(t, "Bob", "client-b")
	startOn(t, hs, ana, StartSessionParams{ProjectKey: "taqueria", RepoURL: "git@github.com:mklfarha/taqueria.git"})

	args := StartSessionParams{
		ProjectKey:     "taqueria_tracker",
		RepoURL:        "https://github.com/mklfarha/taqueria.git",
		IdempotencyKey: "override-replay-1",
	}
	firstEnv, firstBytes := startOn(t, hs, bob, args)
	for _, want := range []string{
		`"project_key":"taqueria"`,
		`you sent project_key \"taqueria_tracker\"; this repository is project \"taqueria\" — use that key (or send only repo_url)`,
	} {
		if !strings.Contains(firstBytes, want) {
			t.Errorf("the original response is missing %s:\n%s", want, firstBytes)
		}
	}
	summaryBefore, _ := startedSummary(t, hs, bob.agent.ID.String())
	if !strings.HasSuffix(summaryBefore, " started work on taqueria") {
		t.Errorf("summary = %q, want it to name the resolved project taqueria", summaryBefore)
	}

	// Move the world: the project is renamed underneath the stored answer.
	if _, err := hs.core.DB().Exec("UPDATE `project` SET `key` = ? WHERE `team_uuid` = ? AND `key` = ?",
		"taqueria_renamed", hs.teamID.String(), "taqueria"); err != nil {
		t.Fatal(err)
	}
	events := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team_event`")
	sessions := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `session`")

	replayEnv, replayBytes := startOn(t, hs, bob, args)
	t.Logf("first:  %s", firstBytes)
	t.Logf("replay: %s", replayBytes)
	if replayBytes != firstBytes {
		t.Fatalf("the replay differs from the original:\nfirst:  %s\nreplay: %s", firstBytes, replayBytes)
	}
	if replayEnv.ProjectKey != "taqueria" || replayEnv.Key != firstEnv.Key {
		t.Errorf("replay project_key=%q key=%q, want taqueria and %s", replayEnv.ProjectKey, replayEnv.Key, firstEnv.Key)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team_event`"); n != events {
		t.Errorf("the replay wrote an event: %d -> %d", events, n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `session`"); n != sessions {
		t.Errorf("the replay started a session: %d -> %d", sessions, n)
	}
	if after, _ := startedSummary(t, hs, bob.agent.ID.String()); after != summaryBefore {
		t.Errorf("the replay changed the stored summary: %q -> %q", summaryBefore, after)
	}
}

// TestIntegrationStartSessionReplaysEventStoredBeforeResolvedSummary: an event
// written the way commit wrote every event before Apply could set the summary
// — Mutation.Summary naming the key as sent, and a stored response with no
// project_key — still replays byte-for-byte, keeps its summary, and writes
// nothing. It also pins commit's fallback: an Apply that leaves tc.Summary
// empty (every tool but start_session) stores Mutation.Summary unchanged.
func TestIntegrationStartSessionReplaysEventStoredBeforeResolvedSummary(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")
	bob := hs.join(t, "Bob", "client-b")
	startOn(t, hs, ana, StartSessionParams{ProjectKey: "taqueria", RepoURL: "https://github.com/mklfarha/taqueria"})

	legacySummary := bob.agent.Label + " started work on taqueria_tracker"
	stored, err := hs.h.commit(bob.ctx, Mutation{
		TeamUUID:       hs.teamID,
		IdempotencyKey: "session_started:" + bob.agent.ID.String() + ":legacy-1",
		Kind:           enums.EVENT_KIND_SESSION_STARTED,
		Structural:     true,
		AgentUUID:      uuidPtr(bob.agent.ID),
		MemberUUID:     uuidPtr(bob.member.ID),
		SubjectKind:    enums.SUBJECT_KIND_SESSION,
		Summary:        legacySummary,
		Apply: func(_ context.Context, tc *TxContext, env *Envelope) error {
			env.Key = tc.Key("S")
			env.Note = "heartbeat every ~60s with this session_key, or your claims lapse"
			return nil
		},
	})
	if err != nil {
		t.Fatalf("writing the legacy-shaped event: %v", err)
	}
	if strings.Contains(string(stored), "project_key") {
		t.Fatalf("the legacy-shaped response already has project_key: %s", stored)
	}
	if got, _ := startedSummary(t, hs, bob.agent.ID.String()); got != legacySummary {
		t.Fatalf("an Apply that set no tc.Summary stored %q, want Mutation.Summary %q", got, legacySummary)
	}
	events := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team_event`")
	sessions := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `session`")

	// The retry resolves to a different project and would build a different
	// answer and summary — but it is a replay, so it must build neither.
	_, replayBytes := startOn(t, hs, bob, StartSessionParams{
		ProjectKey:     "taqueria_tracker",
		RepoURL:        "git@github.com:mklfarha/taqueria.git",
		IdempotencyKey: "legacy-1",
	})
	t.Logf("stored: %s", stored)
	t.Logf("replay: %s", replayBytes)
	if replayBytes != string(stored) {
		t.Fatalf("the replay of a stored event differs:\nstored: %s\nreplay: %s", stored, replayBytes)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team_event`"); n != events {
		t.Errorf("the replay wrote an event: %d -> %d", events, n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `session`"); n != sessions {
		t.Errorf("the replay started a session: %d -> %d", sessions, n)
	}
	if got, _ := startedSummary(t, hs, bob.agent.ID.String()); got != legacySummary {
		t.Errorf("the replay rewrote the stored summary: %q -> %q", legacySummary, got)
	}
}
