package mcp

import (
	"database/sql"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/app/coordination"
	"github.com/mklfarha/metiche/backend/enums"
)

// These replay, through the real tool handlers and a real MySQL, the case that
// motivated conflictresolve.go: two agents of one person in the same file, a
// report_back saying how they would settle it, one of them moving its code to
// a new file, and both finishing. The conflict has to end resolved, with an
// explanation built from what actually happened.
//
//	METICHE_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/metiche_test?parseTime=true&interpolateParams=true' \
//	  go test -p 1 ./app/mcp/ -run 'Settl|Overlap' -v

const salsaNote = "Different feature, same file. Moving my salsa handler into new app/salsas_picosas.go; " +
	"rest.go keeps only my 2 route lines. Releasing rest.go within a few minutes; register your route after that."

type settleCase struct {
	claude, codex       *caller
	claudeKey, codexKey string
	conflictKey         string
	conflictID          string
	noticeKey           string
}

type conflictState struct {
	ID         string
	Status     enums.ConflictStatus
	Resolution enums.ConflictResolution
	Note       string
	ResolvedAt sql.NullTime
}

func conflictByKey(t *testing.T, hs *harness, key string) conflictState {
	t.Helper()
	var (
		cs          conflictState
		status, res int64
		note        sql.NullString
	)
	if err := hs.core.DB().QueryRow(
		"SELECT `id`, `status`, COALESCE(`resolution`, 0), `resolution_note`, `resolved_at` FROM `conflict` "+
			"WHERE `team_uuid` = ? AND `key` = ?", hs.teamID.String(), key).
		Scan(&cs.ID, &status, &res, &note, &cs.ResolvedAt); err != nil {
		t.Fatalf("reading conflict %s: %v", key, err)
	}
	cs.Status, cs.Resolution, cs.Note = enums.ConflictStatus(status), enums.ConflictResolution(res), note.String
	return cs
}

func setAgentLabel(t *testing.T, hs *harness, c *caller, label string) {
	t.Helper()
	if _, err := hs.core.DB().Exec("UPDATE `agent` SET `label` = ? WHERE `id` = ?", label, c.agent.ID.String()); err != nil {
		t.Fatalf("labelling agent: %v", err)
	}
}

func settleUpdate(t *testing.T, hs *harness, c *caller, args UpdateIntentParams) (Envelope, string) {
	t.Helper()
	res, _, err := hs.h.UpdateIntent(c.ctx, nil, args)
	if err != nil {
		t.Fatalf("update_intent(%+v): %v", args, err)
	}
	var env Envelope
	decodeResult(t, res, &env)
	raw := resultText(t, res)
	t.Logf("update_intent -> %s", raw)
	return env, raw
}

func settleEnd(t *testing.T, hs *harness, c *caller, key, note string) {
	t.Helper()
	res, _, err := hs.h.EndSession(c.ctx, nil, EndSessionParams{SessionKey: key, Outcome: "succeeded", Note: note})
	if err != nil {
		t.Fatalf("end_session(%s): %v", key, err)
	}
	t.Logf("end_session(%s) -> %s", key, resultText(t, res))
}

func resolvedEvents(t *testing.T, hs *harness, conflictKey string) int {
	t.Helper()
	return countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? AND `subject_key` = ?",
		hs.teamID.String(), int64(enums.EVENT_KIND_CONFLICT_RESOLVED), conflictKey)
}

// assertGaplessLog checks the invariant the derived event must not break:
// sequences 1..N with no hole, and team.sequence pointing at N.
func assertGaplessLog(t *testing.T, hs *harness) {
	t.Helper()
	var n, maxSeq, teamSeq int64
	if err := hs.core.DB().QueryRow(
		"SELECT COUNT(*), COALESCE(MAX(`sequence`), 0) FROM `team_event` WHERE `team_uuid` = ?", hs.teamID.String()).
		Scan(&n, &maxSeq); err != nil {
		t.Fatal(err)
	}
	if err := hs.core.DB().QueryRow("SELECT `sequence` FROM `team` WHERE `id` = ?", hs.teamID.String()).Scan(&teamSeq); err != nil {
		t.Fatal(err)
	}
	if n != maxSeq || teamSeq != maxSeq {
		t.Fatalf("event log is not gapless: %d events, max sequence %d, team.sequence %d", n, maxSeq, teamSeq)
	}
}

// collideOnRest replays the production case up to the report_back: codex's
// session starts first, claude takes app/rest.go, codex takes it later and
// raises the conflict, claude collects the notice and answers it.
func collideOnRest(t *testing.T, hs *harness) settleCase {
	t.Helper()
	withDetector(t, hs)

	// One person, two agents: the same-person, two-agent case.
	claude := hs.join(t, "Ana", "client-claude")
	codex := hs.rejoin(t, claude, "Ana", "client-codex")
	setAgentLabel(t, hs, claude, "claude")
	setAgentLabel(t, hs, codex, "codex")
	sc := settleCase{claude: claude, codex: codex}

	sc.codexKey = startWork(t, hs, codex, "main", "tortilla endpoints")
	sc.claudeKey = startWork(t, hs, claude, "main", "salsa endpoints")

	declare(t, hs, claude, DeclareIntentParams{
		SessionKey: sc.claudeKey,
		Summary:    "add the salsa handler and register its route",
		Paths:      []string{"app/rest.go"},
		Mode:       "write",
	})
	second := declare(t, hs, codex, DeclareIntentParams{
		SessionKey: sc.codexKey,
		Summary:    "register the tortilla route",
		Paths:      []string{"app/rest.go"},
		Mode:       "write",
	})
	if len(second.Conflicts) != 1 {
		t.Fatalf("the later declarer was not told about the collision: %+v", second.Conflicts)
	}
	sc.conflictKey = second.Conflicts[0].Key
	if !strings.Contains(second.Conflicts[0].SuggestedAction, "settle it between you") {
		t.Errorf("suggested action does not tell the agents to settle it: %q", second.Conflicts[0].SuggestedAction)
	}
	cs := conflictByKey(t, hs, sc.conflictKey)
	if cs.Status != enums.CONFLICT_STATUS_OPEN {
		t.Fatalf("fresh conflict is %v, want open", cs.Status)
	}
	sc.conflictID = cs.ID

	inbox, raw := getInstructions(t, hs, claude, GetInstructionsParams{SessionKey: sc.claudeKey})
	if len(inbox.Instructions) != 1 || inbox.Instructions[0].Ref != sc.conflictKey {
		t.Fatalf("claude did not get the notice for %s: %s", sc.conflictKey, raw)
	}
	sc.noticeKey = inbox.Instructions[0].Key

	res, _, err := hs.h.ReportBack(claude.ctx, nil, ReportBackParams{
		SessionKey:     sc.claudeKey,
		InstructionKey: sc.noticeKey,
		Outcome:        "acknowledged",
		Note:           salsaNote,
	})
	if err != nil {
		t.Fatalf("report_back: %v", err)
	}
	t.Logf("report_back -> %s", resultText(t, res))

	// Agreeing is not settling: both still hold rest.go.
	if got := conflictByKey(t, hs, sc.conflictKey); got.Status != enums.CONFLICT_STATUS_OPEN {
		t.Fatalf("a report_back alone closed the conflict (%v); only a cleared overlap may", got.Status)
	}
	return sc
}

// TestIntegrationAgentsSettleACollision is test 1: the production case,
// replayed exactly, ends resolved with a truthful resolution and a note that
// quotes the report_back and names the release.
func TestIntegrationAgentsSettleACollision(t *testing.T) {
	hs := newHarness(t)
	sc := collideOnRest(t, hs)

	// Claude moves its handler into a new file and releases rest.go.
	moved, _ := settleUpdate(t, hs, sc.claude, UpdateIntentParams{
		SessionKey:     sc.claudeKey,
		AddPaths:       []string{"app/salsas_picosas.go"},
		DropPaths:      []string{"app/rest.go"},
		StatusLine:     "moving the salsa handler into app/salsas_picosas.go",
		IdempotencyKey: "move-salsas",
	})

	cs := conflictByKey(t, hs, sc.conflictKey)
	if cs.Status != enums.CONFLICT_STATUS_RESOLVED {
		t.Fatalf("after claude released rest.go the conflict is %v, want resolved", cs.Status)
	}
	if cs.Resolution != enums.CONFLICT_RESOLUTION_COORDINATED {
		t.Errorf("resolution = %v, want coordinated (a report_back agreed it, then the overlap cleared)", cs.Resolution)
	}
	if !cs.ResolvedAt.Valid {
		t.Error("resolved_at is not set")
	}
	for _, want := range []string{
		"Settled by the agents:",
		"(claude) released app/rest.go",
		"kept working in app/salsas_picosas.go",
		"(codex) still holds app/rest.go",
		"said: \"Different feature, same file.",
	} {
		if !strings.Contains(cs.Note, want) {
			t.Errorf("resolution_note is missing %q:\n%s", want, cs.Note)
		}
	}
	if n := utf8.RuneCountInString(cs.Note); n > resolutionNoteChars {
		t.Errorf("resolution_note is %d characters; the column holds %d", n, resolutionNoteChars)
	}

	// The board is told with the event it already understands, structural so
	// it repaints, and the log stays gapless around the derived event.
	if n := resolvedEvents(t, hs, sc.conflictKey); n != 1 {
		t.Fatalf("%d conflict_resolved events for %s, want 1", n, sc.conflictKey)
	}
	var seq int64
	var structural bool
	if err := hs.core.DB().QueryRow(
		"SELECT `sequence`, `structural` FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? AND `subject_key` = ?",
		hs.teamID.String(), int64(enums.EVENT_KIND_CONFLICT_RESOLVED), sc.conflictKey).Scan(&seq, &structural); err != nil {
		t.Fatal(err)
	}
	if !structural {
		t.Error("conflict_resolved is not structural; the board would not repaint")
	}
	if moved.Sequence != seq+1 {
		t.Errorf("update_intent answered sequence %d, conflict_resolved is %d; want the call's own event right after it", moved.Sequence, seq)
	}
	assertGaplessLog(t, hs)

	// Nobody is pending on a settled conflict any more.
	hb, _, err := hs.h.Heartbeat(sc.codex.ctx, nil, HeartbeatParams{SessionKey: sc.codexKey})
	if err != nil {
		t.Fatal(err)
	}
	var beat Envelope
	decodeResult(t, hb, &beat)
	if beat.Pending.Conflicts != 0 {
		t.Errorf("codex still has %d pending conflict(s) after it was settled", beat.Pending.Conflicts)
	}

	// Both finish and end, as in production. The settled conflict stays
	// settled, is not re-announced, and keeps the note it was closed with.
	settleUpdate(t, hs, sc.codex, UpdateIntentParams{SessionKey: sc.codexKey, Status: "done"})
	settleUpdate(t, hs, sc.claude, UpdateIntentParams{SessionKey: sc.claudeKey, Status: "done"})
	settleEnd(t, hs, sc.codex, sc.codexKey, "tortilla route registered")
	settleEnd(t, hs, sc.claude, sc.claudeKey, "salsa handler moved to its own file")

	final := conflictByKey(t, hs, sc.conflictKey)
	if final.Status != enums.CONFLICT_STATUS_RESOLVED || final.Resolution != enums.CONFLICT_RESOLUTION_COORDINATED {
		t.Errorf("after both ended the conflict is %v/%v, want resolved/coordinated", final.Status, final.Resolution)
	}
	if final.Note != cs.Note {
		t.Errorf("the note changed after the conflict was settled:\nbefore %s\nafter  %s", cs.Note, final.Note)
	}
	if n := resolvedEvents(t, hs, sc.conflictKey); n != 1 {
		t.Errorf("%d conflict_resolved events after both ended, want still 1", n)
	}
	assertGaplessLog(t, hs)

	// The bookkeeping that stayed NULL in production: both sides were told.
	if n := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `conflict_participant` WHERE `conflict_uuid` = ? AND `notified_at` IS NOT NULL",
		sc.conflictID); n != 2 {
		t.Errorf("%d participant(s) have notified_at set, want both", n)
	}
	if n := countRows(t, hs.core.DB(),
		"SELECT COUNT(*) FROM `conflict_participant` WHERE `conflict_uuid` = ? AND `acked_at` IS NOT NULL",
		sc.conflictID); n != 1 {
		t.Errorf("%d participant(s) have acked_at set, want the one that answered", n)
	}
	if strings.Contains(final.Note, "mtk_") {
		t.Error("the resolution note carries a token")
	}
	t.Logf("resolution_note (%s): %s", final.Resolution.String(), final.Note)
}

// TestIntegrationOverlapStillHeldStaysOpen is test 2: agreement in a note and
// releases elsewhere do not close a conflict while both sides still hold the
// file. That case is the one to take to a human.
func TestIntegrationOverlapStillHeldStaysOpen(t *testing.T) {
	hs := newHarness(t)
	sc := collideOnRest(t, hs)

	settleUpdate(t, hs, sc.claude, UpdateIntentParams{SessionKey: sc.claudeKey, AddPaths: []string{"app/worker.go"}})
	settleUpdate(t, hs, sc.claude, UpdateIntentParams{SessionKey: sc.claudeKey, DropPaths: []string{"app/worker.go"}})
	settleUpdate(t, hs, sc.codex, UpdateIntentParams{SessionKey: sc.codexKey, AddPaths: []string{"app/tortillas.go"}})
	settleUpdate(t, hs, sc.codex, UpdateIntentParams{SessionKey: sc.codexKey, DropPaths: []string{"app/tortillas.go"}})

	cs := conflictByKey(t, hs, sc.conflictKey)
	if cs.Status != enums.CONFLICT_STATUS_OPEN {
		t.Fatalf("both still hold app/rest.go and the conflict is %v (%v: %s); it must stay open",
			cs.Status, cs.Resolution, cs.Note)
	}
	if cs.Resolution != enums.CONFLICT_RESOLUTION_INVALID || cs.ResolvedAt.Valid || cs.Note != "" {
		t.Errorf("an open conflict carries a resolution: %+v", cs)
	}
	if n := resolvedEvents(t, hs, sc.conflictKey); n != 0 {
		t.Errorf("%d conflict_resolved events for a conflict still overlapping, want 0", n)
	}
	assertGaplessLog(t, hs)
}

// TestIntegrationSettlingReplaysVerbatim is test 4: the call that settles a
// conflict, retried, replays byte-for-byte and settles nothing twice.
func TestIntegrationSettlingReplaysVerbatim(t *testing.T) {
	hs := newHarness(t)
	sc := collideOnRest(t, hs)

	args := UpdateIntentParams{
		SessionKey:     sc.claudeKey,
		AddPaths:       []string{"app/salsas_picosas.go"},
		DropPaths:      []string{"app/rest.go"},
		IdempotencyKey: "move-salsas",
	}
	_, first := settleUpdate(t, hs, sc.claude, args)
	eventsAfterFirst := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", hs.teamID.String())
	noteAfterFirst := conflictByKey(t, hs, sc.conflictKey).Note

	_, second := settleUpdate(t, hs, sc.claude, args)
	if first != second {
		t.Fatalf("the retry answered differently:\nfirst  %s\nsecond %s", first, second)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", hs.teamID.String()); n != eventsAfterFirst {
		t.Errorf("the retry wrote %d more event(s)", n-eventsAfterFirst)
	}
	if n := resolvedEvents(t, hs, sc.conflictKey); n != 1 {
		t.Errorf("%d conflict_resolved events after a retry, want 1", n)
	}
	cs := conflictByKey(t, hs, sc.conflictKey)
	if cs.Status != enums.CONFLICT_STATUS_RESOLVED || cs.Note != noteAfterFirst {
		t.Errorf("the retry moved the conflict: %+v", cs)
	}
	assertGaplessLog(t, hs)
}

// ─────────────────────────────────────────────
// The rules, without a database
// ─────────────────────────────────────────────

func TestSuggestedActionTellsAgentsToSettleIt(t *testing.T) {
	mine := side(t, "1", "main", "app/rest.go", coordination.ModeWrite)
	theirs := side(t, "2", "main", "app/rest.go", coordination.ModeWrite)

	for name, other := range map[string]claimSide{"two people": theirs, "same person": sameMember(theirs, "1")} {
		v := assessOverlap(mine, other, nil)
		for role, s := range map[string]string{"initiator": v.ActionForInitiator, "incumbent": v.ActionForIncumbent} {
			for _, want := range []string{"settle it between you", "split the file or sequence the work", "ask your human only if you can't"} {
				if !strings.Contains(s, want) {
					t.Errorf("%s, %s: action %q is missing %q", name, role, s, want)
				}
			}
			t.Logf("%s, %s: %s", name, role, s)
		}
	}
}

func settleSideFor(t *testing.T, status enums.SessionStatus, patterns ...string) settleSide {
	t.Helper()
	s := settleSide{SessionUUID: uuid.Must(uuid.NewV4()).String(), Key: "S-" + patterns0(patterns), Status: status}
	for _, p := range patterns {
		s.Paths = append(s.Paths, settlePath{Mode: coordination.ModeWrite, Path: mustPath(t, p)})
	}
	return s
}

func patterns0(p []string) string {
	if len(p) == 0 {
		return "0"
	}
	return p[0]
}

func TestChooseResolutionRules(t *testing.T) {
	live := enums.SessionStatus(enums.SESSION_STATUS_LIVE)
	ended := enums.SessionStatus(enums.SESSION_STATUS_ENDED)
	agree := []settleNote{{SessionKey: "S-1", Outcome: "acknowledged", Text: "I move to a new file", Agrees: true}}
	refuse := []settleNote{{SessionKey: "S-1", Outcome: "refused", Text: "no"}}

	cases := []struct {
		name     string
		releaser settleSide
		other    settleSide
		notes    []settleNote
		kind     ReleaseKind
		want     enums.ConflictResolution
	}{
		{"agreed then cleared", settleSideFor(t, live, "app/new.go"), settleSideFor(t, live, "app/rest.go"), agree, ReleaseDropped, enums.CONFLICT_RESOLUTION_COORDINATED},
		{"both ended", settleSideFor(t, ended), settleSideFor(t, ended), nil, ReleaseSessionEnded, enums.CONFLICT_RESOLUTION_SUPERSEDED},
		{"moved to other files", settleSideFor(t, live, "app/new.go"), settleSideFor(t, live, "app/rest.go"), refuse, ReleaseDropped, enums.CONFLICT_RESOLUTION_SPLIT},
		{"dropped and holds nothing", settleSideFor(t, live), settleSideFor(t, live, "app/rest.go"), nil, ReleaseDropped, enums.CONFLICT_RESOLUTION_YIELDED},
		{"finished its intent", settleSideFor(t, live, "app/other.go"), settleSideFor(t, live, "app/rest.go"), nil, ReleaseIntentEnded, enums.CONFLICT_RESOLUTION_YIELDED},
		{"ended while the other holds", settleSideFor(t, ended), settleSideFor(t, live, "app/rest.go"), nil, ReleaseSessionEnded, enums.CONFLICT_RESOLUTION_YIELDED},
		{"claim expired", settleSideFor(t, live, "app/other.go"), settleSideFor(t, live, "app/rest.go"), nil, ReleaseClaimExpired, enums.CONFLICT_RESOLUTION_YIELDED},
	}
	for _, tc := range cases {
		rel := Release{SessionUUID: uuid.Must(uuid.FromString(tc.releaser.SessionUUID)), Kind: tc.kind}
		if got := chooseResolution([]settleSide{tc.releaser, tc.other}, tc.notes, rel); got != tc.want {
			t.Errorf("%s: resolution = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSidesStillOverlap(t *testing.T) {
	live := enums.SessionStatus(enums.SESSION_STATUS_LIVE)
	a := settleSideFor(t, live, "app/rest.go")
	b := settleSideFor(t, live, "app/*.go")
	if !sidesStillOverlap([]settleSide{a, b}) {
		t.Error("write x write on overlapping paths must keep the conflict open")
	}
	c := settleSideFor(t, live, "app/salsas_picosas.go")
	d := settleSideFor(t, live, "app/rest.go")
	if sidesStillOverlap([]settleSide{c, d}) {
		t.Error("two different files do not overlap")
	}
	a.Paths[0].Mode, b.Paths[0].Mode = coordination.ModeRead, coordination.ModeRead
	if sidesStillOverlap([]settleSide{a, b}) {
		t.Error("read x read never overlaps")
	}
}

func TestResolutionNoteIsBoundedAndRedacted(t *testing.T) {
	live := enums.SessionStatus(enums.SESSION_STATUS_LIVE)
	r := settleSideFor(t, live, "app/new.go")
	r.Key, r.Label = "S-20", "claude"
	o := settleSideFor(t, live, "app/rest.go")
	o.Key, o.Label = "S-19", "codex"
	long := "use token mtk_SECRETSECRET123 and bearer abc.def then " + strings.Repeat("really long explanation ", 40)
	notes := []settleNote{{SessionKey: "S-20", Outcome: "acknowledged", Text: long, Agrees: true}}
	rel := Release{SessionUUID: uuid.Must(uuid.FromString(r.SessionUUID)), Kind: ReleaseDropped, Paths: []string{"app/rest.go"}}

	note := buildResolutionNote(SettledConflict{Key: "CF-23", OverlapPath: "app/rest.go"}, []settleSide{r, o}, notes, rel)
	t.Logf("note: %s", note)
	if n := utf8.RuneCountInString(note); n > resolutionNoteChars {
		t.Errorf("note is %d characters, the column holds %d", n, resolutionNoteChars)
	}
	if strings.Contains(note, "SECRETSECRET") || strings.Contains(note, "abc.def") {
		t.Errorf("note leaks a credential: %s", note)
	}
	if !strings.Contains(note, "S-20 (claude) released app/rest.go") || !strings.Contains(note, "S-19 (codex) still holds app/rest.go") {
		t.Errorf("note does not say who released what: %s", note)
	}
}
