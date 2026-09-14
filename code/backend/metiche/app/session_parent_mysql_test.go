package app

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mklfarha/metiche/backend/enums"
)

// The supervisor -> subagent link (v6-session-parent), end to end through
// the REAL wiring: start_session over /v1/mcp with parent_session_key, then
// the board's snapshot, run history and session detail over REST. Every name
// is a test name and every secret is minted at run time.
//
//	METICHE_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/metiche_test?parseTime=true&interpolateParams=true' go test -p 1 ./app/ -run SessionParent -v

// wantNoParent is the one refusal every unusable parent_session_key gets.
const wantNoParent = "not_found: no live session with that key that you can delegate from"

var uuidShape = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// startAnswer is the part of start_session's envelope these tests read.
type startAnswer struct {
	OK               bool   `json:"ok"`
	Key              string `json:"key"`
	ParentSessionKey string `json:"parent_session_key"`
	Note             string `json:"note"`
}

func (w *inviteWorld) start(cs *mcp.ClientSession, slug string, args map[string]any) (startAnswer, string) {
	w.t.Helper()
	full := map[string]any{"project_key": "metiche", "team_slug": slug, "confirm_new_project": "person"}
	for k, v := range args {
		full[k] = v
	}
	isErr, text := w.tool(cs, "start_session", full)
	if isErr {
		w.t.Fatalf("start_session %v: %.300s", args, text)
	}
	var out startAnswer
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.Key == "" {
		w.t.Fatalf("start_session answer %.300s: %v", text, err)
	}
	return out, text
}

// refused runs start_session expecting the parent refusal, and proves it
// wrote nothing.
func (w *inviteWorld) refused(cs *mcp.ClientSession, tm testTeam, parentKey string) string {
	w.t.Helper()
	sessions := w.count("SELECT COUNT(*) FROM `session` WHERE `team_uuid` = ?", tm.id)
	events := w.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", tm.id)
	isErr, text := w.tool(cs, "start_session", map[string]any{
		"project_key": "metiche", "team_slug": tm.slug, "confirm_new_project": "person",
		"goal": "subagent task", "parent_session_key": parentKey,
	})
	if !isErr {
		w.t.Fatalf("parent_session_key %q was accepted: %.300s", parentKey, text)
	}
	if !strings.Contains(text, wantNoParent) {
		w.t.Fatalf("parent_session_key %q refused with the wrong message: %s", parentKey, text)
	}
	if n := w.count("SELECT COUNT(*) FROM `session` WHERE `team_uuid` = ?", tm.id); n != sessions {
		w.t.Errorf("the refusal wrote %d session row(s)", n-sessions)
	}
	if n := w.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", tm.id); n != events {
		w.t.Errorf("the refusal wrote %d event(s)", n-events)
	}
	return text
}

func (w *inviteWorld) get(p person, path string, into any) string {
	w.t.Helper()
	r := w.call(http.MethodGet, path, "", sessionHeader(p))
	if r.status != http.StatusOK {
		w.t.Fatalf("GET %s = %d %s", path, r.status, r.body)
	}
	if into != nil {
		if err := json.Unmarshal([]byte(r.body), into); err != nil {
			w.t.Fatalf("GET %s: %v", path, err)
		}
	}
	return r.body
}

func (w *inviteWorld) sessionID(tm testTeam, key string) string {
	w.t.Helper()
	var id string
	if err := w.db.QueryRow("SELECT `id` FROM `session` WHERE `team_uuid` = ? AND `key` = ?", tm.id, key).Scan(&id); err != nil {
		w.t.Fatalf("session %s: %v", key, err)
	}
	return id
}

func (w *inviteWorld) parentOf(tm testTeam, key string) *string {
	w.t.Helper()
	var parent *string
	if err := w.db.QueryRow("SELECT `parent_session_uuid` FROM `session` WHERE `team_uuid` = ? AND `key` = ?", tm.id, key).Scan(&parent); err != nil {
		w.t.Fatalf("session %s: %v", key, err)
	}
	return parent
}

type wireSession struct {
	Key              string `json:"key"`
	Status           string `json:"status"`
	Goal             string `json:"goal"`
	ParentSessionKey string `json:"parent_session_key"`
	Counts           struct {
		Subagents int `json:"subagents"`
	} `json:"counts"`
}

func byKey(list []wireSession) map[string]wireSession {
	out := map[string]wireSession{}
	for _, s := range list {
		out[s.Key] = s
	}
	return out
}

// A supervisor and two subagents of the same agent: the link is stored,
// returned, and shown on the snapshot, the run history and both run pages.
// Deleting the supervisor's row orphans the subagents and keeps them. No
// response carries a uuid.
func TestSessionParentLinksSubagentsEndToEnd(t *testing.T) {
	w := newInviteWorld(t)
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana := w.join(tm, "Ana")
	cs := w.connect(ana.token)

	var bodies []string
	sup, raw := w.start(cs, tm.slug, map[string]any{"goal": "supervise the refactor", "branch": "feat/parent"})
	bodies = append(bodies, raw)
	if sup.ParentSessionKey != "" {
		t.Fatalf("a session nobody delegated names parent %q", sup.ParentSessionKey)
	}

	subs := make([]startAnswer, 0, 2)
	for _, goal := range []string{"subagent: the handlers", "subagent: the tests"} {
		sub, raw := w.start(cs, tm.slug, map[string]any{"goal": goal, "parent_session_key": sup.Key})
		bodies = append(bodies, raw)
		t.Logf("subagent start answer: %s", raw)
		if sub.ParentSessionKey != sup.Key {
			t.Fatalf("start_session returned parent_session_key %q, want %q", sub.ParentSessionKey, sup.Key)
		}
		subs = append(subs, sub)
	}

	// Stored: the uuid of the supervisor, on both rows, and on no other.
	supID := w.sessionID(tm, sup.Key)
	for _, sub := range subs {
		if got := w.parentOf(tm, sub.Key); got == nil || *got != supID {
			t.Fatalf("%s parent_session_uuid = %v, want the supervisor's row", sub.Key, got)
		}
	}
	if got := w.parentOf(tm, sup.Key); got != nil {
		t.Fatalf("the supervisor has a parent: %v", *got)
	}
	// Subagents are sessions of the SAME agent, so they count toward its cap.
	if n := w.count("SELECT COUNT(DISTINCT `agent_uuid`) FROM `session` WHERE `team_uuid` = ?", tm.id); n != 1 {
		t.Fatalf("%d agents across supervisor and subagents, want 1", n)
	}
	if n := w.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `summary` LIKE ?", tm.id, "%as a subagent of "+sup.Key); n != 2 {
		t.Errorf("%d session_started summaries name the supervisor, want 2", n)
	}

	// Snapshot.
	var snap struct{ Sessions []wireSession }
	bodies = append(bodies, w.get(ana, "/v1/teams/"+tm.slug, &snap))
	lanes := byKey(snap.Sessions)
	if len(lanes) != 3 {
		t.Fatalf("snapshot has %d sessions, want 3", len(lanes))
	}
	for _, sub := range subs {
		if got := lanes[sub.Key].ParentSessionKey; got != sup.Key {
			t.Fatalf("snapshot: %s parent_session_key = %q, want %q", sub.Key, got, sup.Key)
		}
	}
	if got := lanes[sup.Key].ParentSessionKey; got != "" {
		t.Fatalf("snapshot: the supervisor names parent %q", got)
	}

	// Run history.
	var hist struct{ Sessions []wireSession }
	bodies = append(bodies, w.get(ana, "/v1/teams/"+tm.slug+"/sessions", &hist))
	rows := byKey(hist.Sessions)
	if rows[sup.Key].Counts.Subagents != 2 {
		t.Fatalf("history: supervisor counts.subagents = %d, want 2", rows[sup.Key].Counts.Subagents)
	}
	for _, sub := range subs {
		if rows[sub.Key].ParentSessionKey != sup.Key || rows[sub.Key].Counts.Subagents != 0 {
			t.Fatalf("history: %s = %+v, want parent %s and no subagents", sub.Key, rows[sub.Key], sup.Key)
		}
	}

	// Run detail, both ways.
	var supDetail struct {
		Session   wireSession   `json:"session"`
		Subagents []wireSession `json:"subagents"`
	}
	bodies = append(bodies, w.get(ana, "/v1/teams/"+tm.slug+"/sessions/"+sup.Key, &supDetail))
	if len(supDetail.Subagents) != 2 || supDetail.Subagents[0].Key != subs[0].Key || supDetail.Subagents[1].Key != subs[1].Key {
		t.Fatalf("supervisor detail subagents = %+v, want %s then %s", supDetail.Subagents, subs[0].Key, subs[1].Key)
	}
	for _, s := range supDetail.Subagents {
		if s.ParentSessionKey != sup.Key {
			t.Fatalf("supervisor detail: subagent %s names parent %q", s.Key, s.ParentSessionKey)
		}
	}
	var subDetail struct {
		Session   wireSession   `json:"session"`
		Subagents []wireSession `json:"subagents"`
	}
	bodies = append(bodies, w.get(ana, "/v1/teams/"+tm.slug+"/sessions/"+subs[0].Key, &subDetail))
	if subDetail.Session.ParentSessionKey != sup.Key || len(subDetail.Subagents) != 0 {
		t.Fatalf("subagent detail = %+v with %d subagents, want parent %s and none", subDetail.Session, len(subDetail.Subagents), sup.Key)
	}
	t.Logf("snapshot, history and both run pages carry %s -> {%s, %s}", sup.Key, subs[0].Key, subs[1].Key)

	// FK ON DELETE SET NULL: the supervisor's row goes, the subagents stay
	// and simply stop naming a parent.
	w.exec("DELETE FROM `session` WHERE `id` = ?", supID)
	for _, sub := range subs {
		if got := w.parentOf(tm, sub.Key); got != nil {
			t.Fatalf("%s still names the deleted supervisor: %s", sub.Key, *got)
		}
	}
	if n := w.count("SELECT COUNT(*) FROM `session` WHERE `team_uuid` = ?", tm.id); n != 2 {
		t.Fatalf("%d sessions after deleting the supervisor, want the 2 subagents", n)
	}
	var after struct{ Sessions []wireSession }
	bodies = append(bodies, w.get(ana, "/v1/teams/"+tm.slug, &after))
	if len(after.Sessions) != 2 {
		t.Fatalf("snapshot after the delete has %d sessions, want 2", len(after.Sessions))
	}
	for _, s := range after.Sessions {
		if s.ParentSessionKey != "" {
			t.Fatalf("snapshot after the delete: %s still names parent %q", s.Key, s.ParentSessionKey)
		}
	}
	t.Logf("supervisor row deleted: both subagents kept, parent_session_uuid NULL")

	// Canary: keys only, never a uuid, in anything this test was answered.
	ids := []string{supID, tm.id, ana.account, ana.member}
	for _, b := range bodies {
		if m := uuidShape.FindString(b); m != "" {
			t.Fatalf("a response carried a uuid (%s): %.400s", m, b)
		}
		for _, id := range ids {
			if strings.Contains(b, id) {
				t.Fatalf("a response carried a row id: %.400s", b)
			}
		}
	}
	t.Logf("%d responses read (3 start_session answers, 2 snapshots, history, 2 run pages): no uuid in any", len(bodies))
}

// Retrying a linked start_session returns the stored answer, byte for byte,
// even after the supervisor has ended and a fresh call would be refused.
func TestSessionParentReplayIsByteIdentical(t *testing.T) {
	w := newInviteWorld(t)
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana := w.join(tm, "Ana")
	cs := w.connect(ana.token)

	sup, _ := w.start(cs, tm.slug, map[string]any{"goal": "supervise"})
	args := map[string]any{"goal": "subagent", "parent_session_key": sup.Key, "idempotency_key": "subagent-retry-" + randHex(t, 4)}
	first, firstRaw := w.start(cs, tm.slug, args)
	_, secondRaw := w.start(cs, tm.slug, args)
	if firstRaw != secondRaw {
		t.Fatalf("the replay differs:\nfirst:  %s\nsecond: %s", firstRaw, secondRaw)
	}
	if first.ParentSessionKey != sup.Key {
		t.Fatalf("first answer parent_session_key = %q, want %q", first.ParentSessionKey, sup.Key)
	}

	if isErr, text := w.tool(cs, "end_session", map[string]any{"session_key": sup.Key}); isErr {
		t.Fatalf("end_session: %s", text)
	}
	_, thirdRaw := w.start(cs, tm.slug, args)
	if thirdRaw != firstRaw {
		t.Fatalf("the replay after the supervisor ended differs:\nfirst: %s\nthird: %s", firstRaw, thirdRaw)
	}
	if n := w.count("SELECT COUNT(*) FROM `session` WHERE `team_uuid` = ? AND `parent_session_uuid` IS NOT NULL", tm.id); n != 1 {
		t.Fatalf("%d linked sessions after three calls with one idempotency key, want 1", n)
	}
	t.Logf("three calls, one session, identical bytes: %s", firstRaw)
}

// Another person's live session on the same team is not a parent you may
// name.
func TestSessionParentFromAnotherAccountIsRefused(t *testing.T) {
	w := newInviteWorld(t)
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana := w.join(tm, "Ana")
	bob := w.join(tm, "Bob")
	anaCS, bobCS := w.connect(ana.token), w.connect(bob.token)

	bobs, _ := w.start(bobCS, tm.slug, map[string]any{"goal": "Bob's own work"})
	text := w.refused(anaCS, tm, bobs.Key)
	t.Logf("Ana naming Bob's live %s: %s", bobs.Key, text)

	// Bob naming his own session is fine: the refusal was about whose it is.
	own, _ := w.start(bobCS, tm.slug, map[string]any{"goal": "Bob's subagent", "parent_session_key": bobs.Key})
	if own.ParentSessionKey != bobs.Key {
		t.Fatalf("Bob's own subagent got parent %q, want %q", own.ParentSessionKey, bobs.Key)
	}
}

// The same person's live session on ANOTHER team is not a parent either:
// keys are per team, and a lane cannot hang under a lane on another board.
func TestSessionParentOnAnotherTeamIsRefused(t *testing.T) {
	w := newInviteWorld(t)
	here := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	there := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana := w.join(there, "Ana")
	cs := w.connect(ana.token)
	// The same account joins the second team with its own token.
	if isErr, text := w.tool(cs, "join_team", map[string]any{"join_code": here.code, "member_name": "Ana", "agent_label": "test", "client_key": "ana-second-" + randHex(t, 3)}); isErr {
		t.Fatalf("Ana joining a second team: %s", text)
	}
	if n := w.count("SELECT COUNT(DISTINCT `account_uuid`) FROM `member` WHERE `team_uuid` IN (?, ?)", here.id, there.id); n != 1 {
		t.Fatalf("%d accounts across the two teams, want Ana's 1", n)
	}

	elsewhere, _ := w.start(cs, there.slug, map[string]any{"goal": "work on the other board"})
	if n := w.count("SELECT COUNT(*) FROM `session` WHERE `team_uuid` = ? AND `key` = ?", here.id, elsewhere.Key); n != 0 {
		t.Fatalf("fixture: %s also exists on this team, so the test would prove nothing", elsewhere.Key)
	}
	text := w.refused(cs, here, elsewhere.Key)
	t.Logf("Ana naming her own live %s from another team: %s", elsewhere.Key, text)
}

// An unknown key and an ended session get the very same answer as the two
// refusals above; a stale session is still a supervisor.
func TestSessionParentUnknownOrEndedIsRefusedTheSameWay(t *testing.T) {
	w := newInviteWorld(t)
	tm := w.team(enums.TEAM_VISIBILITY_PRIVATE)
	ana := w.join(tm, "Ana")
	bob := w.join(tm, "Bob")
	cs, bobCS := w.connect(ana.token), w.connect(bob.token)

	unknown := w.refused(cs, tm, "S-99999")

	ended, _ := w.start(cs, tm.slug, map[string]any{"goal": "done already"})
	if isErr, text := w.tool(cs, "end_session", map[string]any{"session_key": ended.Key}); isErr {
		t.Fatalf("end_session: %s", text)
	}
	endedText := w.refused(cs, tm, ended.Key)

	bobs, _ := w.start(bobCS, tm.slug, map[string]any{"goal": "Bob's work"})
	otherAccount := w.refused(cs, tm, bobs.Key)

	if unknown != endedText || endedText != otherAccount {
		t.Fatalf("the refusals differ, so they tell a caller which keys exist:\nunknown: %s\nended:   %s\nBob's:   %s", unknown, endedText, otherAccount)
	}
	t.Logf("unknown, ended and another person's session all answer: %s", unknown)

	stale, _ := w.start(cs, tm.slug, map[string]any{"goal": "gone quiet"})
	w.exec("UPDATE `session` SET `status` = ? WHERE `team_uuid` = ? AND `key` = ?", enums.SESSION_STATUS_STALE, tm.id, stale.Key)
	sub, _ := w.start(cs, tm.slug, map[string]any{"goal": "subagent of a stale supervisor", "parent_session_key": stale.Key})
	if sub.ParentSessionKey != stale.Key {
		t.Fatalf("a stale supervisor was not accepted: parent %q, want %q", sub.ParentSessionKey, stale.Key)
	}
}
