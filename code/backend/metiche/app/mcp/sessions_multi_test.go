package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mklfarha/metiche/backend/enums"
)

// Several terminals of one client share one token, so they are one agent and
// each terminal is a session. These tests pin down what start_session owes a
// second terminal: its own session, a note naming the first, and a ceiling.

// startFor runs start_session and returns the envelope and the raw bytes.
func startFor(t *testing.T, hs *harness, ctx context.Context, args StartSessionParams) (Envelope, string) {
	t.Helper()
	if args.ProjectKey == "" {
		args.ProjectKey = "metiche"
	}
	// These tests are about sessions, not binding: the first one creates the
	// project, and the person confirmed it (repobinding.go).
	if args.ConfirmNewProject == "" {
		args.ConfirmNewProject = "person"
	}
	res, _, err := hs.h.StartSession(ctx, nil, args)
	if err != nil {
		t.Fatalf("start_session %+v: %v", args, err)
	}
	var env Envelope
	decodeResult(t, res, &env)
	return env, resultText(t, res)
}

// setSessionStatus moves a session the way the sweeper would, without waiting
// for it.
func setSessionStatus(t *testing.T, hs *harness, key string, status enums.SessionStatus) {
	t.Helper()
	if _, err := hs.core.DB().Exec(
		"UPDATE `session` SET `status` = ? WHERE `team_uuid` = ? AND `key` = ?",
		status, hs.teamID.String(), key); err != nil {
		t.Fatalf("setting %s to %s: %v", key, status.String(), err)
	}
}

func TestIntegrationSecondTerminalIsSecondSession(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")

	first, _ := startFor(t, hs, ana.ctx, StartSessionParams{Branch: "feat/a", IdempotencyKey: "terminal-1"})
	t.Logf("first terminal note:  %s", first.Note)

	// The first terminal has been running for a while.
	if _, err := hs.core.DB().Exec(
		"UPDATE `session` SET `started_at` = ? WHERE `team_uuid` = ? AND `key` = ?",
		time.Now().UTC().Add(-12*time.Minute-10*time.Second), hs.teamID.String(), first.Key); err != nil {
		t.Fatal(err)
	}

	second, _ := startFor(t, hs, ana.ctx, StartSessionParams{Branch: "feat/b", IdempotencyKey: "terminal-2"})
	t.Logf("second terminal note: %s", second.Note)

	if first.Key == "" || second.Key == "" || first.Key == second.Key {
		t.Fatalf("two terminals got keys %q and %q, want two distinct sessions", first.Key, second.Key)
	}
	if n := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `session` WHERE `agent_uuid` = ? AND `status` = ?",
		ana.agent.ID.String(), enums.SESSION_STATUS_LIVE); n != 2 {
		t.Errorf("%d live sessions for the agent, want 2", n)
	}

	for _, want := range []string{
		"you already have " + first.Key + " live on feat/a (12m ago)",
		"this is " + second.Key,
		"if " + first.Key + " was this terminal's earlier run, end_session it",
	} {
		if !strings.Contains(second.Note, want) {
			t.Errorf("second terminal's note is missing %q:\n%s", want, second.Note)
		}
	}
	for _, unwanted := range []string{"already have", "end_session", "feat/", "this is"} {
		if strings.Contains(first.Note, unwanted) {
			t.Errorf("the first terminal had no other session, but its note says %q:\n%s", unwanted, first.Note)
		}
	}
	// And it still carries the heartbeat instruction a session always gets.
	if !strings.Contains(second.Note, "heartbeat every ~60s") {
		t.Errorf("the multi-session note displaced the heartbeat instruction:\n%s", second.Note)
	}
}

func TestIntegrationLiveSessionCapPerAgent(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")

	keys := make([]string, 0, maxLiveSessionsPerAgent)
	for i := 0; i < maxLiveSessionsPerAgent; i++ {
		env, _ := startFor(t, hs, ana.ctx, StartSessionParams{
			Branch: fmt.Sprintf("feat/%d", i), IdempotencyKey: fmt.Sprintf("term-%d", i)})
		keys = append(keys, env.Key)
	}
	eventsBefore := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team_event`")

	_, _, err := hs.h.StartSession(ana.ctx, nil, StartSessionParams{
		ProjectKey: "metiche", Branch: "feat/ninth", IdempotencyKey: "term-ninth"})
	if err == nil {
		t.Fatalf("a ninth open session was allowed; the cap is %d", maxLiveSessionsPerAgent)
	}
	t.Logf("ninth refused: %v", err)
	for i, k := range keys {
		if !strings.Contains(err.Error(), k+" live on "+fmt.Sprintf("feat/%d", i)) {
			t.Errorf("the refusal does not list %s on feat/%d: %v", k, i, err)
		}
	}
	if !strings.Contains(err.Error(), "end_session") {
		t.Errorf("the refusal does not say how to get unstuck: %v", err)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `session`"); n != maxLiveSessionsPerAgent {
		t.Errorf("%d sessions after the refusal, want %d", n, maxLiveSessionsPerAgent)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team_event`"); n != eventsBefore {
		t.Errorf("the refusal wrote %d event(s)", n-eventsBefore)
	}

	// A refused call is not a stored answer: once there is room, the SAME
	// idempotency key starts the session instead of replaying the refusal.
	if _, _, err := hs.h.EndSession(ana.ctx, nil, EndSessionParams{SessionKey: keys[0]}); err != nil {
		t.Fatal(err)
	}
	ninth, _ := startFor(t, hs, ana.ctx, StartSessionParams{Branch: "feat/ninth", IdempotencyKey: "term-ninth"})
	if ninth.Key == "" {
		t.Fatal("the ninth session has no key")
	}
	if strings.Contains(ninth.Note, keys[0]+" ") {
		t.Errorf("the ninth session's note lists the ended %s:\n%s", keys[0], ninth.Note)
	}
	if !strings.Contains(ninth.Note, "one of those") {
		t.Errorf("with several others open the note should say 'one of those':\n%s", ninth.Note)
	}

	// Another agent of another person is not charged for Ana's terminals.
	bob := hs.join(t, "Bob", "client-b")
	startFor(t, hs, bob.ctx, StartSessionParams{Branch: "feat/bob"})
}

func TestIntegrationLiveSessionCapCountsStaleNotEnded(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")

	keys := make([]string, 0, maxLiveSessionsPerAgent)
	for i := 0; i < maxLiveSessionsPerAgent; i++ {
		env, _ := startFor(t, hs, ana.ctx, StartSessionParams{Branch: fmt.Sprintf("feat/%d", i)})
		keys = append(keys, env.Key)
	}

	// Stale is a terminal that stopped heartbeating but may come back: it
	// still counts.
	setSessionStatus(t, hs, keys[0], enums.SESSION_STATUS_STALE)
	setSessionStatus(t, hs, keys[1], enums.SESSION_STATUS_STALE)
	_, _, err := hs.h.StartSession(ana.ctx, nil, StartSessionParams{ProjectKey: "metiche"})
	if err == nil {
		t.Fatal("stale sessions were not counted toward the cap")
	}
	if !strings.Contains(err.Error(), keys[0]+" stale on feat/0") {
		t.Errorf("the refusal should list the stale session as stale: %v", err)
	}

	// Ended does not count.
	setSessionStatus(t, hs, keys[2], enums.SESSION_STATUS_ENDED)
	env, _ := startFor(t, hs, ana.ctx, StartSessionParams{Branch: "feat/after-ended"})
	t.Logf("after one ended: %s", env.Note)
	if !strings.Contains(env.Note, keys[0]+" stale on feat/0") {
		t.Errorf("the note should name the stale session as stale:\n%s", env.Note)
	}
	if strings.Contains(env.Note, keys[2]+" ") {
		t.Errorf("the note lists the ended session %s:\n%s", keys[2], env.Note)
	}

	// Full again; abandoned does not count either.
	if _, _, err := hs.h.StartSession(ana.ctx, nil, StartSessionParams{ProjectKey: "metiche"}); err == nil {
		t.Fatal("the cap was not enforced once the freed slot was used")
	}
	setSessionStatus(t, hs, keys[3], enums.SESSION_STATUS_ABANDONED)
	startFor(t, hs, ana.ctx, StartSessionParams{Branch: "feat/after-abandoned"})
}

func TestIntegrationStartSessionReplayKeepsTheNote(t *testing.T) {
	hs := newHarness(t)
	ana := hs.join(t, "Ana", "client-a")

	other, _ := startFor(t, hs, ana.ctx, StartSessionParams{Branch: "feat/a", IdempotencyKey: "earlier"})

	args := StartSessionParams{Branch: "feat/b", IdempotencyKey: "retry-this-one"}
	firstEnv, firstRaw := startFor(t, hs, ana.ctx, args)
	if !strings.Contains(firstEnv.Note, other.Key+" live on feat/a") {
		t.Fatalf("the first answer should name %s:\n%s", other.Key, firstEnv.Note)
	}

	// The world changes between the call and its retry: a third terminal opens
	// and the first one ends. A re-rendered note would mention neither the
	// same way; a replay must not notice.
	third, _ := startFor(t, hs, ana.ctx, StartSessionParams{Branch: "feat/c", IdempotencyKey: "later"})
	if _, _, err := hs.h.EndSession(ana.ctx, nil, EndSessionParams{SessionKey: other.Key}); err != nil {
		t.Fatal(err)
	}

	_, replayRaw := startFor(t, hs, ana.ctx, args)
	if replayRaw != firstRaw {
		t.Errorf("the replay answered differently:\nfirst:  %s\nreplay: %s", firstRaw, replayRaw)
	}
	t.Logf("both calls returned: %s", firstRaw)
	if strings.Contains(replayRaw, third.Key+" ") {
		t.Errorf("the replay mentions %s, which started after the original call", third.Key)
	}

	if n := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `session` WHERE `team_uuid` = ? AND `branch` = ?", hs.teamID.String(), "feat/b"); n != 1 {
		t.Errorf("%d sessions for the retried call, want 1", n)
	}
	if n := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? AND `idempotency_key` LIKE ?",
		hs.teamID.String(), enums.EVENT_KIND_SESSION_STARTED, "%:retry-this-one"); n != 1 {
		t.Errorf("%d session_started events for the retried call, want 1", n)
	}
}
