package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/enums"
)

// Decisions, review context and judgements against a real MySQL. Run with
// METICHE_TEST_MYSQL_DSN set (see integration_test.go):
//
//	go test -p 1 ./app/mcp/ -run 'Decision|Review|Judgement' -v

// fastPathFixture is what declare_intent and update_intent returned, byte for
// byte, for fastPathScenario on a team with no decisions, captured from the
// build BEFORE the decision reviewer existed (METICHE_CAPTURE_FASTPATH=1 prints
// it). The reviewer's fast path must leave every one of these bytes alone.
const fastPathFixture = `["{\"ok\":true,\"key\":\"INT-7\",\"sequence\":7,\"revision\":6,\"pending\":{\"instructions\":0,\"conflicts\":0,\"reviews\":0},\"note\":\"INT-7 declared; holding 2 path(s) for write\"}","{\"ok\":true,\"key\":\"INT-8\",\"sequence\":8,\"revision\":6,\"pending\":{\"instructions\":0,\"conflicts\":1,\"reviews\":0},\"note\":\"INT-8 declared; holding 1 path(s) for write; 1 collision(s) on paths you just claimed — read conflicts[] before you edit\",\"conflicts\":[{\"key\":\"CF-8\",\"kind\":\"path_overlap\",\"severity\":\"critical\",\"with\":\"Ana (test)\",\"paths\":[\"web/src/auth/session.ts\"],\"suggested_action\":\"Ana (test) holds web/src/auth/session.ts (write, just now). You are both editing it: settle it between you — split the file or sequence the work; ask your human only if you can't.\"}]}","{\"ok\":true,\"key\":\"INT-8\",\"sequence\":9,\"revision\":6,\"pending\":{\"instructions\":0,\"conflicts\":1,\"reviews\":0},\"note\":\"INT-8 updated; 1 path(s) added\"}","{\"ok\":true,\"key\":\"INT-8\",\"sequence\":10,\"revision\":6,\"pending\":{\"instructions\":0,\"conflicts\":1,\"reviews\":0},\"note\":\"INT-8 updated\"}","{\"ok\":true,\"key\":\"INT-7\",\"sequence\":11,\"revision\":6,\"pending\":{\"instructions\":1,\"conflicts\":1,\"reviews\":0},\"note\":\"INT-7 is now active\"}","{\"ok\":true,\"key\":\"INT-12\",\"sequence\":12,\"revision\":6,\"pending\":{\"instructions\":1,\"conflicts\":1,\"reviews\":0},\"note\":\"INT-12 declared; holding 1 path(s) for read\"}"]`

// newDecisionHarness is the production wiring: the path detector with the
// decision reviewer chained after it, exactly as app/rest.go installs them.
func newDecisionHarness(t *testing.T) *harness {
	t.Helper()
	hs := newHarness(t)
	hs.h.SetDetector(ChainDetectors(
		NewPathDetector(hs.core, zap.NewNop()),
		NewDecisionReviewer(hs.core, zap.NewNop())))
	return hs
}

// fastPathScenario runs two agents through declarations and updates that
// collide on a path, and returns every declare_intent and update_intent
// response in order. Nothing in it names a team slug, a token or a uuid, so
// the bytes are the same on every fresh harness.
func fastPathScenario(t *testing.T, hs *harness) []string {
	t.Helper()
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	var out []string
	declare := func(a contractAgent, args DeclareIntentParams) {
		t.Helper()
		args.SessionKey = a.key
		res, _, err := hs.h.DeclareIntent(a.ctx, nil, args)
		if err != nil {
			t.Fatalf("declare_intent: %v", err)
		}
		out = append(out, resultText(t, res))
	}
	update := func(a contractAgent, args UpdateIntentParams) {
		t.Helper()
		args.SessionKey = a.key
		res, _, err := hs.h.UpdateIntent(a.ctx, nil, args)
		if err != nil {
			t.Fatalf("update_intent: %v", err)
		}
		out = append(out, resultText(t, res))
	}
	declare(ana, DeclareIntentParams{Summary: "move the session token into the auth service",
		Paths: []string{"web/src/auth/session.ts", "internal/auth/**"}, IdempotencyKey: "fp-ana-1"})
	// claim_path.created_at is a DATETIME, rounded to the second, so a
	// collision inside the same second can read as held for a negative time
	// and drop "just now" from the suggested action. A second's pause makes
	// the wording the same on every run.
	time.Sleep(1100 * time.Millisecond)
	declare(bob, DeclareIntentParams{Summary: "store the session token in localStorage after login",
		Paths: []string{"web/src/auth/session.ts"}, IdempotencyKey: "fp-bob-1"})
	update(bob, UpdateIntentParams{Summary: "read the session from the httpOnly cookie",
		AddPaths: []string{"web/src/api/client.ts"}, IdempotencyKey: "fp-bob-2"})
	update(bob, UpdateIntentParams{StatusLine: "wiring the cookie read", IdempotencyKey: "fp-bob-3"})
	update(ana, UpdateIntentParams{Status: "active", IdempotencyKey: "fp-ana-2"})
	declare(ana, DeclareIntentParams{Summary: "document the auth flow",
		Paths: []string{"docs/auth.md"}, Mode: "read", IdempotencyKey: "fp-ana-3"})
	return out
}

func renderFastPath(responses []string) string {
	b, _ := json.Marshal(responses)
	return string(b)
}

// TestIntegrationDecisionFastPathBytesUnchanged: a team with no ACCEPTED
// decision takes the reviewer's fast path, and gets exactly the bytes it got
// from the build before the reviewer existed (the fixture above, captured at
// commit b598ba5). A revoked or superseded decision is not an accepted one, so
// it changes nothing either.
func TestIntegrationDecisionFastPathBytesUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status enums.DecisionStatus
	}{
		{"no decisions at all", enums.DECISION_STATUS_INVALID},
		{"one revoked decision", enums.DECISION_STATUS_REVOKED},
		{"one superseded decision", enums.DECISION_STATUS_SUPERSEDED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hs := newDecisionHarness(t)
			if tc.status != enums.DECISION_STATUS_INVALID {
				// Written straight to the table: recording it through the tool
				// would write events and move the sequence the fixture pins.
				if _, err := hs.core.DB().Exec(
					"INSERT INTO `decision` (`id`,`team_uuid`,`key`,`title`,`statement`,`status`,`always_show`,`revision`) VALUES (?,?,?,?,?,?,0,1)",
					uuid.Must(uuid.NewV4()).String(), hs.teamID.String(), "#auth-jwt-cookie", "Auth is a JWT in an httpOnly cookie",
					"Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage.",
					int64(tc.status)); err != nil {
					t.Fatal(err)
				}
			}
			got := renderFastPath(fastPathScenario(t, hs))
			if os.Getenv("METICHE_CAPTURE_FASTPATH") == "1" {
				t.Logf("FIXTURE:%s", got)
				return
			}
			if got != fastPathFixture {
				t.Fatalf("declare/update bytes changed on a team with no accepted decisions:\ngot:  %s\nwant: %s", got, fastPathFixture)
			}
			if !strings.Contains(got, "path_overlap") {
				t.Fatal("the scenario should include a path collision")
			}
			if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `judgement`"); n != 0 {
				t.Errorf("the fast path wrote %d judgement(s)", n)
			}
		})
	}
}

// ─────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────

func (hs *harness) decRecord(t *testing.T, a contractAgent, args RecordDecisionParams) (Envelope, string) {
	t.Helper()
	args.SessionKey = a.key
	res, _, err := hs.h.RecordDecision(a.ctx, nil, args)
	if err != nil {
		t.Fatalf("record_decision %s: %v", args.Key, err)
	}
	text := resultText(t, res)
	var env Envelope
	decodeResult(t, res, &env)
	return env, text
}

func (hs *harness) decRecordErr(t *testing.T, a contractAgent, args RecordDecisionParams) string {
	t.Helper()
	args.SessionKey = a.key
	if _, _, err := hs.h.RecordDecision(a.ctx, nil, args); err != nil {
		return err.Error()
	}
	t.Fatalf("record_decision %s was accepted", args.Key)
	return ""
}

func (hs *harness) decDeclare(t *testing.T, a contractAgent, args DeclareIntentParams) (Envelope, string) {
	t.Helper()
	args.SessionKey = a.key
	res, _, err := hs.h.DeclareIntent(a.ctx, nil, args)
	if err != nil {
		t.Fatalf("declare_intent %q: %v", args.Summary, err)
	}
	text := resultText(t, res)
	var env Envelope
	decodeResult(t, res, &env)
	return env, text
}

func (hs *harness) decUpdate(t *testing.T, a contractAgent, args UpdateIntentParams) (Envelope, string) {
	t.Helper()
	args.SessionKey = a.key
	res, _, err := hs.h.UpdateIntent(a.ctx, nil, args)
	if err != nil {
		t.Fatalf("update_intent %s: %v", args.IntentKey, err)
	}
	text := resultText(t, res)
	var env Envelope
	decodeResult(t, res, &env)
	return env, text
}

// decBlock decodes the inline review block, which must be there.
func decBlock(t *testing.T, env Envelope) reviewBlock {
	t.Helper()
	if len(env.Review) == 0 {
		t.Fatal("the response carries no review block")
	}
	var b reviewBlock
	if err := json.Unmarshal(env.Review, &b); err != nil {
		t.Fatalf("review block is not JSON: %v (%s)", err, env.Review)
	}
	return b
}

// judgementCheck is one ledger row, as the tests read it.
type judgementCheck struct {
	pairKey     string
	decision    string
	decisionRev int64
	intent      string
	intentRev   int64
	judge       sql.NullString
	status      enums.JudgementStatus
	verdict     enums.JudgementVerdict
	severity    sql.NullInt64
	confidence  sql.NullFloat64
	rationale   sql.NullString
	assignments int64
	expires     sql.NullTime
	conflict    sql.NullString
	judgedAt    sql.NullTime
}

func (hs *harness) decJudgements(t *testing.T) []judgementCheck {
	t.Helper()
	rows, err := hs.core.DB().Query(
		"SELECT `pair_key`, `subject_a_uuid`, `subject_a_revision`, `subject_b_uuid`, `subject_b_revision`, `judge_session_uuid`, "+
			"`status`, COALESCE(`verdict`, 0), `severity`, `confidence`, `rationale`, `assignment_count`, `judging_expires_at`, "+
			"`conflict_uuid`, `judged_at` FROM `judgement` WHERE `team_uuid` = ? ORDER BY `created_at`, `pair_key`", hs.teamID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []judgementCheck
	for rows.Next() {
		var (
			j               judgementCheck
			status, verdict int64
		)
		if err := rows.Scan(&j.pairKey, &j.decision, &j.decisionRev, &j.intent, &j.intentRev, &j.judge, &status, &verdict,
			&j.severity, &j.confidence, &j.rationale, &j.assignments, &j.expires, &j.conflict, &j.judgedAt); err != nil {
			t.Fatal(err)
		}
		j.status, j.verdict = enums.JudgementStatus(status), enums.JudgementVerdict(verdict)
		out = append(out, j)
	}
	return out
}

// decConflict is one decision_contradiction conflict row.
type decConflict struct {
	uuid, key, rule, action, note, yield, evidence string
	severity, status, resolution, detectedBy       int64
	occurrences                                    int64
	confidence                                     sql.NullFloat64
	escalated                                      sql.NullTime
	resolvedBy                                     sql.NullString
}

func (hs *harness) decConflicts(t *testing.T) []decConflict {
	t.Helper()
	rows, err := hs.core.DB().Query(
		"SELECT `id`, `key`, COALESCE(`detector_rule`, ''), COALESCE(`suggested_action`, ''), COALESCE(`resolution_note`, ''), "+
			"COALESCE(`suggested_yield_session_uuid`, ''), COALESCE(`evidence`, '{}'), `severity`, `status`, COALESCE(`resolution`, 0), "+
			"`detected_by`, `occurrence_count`, `confidence`, `escalated_at`, `resolved_by_member_uuid` "+
			"FROM `conflict` WHERE `team_uuid` = ? AND `kind` = ? ORDER BY `created_at`, `key`",
		hs.teamID.String(), int64(enums.CONFLICT_KIND_DECISION_CONTRADICTION))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []decConflict
	for rows.Next() {
		var c decConflict
		if err := rows.Scan(&c.uuid, &c.key, &c.rule, &c.action, &c.note, &c.yield, &c.evidence, &c.severity, &c.status,
			&c.resolution, &c.detectedBy, &c.occurrences, &c.confidence, &c.escalated, &c.resolvedBy); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func (hs *harness) decMember(t *testing.T, a contractAgent) string {
	t.Helper()
	var member string
	if err := hs.core.DB().QueryRow("SELECT `member_uuid` FROM `session` WHERE `id` = ?", a.session.String()).Scan(&member); err != nil {
		t.Fatal(err)
	}
	return member
}

// decDecision reads one decision row's state.
func (hs *harness) decDecision(t *testing.T, key string) (id string, status enums.DecisionStatus, revision int64, alwaysShow bool, decidedBy, recordedBy, rationale string) {
	t.Helper()
	var st int64
	var dec, rec, rat sql.NullString
	if err := hs.core.DB().QueryRow(
		"SELECT `id`, `status`, `revision`, `always_show`, `decided_by_member_uuid`, `recorded_by_session_uuid`, `rationale` "+
			"FROM `decision` WHERE `team_uuid` = ? AND `key` = ?", hs.teamID.String(), key).
		Scan(&id, &st, &revision, &alwaysShow, &dec, &rec, &rat); err != nil {
		t.Fatalf("reading %s: %v", key, err)
	}
	return id, enums.DecisionStatus(st), revision, alwaysShow, dec.String, rec.String, rat.String
}

// ─────────────────────────────────────────────
// record_decision
// ─────────────────────────────────────────────

// TestIntegrationRecordDecisionLifecycle walks one key through every outcome
// in §3.1's table: new, unchanged, updated, revised, revoked, reinstated.
func TestIntegrationRecordDecisionLifecycle(t *testing.T) {
	hs := newDecisionHarness(t)
	db := hs.core.DB()
	ana := hs.contractAgent(t, "Ana", "client-a")

	// new
	args := goodDecision()
	args.IdempotencyKey = "ana-auth-1"
	args.Rationale = "one session mechanism for the web app and the API"
	env, first := hs.decRecord(t, ana, args)
	t.Logf("new: %s", first)
	if env.Key != "#auth-jwt-cookie" || len(env.Conflicts) != 0 {
		t.Fatalf("envelope = %+v", env)
	}
	if env.Note != "#auth-jwt-cookie recorded (revision 1, 2 scope path(s)); no live plan touches it yet" {
		t.Errorf("note = %q", env.Note)
	}
	id, status, revision, _, decidedBy, recordedBy, rationale := hs.decDecision(t, "#auth-jwt-cookie")
	if status != enums.DECISION_STATUS_ACCEPTED || revision != 1 || recordedBy != ana.session.String() || decidedBy != hs.decMember(t, ana) {
		t.Fatalf("decision row: status %s revision %d recorded_by %q decided_by %q", status.String(), revision, recordedBy, decidedBy)
	}
	if rationale == "" {
		t.Error("the rationale was not stored")
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `decision_path` WHERE `decision_uuid` = ?", id); n != 2 {
		t.Errorf("%d scope rows, want 2", n)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `decision_token` WHERE `decision_uuid` = ? AND `weight` = 2", id); n == 0 {
		t.Error("no title tokens were written")
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? AND `structural` = 1",
		hs.teamID.String(), int64(enums.EVENT_KIND_DECISION_RECORDED)); n != 1 {
		t.Errorf("%d structural decision_recorded events, want 1", n)
	}

	// The same call again replays byte for byte and records nothing.
	_, replay := hs.decRecord(t, ana, args)
	if replay != first {
		t.Fatalf("the replay is not byte-identical:\nfirst:  %s\nreplay: %s", first, replay)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `idempotency_key` LIKE ?",
		hs.teamID.String(), "%:ana-auth-1"); n != 1 {
		t.Errorf("%d events for the key, want 1", n)
	}

	// unchanged: same wording and scope under a new key.
	same := goodDecision()
	same.IdempotencyKey = "ana-auth-2"
	same.Rationale = "one session mechanism for the web app and the API"
	env, text := hs.decRecord(t, ana, same)
	t.Logf("unchanged: %s", text)
	if env.Note != "#auth-jwt-cookie: already recorded with this wording and scope (revision 1); nothing changed" {
		t.Errorf("note = %q", env.Note)
	}

	// updated: rationale only, no revision bump.
	cosmetic := goodDecision()
	cosmetic.Rationale = "a cookie is out of reach of injected scripts"
	env, text = hs.decRecord(t, ana, cosmetic)
	t.Logf("updated: %s", text)
	if env.Note != "#auth-jwt-cookie updated (revision 1 unchanged: rationale or always_show only)" {
		t.Errorf("note = %q", env.Note)
	}
	if _, _, revision, _, _, _, _ = hs.decDecision(t, "#auth-jwt-cookie"); revision != 1 {
		t.Errorf("a cosmetic update bumped the revision to %d", revision)
	}

	// revised: new wording, paths and tokens rewritten.
	revised := goodDecision()
	revised.Statement = "Sessions are a signed JWT in an httpOnly cookie, never a bearer header and never localStorage."
	revised.Scope = []string{"internal/auth/**"}
	env, text = hs.decRecord(t, ana, revised)
	t.Logf("revised: %s", text)
	if env.Note != "#auth-jwt-cookie revised to revision 2; 0 live plan(s) will be checked against the new wording" {
		t.Errorf("note = %q", env.Note)
	}
	if _, _, revision, _, _, _, _ = hs.decDecision(t, "#auth-jwt-cookie"); revision != 2 {
		t.Fatalf("revision = %d, want 2", revision)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `decision_path` WHERE `decision_uuid` = ?", id); n != 1 {
		t.Errorf("%d scope rows after the revision, want 1", n)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `decision_token` WHERE `decision_uuid` = ? AND `token` = ?", id, "bearer"); n != 1 {
		t.Error("the new wording's words were not written")
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `decision_token` WHERE `decision_uuid` = ? AND `token` = ?", id, "secure"); n != 0 {
		t.Error("the old wording's words survived the rewrite")
	}

	// revoked
	env, text = hs.decRecord(t, ana, RecordDecisionParams{Key: "auth-jwt-cookie", Revoke: true, Rationale: "moving to server-side sessions"})
	t.Logf("revoked: %s", text)
	if env.Note != "#auth-jwt-cookie revoked; 0 conflict(s) on it settled" {
		t.Errorf("note = %q", env.Note)
	}
	if _, status, _, _, _, _, _ = hs.decDecision(t, "#auth-jwt-cookie"); status != enums.DECISION_STATUS_REVOKED {
		t.Fatalf("status = %s, want revoked", status.String())
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		hs.teamID.String(), int64(enums.EVENT_KIND_DECISION_SUPERSEDED)); n != 1 {
		t.Errorf("%d decision_superseded events, want 1", n)
	}
	if got := hs.decRecordErr(t, ana, RecordDecisionParams{Key: "auth-jwt-cookie", Revoke: true}); got != "#auth-jwt-cookie is already revoked" {
		t.Errorf("second revoke: %q", got)
	}

	// reinstated
	env, text = hs.decRecord(t, ana, goodDecision())
	t.Logf("reinstated: %s", text)
	if env.Note != "#auth-jwt-cookie reinstated as revision 3; 0 live plan(s) will be checked against it" {
		t.Errorf("note = %q", env.Note)
	}
	if _, status, revision, _, _, _, _ = hs.decDecision(t, "#auth-jwt-cookie"); status != enums.DECISION_STATUS_ACCEPTED || revision != 3 {
		t.Errorf("after reinstating: status %s revision %d", status.String(), revision)
	}
}

// TestIntegrationRecordDecisionAlwaysShowCap: at most five accepted
// always-show decisions per team (§3.1), and turning one off frees the slot.
func TestIntegrationRecordDecisionAlwaysShowCap(t *testing.T) {
	hs := newDecisionHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")

	statements := []string{
		"Every response body is a JSON envelope with code, message and details fields.",
		"Background jobs run on the queue worker, never inside an HTTP handler.",
		"Money is stored in integer cents, never a float, everywhere in the system.",
		"Times crossing a boundary are RFC 3339 in UTC, never a local timestamp.",
		"Feature switches live in config, never as a compiled-in constant anywhere.",
	}
	for i, statement := range statements {
		env, _ := hs.decRecord(t, ana, RecordDecisionParams{
			Key: fmt.Sprintf("rule-%d", i+1), Title: fmt.Sprintf("Rule number %d", i+1), Statement: statement, AlwaysShow: true})
		if !strings.Contains(env.Note, "recorded (revision 1") {
			t.Fatalf("always-show %d: %q", i+1, env.Note)
		}
	}
	got := hs.decRecordErr(t, ana, RecordDecisionParams{
		Key: "rule-6", Title: "Rule number 6", AlwaysShow: true,
		Statement: "Uploaded files go to object storage, never to the application's own disk."})
	t.Logf("sixth always-show: %s", got)
	want := "the team already has 5 always-show decisions (#rule-1, #rule-2, #rule-3, #rule-4, #rule-5); " +
		"an always-show decision is checked against every plan, so turn one off before adding another"
	if got != want {
		t.Errorf("refusal = %q\nwant       %q", got, want)
	}

	// Turning one off is a cosmetic update, and frees the slot.
	off := RecordDecisionParams{Key: "rule-1", Title: "Rule number 1", Statement: statements[0]}
	if env, _ := hs.decRecord(t, ana, off); !strings.Contains(env.Note, "updated (revision 1 unchanged") {
		t.Errorf("turning always_show off: %q", env.Note)
	}
	if _, _, _, alwaysShow, _, _, _ := hs.decDecision(t, "#rule-1"); alwaysShow {
		t.Error("#rule-1 is still always-show")
	}
	if env, _ := hs.decRecord(t, ana, RecordDecisionParams{
		Key: "rule-6", Title: "Rule number 6", AlwaysShow: true,
		Statement: "Uploaded files go to object storage, never to the application's own disk."}); !strings.Contains(env.Note, "recorded (revision 1") {
		t.Errorf("the sixth should fit now: %q", env.Note)
	}
}

// TestIntegrationRecordDecisionPermission: changing another member's decision
// needs person_confirmed, and on success the decision changes hands (§10.1).
func TestIntegrationRecordDecisionPermission(t *testing.T) {
	hs := newDecisionHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	hs.decRecord(t, ana, goodDecision())

	revise := goodDecision()
	revise.Statement = "Sessions are a bearer token in the Authorization header on every API call."
	got := hs.decRecordErr(t, bob, revise)
	t.Logf("Bob revising Ana's decision: %s", got)
	want := fmt.Sprintf("not_permitted: #auth-jwt-cookie was recorded by Ana. Changing it changes what Ana agreed to: "+
		"settle it with Ana's agent (%s is live) or ask your person, then call again with person_confirmed: true.", ana.key)
	if got != want {
		t.Errorf("refusal = %q\nwant       %q", got, want)
	}
	// A cosmetic change is a change too.
	cosmetic := goodDecision()
	cosmetic.AlwaysShow = true
	if got := hs.decRecordErr(t, bob, cosmetic); !strings.HasPrefix(got, "not_permitted: ") {
		t.Errorf("cosmetic change by another member: %q", got)
	}
	// And so is superseding it.
	if got := hs.decRecordErr(t, bob, RecordDecisionParams{
		Key: "auth-bearer-header", Title: "Auth is a bearer token", Supersedes: "auth-jwt-cookie",
		Statement: "Clients send the session JWT as Authorization: Bearer on every API call."}); !strings.HasPrefix(got, "not_permitted: ") {
		t.Errorf("superseding another member's decision: %q", got)
	}
	if _, _, revision, _, _, _, _ := hs.decDecision(t, "#auth-jwt-cookie"); revision != 1 {
		t.Fatalf("a refused change wrote revision %d", revision)
	}

	// With the person's agreement it goes through, and the decision is Bob's.
	revise.PersonConfirmed = true
	env, text := hs.decRecord(t, bob, revise)
	t.Logf("Bob with person_confirmed: %s", text)
	if !strings.Contains(env.Note, "revised to revision 2") {
		t.Errorf("note = %q", env.Note)
	}
	if _, _, _, _, decidedBy, _, _ := hs.decDecision(t, "#auth-jwt-cookie"); decidedBy != hs.decMember(t, bob) {
		t.Error("decided_by did not move to the member who changed it")
	}
	// Now it is Ana who needs the agreement.
	if got := hs.decRecordErr(t, ana, goodDecision()); !strings.Contains(got, "was recorded by Bob") {
		t.Errorf("Ana revising Bob's decision: %q", got)
	}

	// With no live session of the decider, the message says to ask the person.
	if _, _, err := hs.h.EndSession(bob.ctx, nil, EndSessionParams{SessionKey: bob.key}); err != nil {
		t.Fatal(err)
	}
	got = hs.decRecordErr(t, ana, goodDecision())
	t.Logf("Bob's agent gone: %s", got)
	want = "not_permitted: #auth-jwt-cookie was recorded by Bob. Changing it changes what Bob agreed to: " +
		"ask your person, then call again with person_confirmed: true."
	if got != want {
		t.Errorf("refusal = %q\nwant       %q", got, want)
	}
}

// TestIntegrationRecordDecisionPairsLivePlans: a decision recorded after the
// plans exist pairs itself with them, assigned under the per-minute cap, and
// never with the recording session's own plans (§3.1).
func TestIntegrationRecordDecisionPairsLivePlans(t *testing.T) {
	hs := newDecisionHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	cai := hs.contractAgent(t, "Cai", "client-c")

	for i, file := range []string{"a.ts", "b.ts", "c.ts", "d.ts"} {
		hs.decDeclare(t, bob, DeclareIntentParams{
			Summary: fmt.Sprintf("wire login screen part %d", i+1), Paths: []string{"web/src/auth/" + file}})
	}
	hs.decDeclare(t, cai, DeclareIntentParams{Summary: "add the logout button", Paths: []string{"web/src/auth/logout.ts"}})
	hs.decDeclare(t, ana, DeclareIntentParams{Summary: "write the token signer", Paths: []string{"web/src/auth/sign.ts"}})

	env, text := hs.decRecord(t, ana, goodDecision())
	t.Logf("record with live plans: %s", text)
	if env.Note != "#auth-jwt-cookie recorded (revision 1, 2 scope path(s)); 5 live plan(s) will be checked against it" {
		t.Errorf("note = %q", env.Note)
	}

	rows := hs.decJudgements(t)
	if len(rows) != 5 {
		t.Fatalf("%d judgements, want 5 (four of Bob's plans, one of Cai's, none of Ana's own)", len(rows))
	}
	assigned := map[string]int{}
	unassigned := 0
	for _, r := range rows {
		if r.judge.Valid {
			assigned[r.judge.String]++
			if r.assignments != 1 || !r.expires.Valid {
				t.Errorf("assigned pair has assignment_count %d expires %v", r.assignments, r.expires.Valid)
			}
			continue
		}
		unassigned++
		if r.assignments != 0 || r.expires.Valid {
			t.Errorf("unassigned pair has assignment_count %d expires %v", r.assignments, r.expires.Valid)
		}
	}
	if assigned[bob.session.String()] != 3 || unassigned != 1 {
		t.Errorf("Bob got %d assigned and %d unassigned, want 3 and 1 (max_reviews_per_minute is 3)", assigned[bob.session.String()], unassigned)
	}
	if assigned[cai.session.String()] != 1 {
		t.Errorf("Cai got %d assigned, want 1: the cap is per session", assigned[cai.session.String()])
	}
	if assigned[ana.session.String()] != 0 {
		t.Error("the recording session's own plan was paired with its own decision")
	}

	// The count rides on a bare heartbeat, which is the whole push mechanism.
	res, _, err := hs.h.Heartbeat(bob.ctx, nil, HeartbeatParams{SessionKey: bob.key})
	if err != nil {
		t.Fatal(err)
	}
	var hb Envelope
	decodeResult(t, res, &hb)
	t.Logf("Bob's heartbeat: %s", resultText(t, res))
	if hb.Pending.Reviews != 3 {
		t.Errorf("pending.reviews = %d, want 3", hb.Pending.Reviews)
	}
	if hb.Note != "3 pair(s) to judge against your plan — call get_review_context, then report_judgement" {
		t.Errorf("note = %q", hb.Note)
	}
}

// TestIntegrationDeclareIntentReviewBlock: the inline block on a declaration,
// capped at three pairs, replayed byte for byte (§3.2).
func TestIntegrationDeclareIntentReviewBlock(t *testing.T) {
	hs := newDecisionHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")

	for _, d := range []RecordDecisionParams{
		{Key: "dec-alpha", Title: "Auth is a JWT in an httpOnly cookie",
			Statement: "Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage."},
		{Key: "dec-bravo", Title: "The login screen owns no styling",
			Statement: "Screens import shared components; none of them declares its own CSS module anywhere."},
		{Key: "dec-charlie", Title: "Forms validate on the server",
			Statement: "Every form posts to the API and shows what the server refused; browsers check nothing alone."},
		{Key: "dec-delta", Title: "Text comes from the catalogue",
			Statement: "User-facing wording is looked up by key, never written inline in a component file."},
	} {
		d.Scope = []string{"web/src/auth/**"}
		hs.decRecord(t, ana, d)
	}

	args := DeclareIntentParams{Summary: "store the session token in localStorage after login",
		Paths: []string{"web/src/auth/session.ts"}, IdempotencyKey: "bob-login-1"}
	env, first := hs.decDeclare(t, bob, args)
	t.Logf("declare with four candidate decisions: %s", first)

	block := decBlock(t, env)
	if len(block.Pairs) != reviewMaxInlinePairs || block.More != 0 || block.AnswerWith != "report_judgement" {
		t.Fatalf("block = %+v, want %d pairs", block, reviewMaxInlinePairs)
	}
	for _, p := range block.Pairs {
		if p.Why != "scope" || p.Statement == "" || len(p.PairKey) != 64 {
			t.Errorf("pair = %+v", p)
		}
	}
	if env.Pending.Reviews != 3 {
		t.Errorf("pending.reviews = %d, want 3", env.Pending.Reviews)
	}
	if !strings.HasSuffix(env.Note, "; judge 3 pair(s) against your plan before you edit: see review, then report_judgement") {
		t.Errorf("note = %q", env.Note)
	}
	rows := hs.decJudgements(t)
	if len(rows) != 3 {
		t.Fatalf("%d judgements, want 3: only the top three candidates are asked", len(rows))
	}

	// A replay returns the same pair keys and writes nothing new.
	_, replay := hs.decDeclare(t, bob, args)
	if replay != first {
		t.Fatalf("the replay is not byte-identical:\nfirst:  %s\nreplay: %s", first, replay)
	}
	if rows := hs.decJudgements(t); len(rows) != 3 {
		t.Errorf("the replay wrote %d judgements", len(rows))
	}
}

// TestIntegrationReviewBlockCanBeSwitchedOff: with review_block_enabled false
// the pairs are still assigned; only the block is omitted (§3.2).
func TestIntegrationReviewBlockCanBeSwitchedOff(t *testing.T) {
	hs := newDecisionHarness(t)
	if _, err := hs.core.DB().Exec("UPDATE `team` SET `settings` = ? WHERE `id` = ?",
		`{"review_block_enabled": false}`, hs.teamID.String()); err != nil {
		t.Fatal(err)
	}
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	hs.decRecord(t, ana, goodDecision())

	env, text := hs.decDeclare(t, bob, DeclareIntentParams{
		Summary: "store the session token in localStorage after login", Paths: []string{"web/src/auth/session.ts"}})
	t.Logf("declare with the block switched off: %s", text)
	if len(env.Review) != 0 {
		t.Errorf("review block = %s, want none", env.Review)
	}
	if env.Pending.Reviews != 1 {
		t.Errorf("pending.reviews = %d, want 1: the pair is still assigned", env.Pending.Reviews)
	}
	if !strings.HasSuffix(env.Note, "; judge 1 pair(s) against your plan before you edit: call get_review_context, then report_judgement") {
		t.Errorf("note = %q", env.Note)
	}
	if rows := hs.decJudgements(t); len(rows) != 1 || !rows[0].judge.Valid {
		t.Errorf("judgements = %+v", rows)
	}
}

// TestIntegrationUpdateIntentMintsPairs: a status line alone asks nothing new;
// a changed summary is a new revision and earns one more look (§3.2, F5).
func TestIntegrationUpdateIntentMintsPairs(t *testing.T) {
	hs := newDecisionHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	hs.decRecord(t, ana, goodDecision())

	env, _ := hs.decDeclare(t, bob, DeclareIntentParams{
		Summary: "store the session token in localStorage after login", Paths: []string{"web/src/auth/session.ts"}})
	intent := env.Key
	firstPair := decBlock(t, env).Pairs[0].PairKey

	env, text := hs.decUpdate(t, bob, UpdateIntentParams{IntentKey: intent, StatusLine: "wiring the login form"})
	t.Logf("status line only: %s", text)
	if len(env.Review) != 0 {
		t.Errorf("a status line minted a review block: %s", env.Review)
	}
	if rows := hs.decJudgements(t); len(rows) != 1 {
		t.Fatalf("%d judgements after a status line, want 1", len(rows))
	}

	env, text = hs.decUpdate(t, bob, UpdateIntentParams{IntentKey: intent, Summary: "read the session from the httpOnly cookie"})
	t.Logf("changed summary: %s", text)
	block := decBlock(t, env)
	if len(block.Pairs) != 1 || block.Pairs[0].PairKey == firstPair {
		t.Fatalf("a changed summary should mint a new pair: %+v", block)
	}
	rows := hs.decJudgements(t)
	if len(rows) != 2 {
		t.Fatalf("%d judgements, want 2", len(rows))
	}
	var revisions []int64
	for _, r := range rows {
		revisions = append(revisions, r.intentRev)
	}
	if revisions[0] == revisions[1] {
		t.Errorf("both pairs are against intent revision %d", revisions[0])
	}
}

// TestIntegrationDecisionJudgementInsertIsIdempotent: the unique index makes
// the first insert win, so a pair is never asked twice (§3.1).
func TestIntegrationDecisionJudgementInsertIsIdempotent(t *testing.T) {
	hs := newDecisionHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	hs.decRecord(t, ana, goodDecision())
	env, _ := hs.decDeclare(t, bob, DeclareIntentParams{Summary: "wire the login form", Paths: []string{"web/src/auth/session.ts"}})
	rows := hs.decJudgements(t)
	if len(rows) != 1 {
		t.Fatalf("setup: %d judgements", len(rows))
	}
	_ = env

	ctx := context.Background()
	tx, err := hs.core.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	j := decisionJudgement{DecisionUUID: rows[0].decision, DecisionRevision: rows[0].decisionRev,
		IntentUUID: rows[0].intent, IntentRevision: rows[0].intentRev, JudgeSession: bob.session.String()}
	key, inserted, err := insertDecisionJudgement(ctx, tx, hs.teamID, j, decisionJudgeWindow, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if inserted || key != rows[0].pairKey {
		t.Errorf("re-inserting the same pair: inserted=%v key=%s", inserted, key)
	}
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM `judgement` WHERE `team_uuid` = ? AND `pair_key` = ?", hs.teamID.String(), key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d rows for the pair, want 1", n)
	}
}

// TestIntegrationReviewerSkipsRecorderAndPinnedPairs: an agent is never asked
// to judge its own decision, and a pair a person pinned is never asked again
// (§3.1, §4.6).
func TestIntegrationReviewerSkipsRecorderAndPinnedPairs(t *testing.T) {
	hs := newDecisionHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	hs.decRecord(t, ana, goodDecision())

	// The recorder's own plan is not paired with the decision it just recorded.
	env, text := hs.decDeclare(t, ana, DeclareIntentParams{
		Summary: "sign the session token", Paths: []string{"web/src/auth/sign.ts"}})
	t.Logf("the recorder's own declaration: %s", text)
	if len(env.Review) != 0 || env.Pending.Reviews != 0 {
		t.Errorf("the recording session was asked to judge its own decision: %s", text)
	}

	// Bob is asked, answers, and a person pins the pair.
	env, _ = hs.decDeclare(t, bob, DeclareIntentParams{
		Summary: "store the session token in localStorage after login", Paths: []string{"web/src/auth/session.ts"}})
	intent := env.Key
	if _, err := hs.core.DB().Exec("UPDATE `judgement` SET `pinned` = 1 WHERE `team_uuid` = ?", hs.teamID.String()); err != nil {
		t.Fatal(err)
	}

	// A new revision of the plan does not ask again.
	env, text = hs.decUpdate(t, bob, UpdateIntentParams{IntentKey: intent, Summary: "read the session from the cookie instead"})
	t.Logf("after pinning, a revised plan: %s", text)
	if len(env.Review) != 0 {
		t.Errorf("a pinned pair was asked again: %s", env.Review)
	}
	if rows := hs.decJudgements(t); len(rows) != 1 {
		t.Errorf("%d judgements, want the one pinned row", len(rows))
	}

	// Neither does a new revision of the decision.
	revised := goodDecision()
	revised.Statement = "Sessions are a signed JWT in an httpOnly cookie, and the API reads it from there only."
	env, text = hs.decRecord(t, ana, revised)
	t.Logf("after pinning, a revised decision: %s", text)
	if !strings.Contains(env.Note, "0 live plan(s) will be checked against the new wording") {
		t.Errorf("note = %q", env.Note)
	}
	if rows := hs.decJudgements(t); len(rows) != 1 {
		t.Errorf("%d judgements after revising, want the one pinned row", len(rows))
	}
}

// ─────────────────────────────────────────────
// get_review_context and report_judgement
// ─────────────────────────────────────────────

func (hs *harness) decContext(t *testing.T, a contractAgent, args GetReviewContextParams) (ReviewContextResult, string) {
	t.Helper()
	args.SessionKey = a.key
	res, _, err := hs.h.GetReviewContext(a.ctx, nil, args)
	if err != nil {
		t.Fatalf("get_review_context: %v", err)
	}
	text := resultText(t, res)
	var out ReviewContextResult
	decodeResult(t, res, &out)
	return out, text
}

func (hs *harness) decContextErr(t *testing.T, a contractAgent, args GetReviewContextParams) string {
	t.Helper()
	args.SessionKey = a.key
	if _, _, err := hs.h.GetReviewContext(a.ctx, nil, args); err != nil {
		return err.Error()
	}
	t.Fatal("get_review_context was accepted")
	return ""
}

func (hs *harness) decJudge(t *testing.T, a contractAgent, args ReportJudgementParams) (Envelope, string) {
	t.Helper()
	args.SessionKey = a.key
	res, _, err := hs.h.ReportJudgement(a.ctx, nil, args)
	if err != nil {
		t.Fatalf("report_judgement %s: %v", args.Verdict, err)
	}
	text := resultText(t, res)
	var env Envelope
	decodeResult(t, res, &env)
	return env, text
}

func (hs *harness) decJudgeErr(t *testing.T, a contractAgent, args ReportJudgementParams) string {
	t.Helper()
	args.SessionKey = a.key
	if _, _, err := hs.h.ReportJudgement(a.ctx, nil, args); err != nil {
		return err.Error()
	}
	t.Fatalf("report_judgement %s was accepted", args.Verdict)
	return ""
}

func conf(v float64) *float64 { return &v }

// decPair records #auth-jwt-cookie as Ana and declares one plan of Bob's
// inside its scope, returning Bob's intent key and the pair he must judge.
func (hs *harness) decPair(t *testing.T, ana, bob contractAgent, summary string) (intentKey, pairKey string) {
	t.Helper()
	hs.decRecord(t, ana, goodDecision())
	env, _ := hs.decDeclare(t, bob, DeclareIntentParams{Summary: summary, Paths: []string{"web/src/auth/session.ts"}})
	return env.Key, decBlock(t, env).Pairs[0].PairKey
}

func (hs *harness) decInstructionsFor(t *testing.T, session uuid.UUID) int {
	t.Helper()
	return countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `instruction` WHERE `target_session_uuid` = ?", session.String())
}

// TestIntegrationGetReviewContextIsReadOnly: it answers with the pairs, and
// the database is byte for byte what it was (§3.2, §7.2).
func TestIntegrationGetReviewContextIsReadOnly(t *testing.T) {
	hs := newDecisionHarness(t)
	db := hs.core.DB()
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	cai := hs.contractAgent(t, "Cai", "client-c")

	pad := func(s string) string {
		return s + strings.Repeat(" and this sentence is padding so the item fills the budget", 8)[:400-len(s)]
	}
	for _, d := range []RecordDecisionParams{
		{Key: "dec-alpha", Title: "Auth is a JWT in an httpOnly cookie", Statement: pad("Sessions are a signed JWT in an httpOnly, Secure cookie.")},
		{Key: "dec-bravo", Title: "Screens import shared components", Statement: pad("No screen declares its own CSS module.")},
		{Key: "dec-charlie", Title: "Forms validate on the server", Statement: pad("Every form posts to the API and shows what it refused.")},
	} {
		d.Scope = []string{"web/src/auth/**"}
		hs.decRecord(t, ana, d)
	}
	env, _ := hs.decDeclare(t, bob, DeclareIntentParams{
		Summary: "store the session token in localStorage after login", Paths: []string{"web/src/auth/session.ts"}})
	intent := env.Key
	caiEnv, _ := hs.decDeclare(t, cai, DeclareIntentParams{
		Summary: "add the logout button to the header", Paths: []string{"web/src/auth/logout.ts"}})
	caiPair := decBlock(t, caiEnv).Pairs[0].PairKey

	before := []int{
		countRows(t, db, "SELECT COUNT(*) FROM `judgement`"),
		countRows(t, db, "SELECT COUNT(*) FROM `instruction`"),
		countRows(t, db, "SELECT COUNT(*) FROM `team_event`"),
		countRows(t, db, "SELECT `sequence` FROM `team` WHERE `id` = ?", hs.teamID.String()),
		countRows(t, db, "SELECT COUNT(*) FROM `judgement` WHERE `status` = ?", int64(enums.JUDGEMENT_STATUS_PENDING)),
	}

	got, text := hs.decContext(t, bob, GetReviewContextParams{Limit: 3})
	t.Logf("get_review_context: %s", text)
	if len(got.Reviews) != 2 || got.MoreWaiting != 1 {
		t.Fatalf("%d reviews, more_waiting %d, want 2 and 1 (the 2200-character budget)", len(got.Reviews), got.MoreWaiting)
	}
	if got.Note != "2 pair(s) to judge: read each, then report_judgement; 1 more waiting — call get_review_context again" {
		t.Errorf("note = %q", got.Note)
	}
	if got.Pending.Reviews != 3 || got.Key != bob.key {
		t.Errorf("envelope = %+v", got.Envelope)
	}
	for _, item := range got.Reviews {
		if item.Kind != "decision_contradiction" || item.AnswerWith != reviewAnswerWith || item.ExpiresAt == nil {
			t.Errorf("item = %+v", item)
		}
		if item.Question != fmt.Sprintf("Would %s, as planned, break decision %s?", intent, item.Decision.Key) {
			t.Errorf("question = %q", item.Question)
		}
		if item.Decision == nil || item.Decision.Statement == "" || len(item.Decision.Scope) != 1 || item.Decision.DecidedBy != "Ana" {
			t.Errorf("decision = %+v", item.Decision)
		}
		if item.Plan.Key != intent || len(item.Plan.Paths) != 1 || item.Plan.Paths[0] != "web/src/auth/session.ts" || item.Plan.Revision != 1 {
			t.Errorf("plan = %+v", item.Plan)
		}
		if len(item.Why) == 0 || item.Why[0] != "your claim web/src/auth/session.ts is in its scope" {
			t.Errorf("why = %v", item.Why)
		}
	}
	// answer_with names the parameter, which is the point of it; what must
	// never appear is a judgement's own rationale field.
	if strings.Contains(text, `"rationale":`) {
		t.Error("the review context carries a rationale")
	}

	after := []int{
		countRows(t, db, "SELECT COUNT(*) FROM `judgement`"),
		countRows(t, db, "SELECT COUNT(*) FROM `instruction`"),
		countRows(t, db, "SELECT COUNT(*) FROM `team_event`"),
		countRows(t, db, "SELECT `sequence` FROM `team` WHERE `id` = ?", hs.teamID.String()),
		countRows(t, db, "SELECT COUNT(*) FROM `judgement` WHERE `status` = ?", int64(enums.JUDGEMENT_STATUS_PENDING)),
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("get_review_context wrote something: counts %v became %v", before, after)
		}
	}

	if one, _ := hs.decContext(t, bob, GetReviewContextParams{Limit: 1}); len(one.Reviews) != 1 || one.MoreWaiting != 2 {
		t.Errorf("limit 1: %d reviews, more_waiting %d", len(one.Reviews), one.MoreWaiting)
	}
	if clamped, _ := hs.decContext(t, bob, GetReviewContextParams{Limit: 99}); len(clamped.Reviews) != 2 {
		t.Errorf("limit 99 should clamp to %d: %d reviews", ReviewContextMaxLimit, len(clamped.Reviews))
	}

	// One pair by key, and never somebody else's.
	mine := got.Reviews[0].PairKey
	if one, _ := hs.decContext(t, bob, GetReviewContextParams{PairKey: mine}); len(one.Reviews) != 1 || one.Reviews[0].PairKey != mine {
		t.Errorf("by pair_key: %+v", one.Reviews)
	}
	if err := hs.decContextErr(t, bob, GetReviewContextParams{PairKey: caiPair}); err != fmt.Sprintf("this pair is %s's to judge; nothing for you here", cai.key) {
		t.Errorf("another session's pair: %q", err)
	}
	if err := hs.decContextErr(t, bob, GetReviewContextParams{PairKey: "nope"}); err !=
		"pair_key nope is not a pair on this team — call get_review_context to see what is waiting for you" {
		t.Errorf("unknown pair: %q", err)
	}

	// An answered pair says so, with the verdict and when.
	hs.decJudge(t, cai, ReportJudgementParams{PairKey: caiPair, Verdict: "no_conflict", Confidence: conf(0.95)})
	if err := hs.decContextErr(t, cai, GetReviewContextParams{PairKey: caiPair}); !strings.HasPrefix(err,
		fmt.Sprintf("pair %s was already judged no conflict at ", caiPair)) {
		t.Errorf("already judged: %q", err)
	}

	// And it still answers an ended session, which is where get_instructions
	// stands too.
	if _, _, err := hs.h.EndSession(bob.ctx, nil, EndSessionParams{SessionKey: bob.key}); err != nil {
		t.Fatal(err)
	}
	ended, text := hs.decContext(t, bob, GetReviewContextParams{})
	t.Logf("on an ended session: %s", text)
	if len(ended.Reviews) != 0 || ended.Note != "nothing waiting for you to judge" {
		t.Errorf("ended session: %+v", ended)
	}
}

// TestIntegrationReportJudgementNoConflict: the usual answer costs one tiny
// event and raises nothing (§3.3).
func TestIntegrationReportJudgementNoConflict(t *testing.T) {
	hs := newDecisionHarness(t)
	db := hs.core.DB()
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	intent, pair := hs.decPair(t, ana, bob, "read the session from the httpOnly cookie the login endpoint sets")

	env, text := hs.decJudge(t, bob, ReportJudgementParams{PairKey: pair, Verdict: "no_conflict", Confidence: conf(0.95),
		Rationale: "the plan reads the cookie the decision requires"})
	t.Logf("no_conflict: %s", text)
	if env.Note != fmt.Sprintf("judged %s against #auth-jwt-cookie: no conflict (0.95)", intent) {
		t.Errorf("note = %q", env.Note)
	}
	if len(env.Conflicts) != 0 || env.Pending.Conflicts != 0 {
		t.Errorf("a no_conflict raised %+v", env.Conflicts)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `conflict`"); n != 0 {
		t.Errorf("%d conflict rows, want 0", n)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `team_event` WHERE `kind` = ? AND `structural` = 0",
		int64(enums.EVENT_KIND_JUDGEMENT_REPORTED)); n != 1 {
		t.Errorf("%d non-structural judgement_reported events, want 1 (PLAN's one tiny event)", n)
	}
	rows := hs.decJudgements(t)
	if len(rows) != 1 || rows[0].status != enums.JUDGEMENT_STATUS_JUDGED || rows[0].verdict != enums.JUDGEMENT_VERDICT_NO_CONFLICT {
		t.Fatalf("ledger = %+v", rows)
	}
	if rows[0].severity.Valid || !rows[0].judgedAt.Valid || !rows[0].confidence.Valid {
		t.Errorf("row = %+v: severity is stored only for a conflict, judged_at always", rows[0])
	}
	if n := hs.decInstructionsFor(t, ana.session); n != 0 {
		t.Errorf("the decider was told about a no_conflict (%d instructions)", n)
	}
}

// TestIntegrationReportJudgementConflict: the conflict row, its evidence, its
// participants, the decider's notice and the caller's conflicts[] (§3.3).
func TestIntegrationReportJudgementConflict(t *testing.T) {
	hs := newDecisionHarness(t)
	db := hs.core.DB()
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	intent, pair := hs.decPair(t, ana, bob, "store the session token in localStorage after login")

	args := ReportJudgementParams{PairKey: pair, Verdict: "conflict", Confidence: conf(0.9),
		Rationale: "plan stores the token in localStorage; #auth-jwt-cookie forbids it"}
	env, first := hs.decJudge(t, bob, args)
	t.Logf("conflict 0.9: %s", first)

	if len(env.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want one", env.Conflicts)
	}
	notice := env.Conflicts[0]
	if notice.Kind != "decision_contradiction" || notice.Severity != "medium" || notice.AtFault != "plan" ||
		notice.Decision != "#auth-jwt-cookie" || notice.With != "Ana (test)" {
		t.Fatalf("notice = %+v", notice)
	}
	if !strings.Contains(notice.SuggestedAction, fmt.Sprintf("Your plan %s breaks #auth-jwt-cookie", intent)) ||
		!strings.Contains(notice.SuggestedAction, fmt.Sprintf("settle that with Ana's agent (%s)", ana.key)) {
		t.Errorf("suggested_action = %q", notice.SuggestedAction)
	}
	if env.Key != notice.Key || env.Note != fmt.Sprintf("judged %s against #auth-jwt-cookie: conflict (0.90) — read conflicts[] before you edit", intent) {
		t.Errorf("envelope key %q note %q", env.Key, env.Note)
	}
	if env.Pending.Conflicts != 1 {
		t.Errorf("pending.conflicts = %d", env.Pending.Conflicts)
	}

	rows := hs.decConflicts(t)
	if len(rows) != 1 {
		t.Fatalf("%d conflict rows", len(rows))
	}
	c := rows[0]
	if c.rule != RuleDecisionJudged || c.detectedBy != int64(enums.DETECTED_BY_AGENT) ||
		enums.ConflictSeverity(c.severity) != enums.CONFLICT_SEVERITY_MEDIUM ||
		enums.ConflictStatus(c.status) != enums.CONFLICT_STATUS_OPEN {
		t.Fatalf("conflict row = %+v", c)
	}
	if !c.confidence.Valid || c.confidence.Float64 < 0.89 || c.yield != bob.session.String() {
		t.Errorf("confidence %v yield %q", c.confidence, c.yield)
	}
	t.Logf("evidence: %s", c.evidence)
	// Decoded rather than matched as text: MySQL parses a JSON column and
	// hands it back with its own spacing.
	var evidence map[string]any
	if err := json.Unmarshal([]byte(c.evidence), &evidence); err != nil {
		t.Fatalf("evidence is not JSON: %v", err)
	}
	for field, want := range map[string]string{
		"overlap_path": "#auth-jwt-cookie",
		"a_label":      "decision: #auth-jwt-cookie (Ana)",
		"a_summary":    goodDecision().Statement,
		"a_pattern":    "web/src/auth/**",
		"b_label":      "Bob (test)",
		"b_summary":    "store the session token in localStorage after login",
		"b_pattern":    "web/src/auth/session.ts",
		"detail":       RuleDecisionJudged,
	} {
		if got, _ := evidence[field].(string); got != want {
			t.Errorf("evidence %s = %q, want %q", field, got, want)
		}
	}
	issues, _ := evidence["field_issues"].([]any)
	if len(issues) != 1 {
		t.Fatalf("field_issues = %v", evidence["field_issues"])
	}
	wantIssue := fmt.Sprintf("%s's model (0.90): plan stores the token in localStorage; #auth-jwt-cookie forbids it", bob.key)
	if issue, _ := issues[0].(string); issue != wantIssue {
		t.Errorf("field_issues[0] = %q\nwant              %q", issues[0], wantIssue)
	}

	// Participants: the plan's owner as initiator, the decider as incumbent.
	var kind int64
	var subject string
	if err := db.QueryRow("SELECT `subject_kind`, `subject_uuid` FROM `conflict_participant` WHERE `conflict_uuid` = ? AND `session_uuid` = ? AND `role` = ?",
		c.uuid, bob.session.String(), int64(enums.PARTICIPANT_ROLE_INITIATOR)).Scan(&kind, &subject); err != nil {
		t.Fatalf("no initiator participant: %v", err)
	}
	if enums.SubjectKind(kind) != enums.SUBJECT_KIND_INTENT {
		t.Errorf("initiator subject kind = %s", enums.SubjectKind(kind).String())
	}
	if err := db.QueryRow("SELECT `subject_kind`, `subject_uuid` FROM `conflict_participant` WHERE `conflict_uuid` = ? AND `session_uuid` = ? AND `role` = ?",
		c.uuid, ana.session.String(), int64(enums.PARTICIPANT_ROLE_INCUMBENT)).Scan(&kind, &subject); err != nil {
		t.Fatalf("no incumbent participant: %v", err)
	}
	if enums.SubjectKind(kind) != enums.SUBJECT_KIND_DECISION {
		t.Errorf("incumbent subject kind = %s", enums.SubjectKind(kind).String())
	}

	// The decider hears about it once, through an instruction; the caller was
	// told synchronously and gets none.
	if n := hs.decInstructionsFor(t, bob.session); n != 0 {
		t.Errorf("the caller got %d instruction(s)", n)
	}
	var body string
	if err := db.QueryRow("SELECT `body` FROM `instruction` WHERE `target_session_uuid` = ?", ana.session.String()).Scan(&body); err != nil {
		t.Fatalf("the decider got no notice: %v", err)
	}
	t.Logf("Ana's notice: %s", body)
	if !strings.HasPrefix(body, fmt.Sprintf("%s (medium) on #auth-jwt-cookie: ", c.key)) ||
		!strings.Contains(body, fmt.Sprintf("judged its plan %s breaks your decision", intent)) ||
		!strings.Contains(body, "revise it with record_decision") {
		t.Errorf("notice body = %q", body)
	}
	res, _, err := hs.h.GetInstructions(ana.ctx, nil, GetInstructionsParams{SessionKey: ana.key})
	if err != nil {
		t.Fatal(err)
	}
	var got InstructionsResult
	decodeResult(t, res, &got)
	t.Logf("Ana's get_instructions: %s", resultText(t, res))
	if len(got.Instructions) != 1 || got.Instructions[0].Ref != c.key || got.Instructions[0].Kind != "conflict_notice" {
		t.Fatalf("instructions = %+v", got.Instructions)
	}

	// The event is structural: the conflict list changed shape.
	if n := countRows(t, db, "SELECT COUNT(*) FROM `team_event` WHERE `kind` = ? AND `structural` = 1",
		int64(enums.EVENT_KIND_JUDGEMENT_REPORTED)); n != 1 {
		t.Errorf("%d structural judgement_reported events, want 1", n)
	}
	if rows := hs.decJudgements(t); len(rows) != 1 || !rows[0].conflict.Valid || rows[0].conflict.String != c.uuid {
		t.Errorf("the verdict is not linked to its conflict: %+v", rows)
	}

	// The same verdict replays; a different one is refused.
	args.Confidence, args.Rationale = conf(0.7), "a different rationale entirely"
	if _, replay := hs.decJudge(t, bob, args); replay != first {
		t.Fatalf("the replay is not byte-identical:\nfirst:  %s\nreplay: %s", first, replay)
	}
	other := ReportJudgementParams{PairKey: pair, Verdict: "no_conflict"}
	if err := hs.decJudgeErr(t, bob, other); !strings.HasPrefix(err, "already judged conflict at ") ||
		!strings.HasSuffix(err, "; a changed plan earns a new pair when you update_intent") {
		t.Errorf("a second verdict: %q", err)
	}
}

// TestIntegrationReportJudgementBelowTheFloorIsQuiet: a hedged conflict and an
// unsure verdict are recorded on the board and interrupt nobody (§4.2).
func TestIntegrationReportJudgementBelowTheFloorIsQuiet(t *testing.T) {
	hs := newDecisionHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	hs.decRecord(t, ana, goodDecision())

	first, _ := hs.decDeclare(t, bob, DeclareIntentParams{Summary: "keep the token in the browser store", Paths: []string{"web/src/auth/session.ts"}})
	second, _ := hs.decDeclare(t, bob, DeclareIntentParams{Summary: "add a remember-me box to the login form", Paths: []string{"web/src/auth/remember.ts"}})

	env, text := hs.decJudge(t, bob, ReportJudgementParams{PairKey: decBlock(t, first).Pairs[0].PairKey,
		Verdict: "conflict", Confidence: conf(0.6), Rationale: "it might keep the token where the decision forbids"})
	t.Logf("conflict 0.6: %s", text)
	if len(env.Conflicts) != 0 {
		t.Errorf("a 0.6 conflict interrupted the caller: %+v", env.Conflicts)
	}
	if env.Note != fmt.Sprintf("judged %s against #auth-jwt-cookie: conflict (0.60), recorded low on the board; below 0.7 it interrupts nobody", first.Key) {
		t.Errorf("note = %q", env.Note)
	}

	env, text = hs.decJudge(t, bob, ReportJudgementParams{PairKey: decBlock(t, second).Pairs[0].PairKey,
		Verdict: "unsure", Rationale: "the decision does not say where a remember-me flag lives"})
	t.Logf("unsure: %s", text)
	if len(env.Conflicts) != 0 {
		t.Errorf("an unsure verdict interrupted the caller: %+v", env.Conflicts)
	}
	if env.Note != fmt.Sprintf("judged %s against #auth-jwt-cookie: unsure, recorded low on the board", second.Key) {
		t.Errorf("note = %q", env.Note)
	}

	rows := hs.decConflicts(t)
	if len(rows) != 2 {
		t.Fatalf("%d conflict rows, want two low ones", len(rows))
	}
	rules := map[string]decConflict{}
	for _, c := range rows {
		rules[c.rule] = c
		if enums.ConflictSeverity(c.severity) != enums.CONFLICT_SEVERITY_LOW {
			t.Errorf("%s is %s, want low", c.key, enums.ConflictSeverity(c.severity).String())
		}
	}
	if _, ok := rules[RuleDecisionJudged]; !ok {
		t.Error("the hedged conflict has no decision_contradiction.judged row")
	}
	unsure, ok := rules[RuleDecisionUnsure]
	if !ok {
		t.Fatal("the unsure verdict has no decision_contradiction.unsure row")
	}
	if !strings.Contains(unsure.evidence, "model (unsure):") {
		t.Errorf("an unsure verdict without a confidence should render (unsure): %s", unsure.evidence)
	}
	if !strings.Contains(unsure.action, "does not settle whether") {
		t.Errorf("unsure suggested_action = %q", unsure.action)
	}
	if n := hs.decInstructionsFor(t, ana.session); n != 0 {
		t.Errorf("the decider was interrupted %d time(s) below the notify floor", n)
	}
}

// TestIntegrationReportJudgementSameMemberIsSofter: one person's two agents
// are coordinated by that person — severity drops a step and nobody is told
// (§4.2 step 4).
func TestIntegrationReportJudgementSameMemberIsSofter(t *testing.T) {
	hs := newDecisionHarness(t)
	first := hs.join(t, "Ana", "client-a")
	second := hs.rejoin(t, first, "Ana", "client-a2")
	recorder := hs.contractAgentFor(t, first.ctx, "Ana")
	planner := hs.contractAgentFor(t, second.ctx, "Ana")

	hs.decRecord(t, recorder, goodDecision())
	env, _ := hs.decDeclare(t, planner, DeclareIntentParams{
		Summary: "store the session token in localStorage after login", Paths: []string{"web/src/auth/session.ts"}})

	env, text := hs.decJudge(t, planner, ReportJudgementParams{PairKey: decBlock(t, env).Pairs[0].PairKey,
		Verdict: "conflict", Confidence: conf(0.9), Rationale: "plan stores the token in localStorage"})
	t.Logf("same person, two agents: %s", text)
	if len(env.Conflicts) != 0 {
		t.Errorf("one person's own decision interrupted their own agent: %+v", env.Conflicts)
	}
	rows := hs.decConflicts(t)
	if len(rows) != 1 || enums.ConflictSeverity(rows[0].severity) != enums.CONFLICT_SEVERITY_LOW {
		t.Fatalf("conflict = %+v, want one low row", rows)
	}
	if !strings.Contains(rows[0].action, "which your own person decided") {
		t.Errorf("suggested_action = %q", rows[0].action)
	}
	if n := hs.decInstructionsFor(t, recorder.session); n != 0 {
		t.Errorf("the other agent of the same person got %d instruction(s)", n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `conflict_participant` WHERE `conflict_uuid` = ?", rows[0].uuid); n != 1 {
		t.Errorf("%d participants, want only the plan's owner", n)
	}
}

// TestIntegrationReportJudgementRefusals: every refusal in §3.3's table that a
// real call can reach.
func TestIntegrationReportJudgementRefusals(t *testing.T) {
	hs := newDecisionHarness(t)
	db := hs.core.DB()
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	cai := hs.contractAgent(t, "Cai", "client-c")
	hs.decRecord(t, ana, goodDecision())

	bobEnv, _ := hs.decDeclare(t, bob, DeclareIntentParams{Summary: "store the token in localStorage", Paths: []string{"web/src/auth/session.ts"}})
	bobIntent, bobPair := bobEnv.Key, decBlock(t, bobEnv).Pairs[0].PairKey
	caiEnv, _ := hs.decDeclare(t, cai, DeclareIntentParams{Summary: "add the logout button", Paths: []string{"web/src/auth/logout.ts"}})
	caiPair := decBlock(t, caiEnv).Pairs[0].PairKey

	judge := ReportJudgementParams{Verdict: "conflict", Confidence: conf(0.9), Rationale: "plan stores the token in localStorage"}

	unknown := judge
	unknown.PairKey = "nope"
	if err := hs.decJudgeErr(t, bob, unknown); err != "pair_key nope is not a pair on this team — call get_review_context" {
		t.Errorf("unknown pair: %q", err)
	}
	notMine := judge
	notMine.PairKey = caiPair
	if err := hs.decJudgeErr(t, bob, notMine); err != fmt.Sprintf("this pair is %s's to judge, not yours", cai.key) {
		t.Errorf("another session's pair: %q", err)
	}

	// A changed plan makes the pair stale.
	hs.decUpdate(t, bob, UpdateIntentParams{IntentKey: bobIntent, Summary: "read the session from the httpOnly cookie"})
	stale := judge
	stale.PairKey = bobPair
	if err := hs.decJudgeErr(t, bob, stale); err != fmt.Sprintf(
		"%s is now revision 2 and you judged revision 1: call get_review_context for the current pair", bobIntent) {
		t.Errorf("stale plan: %q", err)
	}

	// A changed decision does the same.
	revised := goodDecision()
	revised.Statement = "Sessions are a signed JWT in an httpOnly cookie, and nothing else may hold one."
	hs.decRecord(t, ana, revised)
	staleDecision := judge
	staleDecision.PairKey = caiPair
	if err := hs.decJudgeErr(t, cai, staleDecision); err != "#auth-jwt-cookie is now revision 2 and you judged revision 1: call get_review_context for the current pair" {
		t.Errorf("stale decision: %q", err)
	}

	// A plan that is over, and a decision that is over.
	fresh := hs.decJudgements(t)
	var caiCurrent string
	for _, r := range fresh {
		if r.judge.Valid && r.judge.String == cai.session.String() && r.decisionRev == 2 {
			caiCurrent = r.pairKey
		}
	}
	if caiCurrent == "" {
		t.Fatal("the revised decision did not earn Cai's plan a new pair")
	}
	if _, err := db.Exec("UPDATE `intent` SET `status` = ? WHERE `team_uuid` = ? AND `key` = ?",
		int64(enums.INTENT_STATUS_DONE), hs.teamID.String(), caiEnv.Key); err != nil {
		t.Fatal(err)
	}
	ended := judge
	ended.PairKey = caiCurrent
	if err := hs.decJudgeErr(t, cai, ended); err != fmt.Sprintf("%s is done; nothing to judge", caiEnv.Key) {
		t.Errorf("finished plan: %q", err)
	}
	if _, err := db.Exec("UPDATE `intent` SET `status` = ? WHERE `team_uuid` = ? AND `key` = ?",
		int64(enums.INTENT_STATUS_DECLARED), hs.teamID.String(), caiEnv.Key); err != nil {
		t.Fatal(err)
	}
	hs.decRecord(t, ana, RecordDecisionParams{Key: "auth-jwt-cookie", Revoke: true, Rationale: "moving to server sessions"})
	if err := hs.decJudgeErr(t, cai, ended); err != "#auth-jwt-cookie was revoked; nothing to judge" {
		t.Errorf("withdrawn decision: %q", err)
	}

	// A pair the plan's own end expired.
	hs.decUpdate(t, bob, UpdateIntentParams{IntentKey: bobIntent, Status: "done"})
	expired := judge
	expired.PairKey = bobPair
	if err := hs.decJudgeErr(t, bob, expired); err != "this pair expired unanswered; call get_review_context for what is waiting now" {
		t.Errorf("expired pair: %q", err)
	}
}

// TestIntegrationReportJudgementSanitizesTheRationale: what an agent typed
// reaches the board masked (§3.0).
func TestIntegrationReportJudgementSanitizesTheRationale(t *testing.T) {
	hs := newDecisionHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	_, pair := hs.decPair(t, ana, bob, "store the session token in localStorage after login")

	_, text := hs.decJudge(t, bob, ReportJudgementParams{PairKey: pair, Verdict: "conflict", Confidence: conf(0.9),
		Rationale: `plan writes mtk_liveagenttoken with password=hunter2 into localStorage`})
	t.Logf("verdict with a credential in the rationale: %s", text)
	for _, secret := range []string{"mtk_liveagenttoken", "hunter2"} {
		if strings.Contains(text, secret) {
			t.Errorf("the response carries %q", secret)
		}
	}
	rows := hs.decJudgements(t)
	if len(rows) != 1 || !rows[0].rationale.Valid {
		t.Fatalf("ledger = %+v", rows)
	}
	if strings.Contains(rows[0].rationale.String, "hunter2") || strings.Contains(rows[0].rationale.String, "mtk_live") {
		t.Errorf("the stored rationale carries a credential: %q", rows[0].rationale.String)
	}
	if !strings.Contains(rows[0].rationale.String, "[redacted]") {
		t.Errorf("rationale = %q, want the credentials masked", rows[0].rationale.String)
	}
	assertNoSecretsInEventLog(t, hs, "mtk_liveagenttoken", "hunter2")
}

// ─────────────────────────────────────────────
// Settlement (§4.5, §4.6, §4.8)
// ─────────────────────────────────────────────

// decConflictOnly returns the single decision conflict, failing otherwise.
func (hs *harness) decConflictOnly(t *testing.T) decConflict {
	t.Helper()
	rows := hs.decConflicts(t)
	if len(rows) != 1 {
		t.Fatalf("%d decision conflicts, want one", len(rows))
	}
	return rows[0]
}

// TestIntegrationDecisionConflictConvergesWhenThePlanChanges is the whole
// point of the feature: the agents settle it between themselves (§4.5 rule 4).
func TestIntegrationDecisionConflictConvergesWhenThePlanChanges(t *testing.T) {
	hs := newDecisionHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	intent, pair := hs.decPair(t, ana, bob, "store the session token in localStorage after login")
	hs.decJudge(t, bob, ReportJudgementParams{PairKey: pair, Verdict: "conflict", Confidence: conf(0.9),
		Rationale: "plan stores the token in localStorage; #auth-jwt-cookie forbids it"})
	conflict := hs.decConflictOnly(t)

	env, _ := hs.decUpdate(t, bob, UpdateIntentParams{IntentKey: intent, Summary: "read the session from the httpOnly cookie"})
	next := decBlock(t, env).Pairs[0].PairKey
	if next == pair {
		t.Fatal("the revised plan did not earn a new pair")
	}
	env, text := hs.decJudge(t, bob, ReportJudgementParams{PairKey: next, Verdict: "no_conflict", Confidence: conf(0.95),
		Rationale: "plan now relies on the httpOnly cookie"})
	t.Logf("the verdict that settles it: %s", text)
	if env.Key != conflict.key || !strings.HasSuffix(env.Note, fmt.Sprintf("; %s settled", conflict.key)) {
		t.Errorf("envelope key %q note %q", env.Key, env.Note)
	}

	settled := hs.decConflictOnly(t)
	if enums.ConflictStatus(settled.status) != enums.CONFLICT_STATUS_RESOLVED ||
		enums.ConflictResolution(settled.resolution) != enums.CONFLICT_RESOLUTION_CONVERGED {
		t.Fatalf("conflict = status %s resolution %s", enums.ConflictStatus(settled.status).String(), enums.ConflictResolution(settled.resolution).String())
	}
	t.Logf("resolution note: %s", settled.note)
	if !strings.HasPrefix(settled.note, fmt.Sprintf("Settled by the agents: %s (test) revised %s at ", bob.key, intent)) ||
		!strings.HasSuffix(settled.note, `and judged it no longer contradicts #auth-jwt-cookie (r1): "plan now relies on the httpOnly cookie".`) {
		t.Errorf("note = %q", settled.note)
	}
	if settled.resolvedBy.Valid {
		t.Error("metiche closed it, so resolved_by_member_uuid must stay NULL")
	}
	var summary string
	if err := hs.core.DB().QueryRow("SELECT `summary` FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		hs.teamID.String(), int64(enums.EVENT_KIND_CONFLICT_RESOLVED)).Scan(&summary); err != nil {
		t.Fatalf("no conflict_resolved event: %v", err)
	}
	t.Logf("conflict_resolved: %s", summary)
	if summary != fmt.Sprintf("%s settled (converged): the plan no longer contradicts #auth-jwt-cookie", conflict.key) {
		t.Errorf("summary = %q", summary)
	}
}

// TestIntegrationDecisionConflictConvergesWhenTheDecisionChanges: the other
// way round, and the note says who moved (§4.8).
func TestIntegrationDecisionConflictConvergesWhenTheDecisionChanges(t *testing.T) {
	hs := newDecisionHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	intent, pair := hs.decPair(t, ana, bob, "store the session token in localStorage after login")
	hs.decJudge(t, bob, ReportJudgementParams{PairKey: pair, Verdict: "conflict", Confidence: conf(0.9),
		Rationale: "plan stores the token in localStorage; #auth-jwt-cookie forbids it"})

	revised := goodDecision()
	revised.Statement = "Sessions may be a JWT in an httpOnly cookie or in browser storage, as long as they expire in an hour."
	env, text := hs.decRecord(t, ana, revised)
	t.Logf("Ana revises the decision instead: %s", text)
	if !strings.Contains(env.Note, "1 live plan(s) will be checked against the new wording") {
		t.Errorf("note = %q", env.Note)
	}
	var next string
	for _, r := range hs.decJudgements(t) {
		if r.status == enums.JUDGEMENT_STATUS_PENDING && r.decisionRev == 2 {
			next = r.pairKey
		}
	}
	if next == "" {
		t.Fatal("the new wording did not earn the plan a new pair")
	}
	_, text = hs.decJudge(t, bob, ReportJudgementParams{PairKey: next, Verdict: "no_conflict", Confidence: conf(0.9),
		Rationale: "the new wording allows browser storage"})
	t.Logf("Bob re-judges: %s", text)

	settled := hs.decConflictOnly(t)
	t.Logf("resolution note: %s", settled.note)
	if enums.ConflictResolution(settled.resolution) != enums.CONFLICT_RESOLUTION_CONVERGED {
		t.Fatalf("resolution = %s", enums.ConflictResolution(settled.resolution).String())
	}
	if !strings.HasPrefix(settled.note, fmt.Sprintf("Settled by the agents: %s (test) revised #auth-jwt-cookie to r2 at ", ana.key)) ||
		!strings.Contains(settled.note, fmt.Sprintf("and %s (test) judged %s no longer contradicts it", bob.key, intent)) {
		t.Errorf("note = %q", settled.note)
	}
}

// TestIntegrationDecisionConflictSupersededWhenTheWorkEnds: a finished plan
// and an ended session close their conflicts and expire their pairs (§4.5).
func TestIntegrationDecisionConflictSupersededWhenTheWorkEnds(t *testing.T) {
	for _, tc := range []struct{ name, how string }{{"the plan is done", "intent"}, {"the session ends", "session"}} {
		t.Run(tc.name, func(t *testing.T) {
			hs := newDecisionHarness(t)
			ana := hs.contractAgent(t, "Ana", "client-a")
			bob := hs.contractAgent(t, "Bob", "client-b")
			intent, pair := hs.decPair(t, ana, bob, "store the session token in localStorage after login")
			hs.decJudge(t, bob, ReportJudgementParams{PairKey: pair, Verdict: "conflict", Confidence: conf(0.9),
				Rationale: "plan stores the token in localStorage"})
			// A second plan of Bob's, with a pair still waiting.
			hs.decDeclare(t, bob, DeclareIntentParams{Summary: "wire the logout button", Paths: []string{"web/src/auth/logout.ts"}})
			if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `judgement` WHERE `status` = ?", int64(enums.JUDGEMENT_STATUS_PENDING)); n != 1 {
				t.Fatalf("setup: %d pending pairs, want 1", n)
			}

			if tc.how == "intent" {
				_, text := hs.decUpdate(t, bob, UpdateIntentParams{IntentKey: intent, Status: "done"})
				t.Logf("update_intent done: %s", text)
			} else {
				res, _, err := hs.h.EndSession(bob.ctx, nil, EndSessionParams{SessionKey: bob.key, Note: "login screen shipped"})
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("end_session: %s", resultText(t, res))
			}

			settled := hs.decConflictOnly(t)
			t.Logf("resolution note: %s", settled.note)
			if enums.ConflictStatus(settled.status) != enums.CONFLICT_STATUS_RESOLVED ||
				enums.ConflictResolution(settled.resolution) != enums.CONFLICT_RESOLUTION_SUPERSEDED {
				t.Fatalf("conflict = status %s resolution %s", enums.ConflictStatus(settled.status).String(),
					enums.ConflictResolution(settled.resolution).String())
			}
			if tc.how == "intent" {
				if !strings.HasPrefix(settled.note, fmt.Sprintf("Settled by the agents: %s (test) marked %s done at ", bob.key, intent)) {
					t.Errorf("note = %q", settled.note)
				}
			} else if !strings.HasPrefix(settled.note, fmt.Sprintf("Settled by the agents: %s (test) ended its session at ", bob.key)) ||
				!strings.Contains(settled.note, "succeeded: login screen shipped") {
				t.Errorf("note = %q", settled.note)
			}

			// Its pairs to judge are expired: nothing is waiting on work that
			// is over.
			pending := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `judgement` WHERE `status` = ?", int64(enums.JUDGEMENT_STATUS_PENDING))
			wantPending := 1 // the second plan's pair survives a finished plan
			if tc.how == "session" {
				wantPending = 0
			}
			if pending != wantPending {
				t.Errorf("%d pending pairs, want %d", pending, wantPending)
			}
			// The decisions themselves stand: they are the team's, not the
			// session's (F12).
			if _, status, _, _, _, _, _ := hs.decDecision(t, "#auth-jwt-cookie"); status != enums.DECISION_STATUS_ACCEPTED {
				t.Errorf("the decision is %s", status.String())
			}
		})
	}
}

// TestIntegrationDecisionConflictSupersededWhenTheDecisionGoes: superseding or
// revoking a decision settles the conflicts on it (§3.1, §4.5).
func TestIntegrationDecisionConflictSupersededWhenTheDecisionGoes(t *testing.T) {
	hs := newDecisionHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	intent, pair := hs.decPair(t, ana, bob, "store the session token in localStorage after login")
	hs.decJudge(t, bob, ReportJudgementParams{PairKey: pair, Verdict: "conflict", Confidence: conf(0.9),
		Rationale: "plan stores the token in localStorage"})

	env, text := hs.decRecord(t, ana, RecordDecisionParams{
		Key: "auth-server-session", Title: "Sessions live on the server", Supersedes: "auth-jwt-cookie",
		Statement: "The server holds session state in redis and the browser holds only an opaque identifier.",
		Scope:     []string{"internal/auth/**"}})
	t.Logf("supersede: %s", text)
	if !strings.Contains(env.Note, "; superseded #auth-jwt-cookie, 1 conflict(s) on it settled") {
		t.Errorf("note = %q", env.Note)
	}
	settled := hs.decConflictOnly(t)
	t.Logf("resolution note: %s", settled.note)
	if enums.ConflictResolution(settled.resolution) != enums.CONFLICT_RESOLUTION_SUPERSEDED {
		t.Fatalf("resolution = %s", enums.ConflictResolution(settled.resolution).String())
	}
	if !strings.HasPrefix(settled.note, fmt.Sprintf("Settled by the agents: %s (test) superseded #auth-jwt-cookie with #auth-server-session at ", ana.key)) ||
		!strings.HasSuffix(settled.note, fmt.Sprintf("so %s no longer breaks a standing decision.", intent)) {
		t.Errorf("note = %q", settled.note)
	}
	if _, status, _, _, _, _, _ := hs.decDecision(t, "#auth-jwt-cookie"); status != enums.DECISION_STATUS_SUPERSEDED {
		t.Errorf("the old decision is %s", status.String())
	}
	// Recording the superseded key again says where it went.
	err := hs.decRecordErr(t, ana, goodDecision())
	t.Logf("recording the superseded key: %s", err)
	if !strings.HasPrefix(err, "#auth-jwt-cookie was superseded by #auth-server-session at ") ||
		!strings.HasSuffix(err, "; record a new key or revise #auth-server-session") {
		t.Errorf("refusal = %q", err)
	}
}

// TestIntegrationDecisionConflictReopensThenIsSilenced: a conflict metiche
// closed comes back, clearing escalated_at; one a person closed is only
// counted (§4.6).
func TestIntegrationDecisionConflictReopensThenIsSilenced(t *testing.T) {
	hs := newDecisionHarness(t)
	db := hs.core.DB()
	ana := hs.contractAgent(t, "Ana", "client-a")
	bob := hs.contractAgent(t, "Bob", "client-b")
	intent, pair := hs.decPair(t, ana, bob, "store the session token in localStorage after login")
	hs.decJudge(t, bob, ReportJudgementParams{PairKey: pair, Verdict: "conflict", Confidence: conf(0.9),
		Rationale: "plan stores the token in localStorage"})
	first := hs.decConflictOnly(t)

	// The agents settle it, and a person had been asked along the way.
	env, _ := hs.decUpdate(t, bob, UpdateIntentParams{IntentKey: intent, Summary: "read the session from the httpOnly cookie"})
	hs.decJudge(t, bob, ReportJudgementParams{PairKey: decBlock(t, env).Pairs[0].PairKey, Verdict: "no_conflict",
		Confidence: conf(0.95), Rationale: "plan now relies on the httpOnly cookie"})
	if _, err := db.Exec("UPDATE `conflict` SET `escalated_at` = ? WHERE `id` = ?", time.Now().UTC(), first.uuid); err != nil {
		t.Fatal(err)
	}

	// The plan goes back to what it was: the same conflict is news again.
	env, _ = hs.decUpdate(t, bob, UpdateIntentParams{IntentKey: intent, Summary: "keep the session token in localStorage after all"})
	env, text := hs.decJudge(t, bob, ReportJudgementParams{PairKey: decBlock(t, env).Pairs[0].PairKey, Verdict: "conflict",
		Confidence: conf(0.9), Rationale: "the plan stores the token in localStorage again"})
	t.Logf("the same disagreement again: %s", text)
	reopened := hs.decConflictOnly(t)
	if reopened.key != first.key {
		t.Fatalf("a second conflict row appeared: %s then %s", first.key, reopened.key)
	}
	if enums.ConflictStatus(reopened.status) != enums.CONFLICT_STATUS_OPEN || reopened.resolution != 0 || reopened.note != "" {
		t.Errorf("reopened row = %+v", reopened)
	}
	if reopened.escalated.Valid {
		t.Error("reopening must clear escalated_at, or the conflict can never be escalated again")
	}
	if reopened.occurrences < 2 {
		t.Errorf("occurrence_count = %d", reopened.occurrences)
	}
	if len(env.Conflicts) != 1 {
		t.Errorf("the caller was not told about the reopened conflict: %+v", env.Conflicts)
	}
	notices := hs.decInstructionsFor(t, ana.session)
	if notices != 2 {
		t.Errorf("the decider has %d notices, want 2 (one per time it was news)", notices)
	}

	// A person closes it. From here it is counted and never announced again.
	if _, err := db.Exec("UPDATE `conflict` SET `status` = ?, `resolution` = ?, `resolved_by_member_uuid` = ?, `resolved_at` = ? WHERE `id` = ?",
		int64(enums.CONFLICT_STATUS_RESOLVED), int64(enums.CONFLICT_RESOLUTION_NOT_A_CONFLICT), hs.decMember(t, ana), time.Now().UTC(), first.uuid); err != nil {
		t.Fatal(err)
	}
	env, _ = hs.decUpdate(t, bob, UpdateIntentParams{IntentKey: intent, Summary: "store the token in localStorage, third time"})
	env, text = hs.decJudge(t, bob, ReportJudgementParams{PairKey: decBlock(t, env).Pairs[0].PairKey, Verdict: "conflict",
		Confidence: conf(0.95), Rationale: "still localStorage"})
	t.Logf("after a person settled it: %s", text)
	if len(env.Conflicts) != 0 {
		t.Errorf("a silenced conflict was announced again: %+v", env.Conflicts)
	}
	if env.Note != fmt.Sprintf("judged %s against #auth-jwt-cookie: conflict (0.95); a person already settled this one, so it stays closed", intent) {
		t.Errorf("note = %q", env.Note)
	}
	silenced := hs.decConflictOnly(t)
	if enums.ConflictStatus(silenced.status) != enums.CONFLICT_STATUS_RESOLVED || silenced.occurrences <= reopened.occurrences {
		t.Errorf("silenced row = %+v", silenced)
	}
	if n := hs.decInstructionsFor(t, ana.session); n != notices {
		t.Errorf("a silenced conflict raised %d new notice(s)", n-notices)
	}
}

// ─────────────────────────────────────────────
// Concurrency and the lock
// ─────────────────────────────────────────────

// TestIntegrationDecisionConcurrentDeclarations: eight sessions declaring onto
// one decision's scope at once produce one judgement per pair, a gapless
// sequence, and a lock hold well inside the tripwire (§7.2).
func TestIntegrationDecisionConcurrentDeclarations(t *testing.T) {
	hs := newDecisionHarness(t)
	ana := hs.contractAgent(t, "Ana", "client-a")
	hs.decRecord(t, ana, goodDecision())

	var agents []contractAgent
	for i, name := range []string{"Bob", "Cai", "Dee", "Eve"} {
		c := hs.join(t, name, fmt.Sprintf("client-%d", i))
		agents = append(agents, hs.contractAgentFor(t, c.ctx, name), hs.contractAgentFor(t, c.ctx, name))
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		errs  []error
		start = make(chan struct{})
	)
	for i, a := range agents {
		for retry := 0; retry < 2; retry++ {
			wg.Add(1)
			go func(i int, a contractAgent) {
				defer wg.Done()
				<-start
				_, _, err := hs.h.DeclareIntent(a.ctx, nil, DeclareIntentParams{
					SessionKey:     a.key,
					Summary:        fmt.Sprintf("store the session token in localStorage, plan %d", i),
					Paths:          []string{fmt.Sprintf("web/src/auth/f%d.ts", i)},
					IdempotencyKey: fmt.Sprintf("concurrent-declare-%d", i),
				})
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}(i, a)
		}
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		t.Errorf("a concurrent declare_intent failed: %v", err)
	}

	db := hs.core.DB()
	if n := countRows(t, db, "SELECT COUNT(*) FROM `intent`"); n != len(agents) {
		t.Errorf("%d intents, want %d: a retry declared a second one", n, len(agents))
	}
	rows := hs.decJudgements(t)
	if len(rows) != len(agents) {
		t.Fatalf("%d judgements, want one per plan (%d)", len(rows), len(agents))
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.pairKey] {
			t.Errorf("pair %s was written twice", r.pairKey)
		}
		seen[r.pairKey] = true
	}
	if n := countRows(t, db, "SELECT COUNT(DISTINCT `pair_key`) FROM `judgement`"); n != len(agents) {
		t.Errorf("%d distinct pair keys, want %d", n, len(agents))
	}

	// The log is gapless and the team's cursor is not behind it.
	var last, teamSeq int64
	if err := db.QueryRow("SELECT COALESCE(MAX(`sequence`), 0), COUNT(*) FROM `team_event` WHERE `team_uuid` = ?", hs.teamID.String()).
		Scan(&last, &teamSeq); err != nil {
		t.Fatal(err)
	}
	if last != teamSeq {
		t.Errorf("the log ends at %d with %d events: there is a hole", last, teamSeq)
	}
	var cursor int64
	if err := db.QueryRow("SELECT `sequence` FROM `team` WHERE `id` = ?", hs.teamID.String()).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if cursor != last {
		t.Errorf("team.sequence = %d but the log ends at %d", cursor, last)
	}

	stats := hs.h.LockHoldStats()
	t.Logf("%s: count=%d p50=%.0f p95=%.0f p99=%.0f max=%.2f mean=%.2f",
		stats.Name, stats.Count, stats.P50MS, stats.P95MS, stats.P99MS, stats.MaxMS, stats.MeanMS)
	if stats.P99MS > LockHoldWarnMS {
		t.Errorf("%s p99 = %.0fms, over the %dms tripwire", stats.Name, stats.P99MS, LockHoldWarnMS)
	}
}

// ─────────────────────────────────────────────
// Through the real transport
// ─────────────────────────────────────────────

// decisionServer mounts the production wiring behind httptest, including the
// chained detector app/rest.go installs.
func decisionServer(t *testing.T, hs *harness) string {
	t.Helper()
	t.Setenv("METICHE_ROLE", "all")
	t.Setenv("METICHE_JOIN_PER_HOUR", "1000")
	r := chi.NewRouter()
	handler := Register(r, hs.core, zap.NewNop())
	handler.SetDetector(ChainDetectors(
		NewPathDetector(hs.core, zap.NewNop()),
		NewDecisionReviewer(hs.core, zap.NewNop())))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { r.ServeHTTP(w, req) }))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1/mcp"
}

func decDecode(t *testing.T, text string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(text), into); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, text)
	}
}

// TestIntegrationDecisionsTwoAgentsOverTheTransport is §7.4 steps 1-5: Ana
// records a decision, Bob's plan meets it, Bob's own model judges it, the
// decider is told once, and the conflict settles itself when the plan changes.
func TestIntegrationDecisionsTwoAgentsOverTheTransport(t *testing.T) {
	hs := newHarness(t)
	endpoint := decisionServer(t, hs)
	slug := hs.teamSlug(t)
	ana := hs.join(t, "Ana", "client-ana")
	bob := hs.join(t, "Bob", "client-bob")
	csA, csB := connectAs(t, endpoint, ana.token), connectAs(t, endpoint, bob.token)

	// 1. Ana records #auth-jwt-cookie.
	sA := startSessionOn(t, csA, slug, map[string]any{"goal": "the auth service"})
	text := mustOK(t, csA, "record_decision", map[string]any{
		"session_key": sA.Key, "team_slug": slug, "key": "auth-jwt-cookie",
		"title":     "Auth is a JWT in an httpOnly cookie",
		"statement": "Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage or sessionStorage.",
		"scope":     []string{"internal/auth/**", "web/src/auth/**"},
	})
	t.Logf("1. Ana record_decision:\n%s", text)
	var recorded Envelope
	decDecode(t, text, &recorded)
	if recorded.Key != "#auth-jwt-cookie" || !strings.Contains(recorded.Note, "no live plan touches it yet") {
		t.Fatalf("record_decision = %+v", recorded)
	}

	// 2. Bob declares a plan inside its scope and is handed the pair.
	sB := startSessionOn(t, csB, slug, map[string]any{"goal": "the login screen"})
	text = mustOK(t, csB, "declare_intent", map[string]any{
		"session_key": sB.Key, "team_slug": slug,
		"summary": "store the session token in localStorage after login",
		"paths":   []string{"web/src/auth/session.ts"}, "mode": "write",
	})
	t.Logf("2. Bob declare_intent:\n%s", text)
	var declared Envelope
	decDecode(t, text, &declared)
	block := decBlock(t, declared)
	if len(block.Pairs) != 1 || block.Pairs[0].Decision != "#auth-jwt-cookie" || block.Pairs[0].Why != "scope" {
		t.Fatalf("review block = %+v", block)
	}
	if declared.Pending.Reviews != 1 {
		t.Fatalf("pending.reviews = %d, want 1", declared.Pending.Reviews)
	}
	intent := declared.Key

	// 3. Bob reads the pair and answers it.
	text = mustOK(t, csB, "get_review_context", map[string]any{"session_key": sB.Key, "team_slug": slug})
	t.Logf("3. Bob get_review_context:\n%s", text)
	var context ReviewContextResult
	decDecode(t, text, &context)
	if len(context.Reviews) != 1 || context.Reviews[0].PairKey != block.Pairs[0].PairKey {
		t.Fatalf("review context = %+v", context.Reviews)
	}
	text = mustOK(t, csB, "report_judgement", map[string]any{
		"session_key": sB.Key, "team_slug": slug, "pair_key": block.Pairs[0].PairKey,
		"verdict": "conflict", "confidence": 0.9,
		"rationale": "plan stores the token in localStorage; #auth-jwt-cookie forbids it",
	})
	t.Logf("3. Bob report_judgement conflict 0.9:\n%s", text)
	var judged Envelope
	decDecode(t, text, &judged)
	if len(judged.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v", judged.Conflicts)
	}
	cf := judged.Conflicts[0]
	if cf.AtFault != "plan" || cf.Decision != "#auth-jwt-cookie" || cf.Kind != "decision_contradiction" || cf.With != "Ana (test)" {
		t.Fatalf("conflict notice = %+v", cf)
	}

	// 4. Ana finds out on her next heartbeat, and collects the notice.
	text = mustOK(t, csA, "heartbeat", map[string]any{"session_key": sA.Key, "team_slug": slug})
	t.Logf("4. Ana heartbeat:\n%s", text)
	var beat Envelope
	decDecode(t, text, &beat)
	if beat.Pending.Instructions != 1 || beat.Pending.Conflicts != 1 {
		t.Fatalf("Ana's pending = %+v, want 1 instruction and 1 conflict", beat.Pending)
	}
	text = mustOK(t, csA, "get_instructions", map[string]any{"session_key": sA.Key, "team_slug": slug})
	t.Logf("4. Ana get_instructions:\n%s", text)
	var delivered InstructionsResult
	decDecode(t, text, &delivered)
	if len(delivered.Instructions) != 1 || delivered.Instructions[0].Ref != cf.Key ||
		delivered.Instructions[0].Kind != "conflict_notice" {
		t.Fatalf("Ana's instructions = %+v", delivered.Instructions)
	}

	// 5. Bob changes the plan, is asked once more, and the conflict settles.
	text = mustOK(t, csB, "update_intent", map[string]any{
		"session_key": sB.Key, "team_slug": slug, "intent_key": intent,
		"summary": "read the session from the httpOnly cookie the login endpoint sets",
	})
	t.Logf("5. Bob update_intent:\n%s", text)
	var updated Envelope
	decDecode(t, text, &updated)
	nextBlock := decBlock(t, updated)
	if updated.Pending.Reviews != 1 || len(nextBlock.Pairs) != 1 || nextBlock.Pairs[0].PairKey == block.Pairs[0].PairKey {
		t.Fatalf("the revised plan should earn one new pair: %+v", updated)
	}
	text = mustOK(t, csB, "report_judgement", map[string]any{
		"session_key": sB.Key, "team_slug": slug, "pair_key": nextBlock.Pairs[0].PairKey,
		"verdict": "no_conflict", "confidence": 0.95, "rationale": "plan now relies on the httpOnly cookie",
	})
	t.Logf("5. Bob report_judgement no_conflict:\n%s", text)
	var settledEnv Envelope
	decDecode(t, text, &settledEnv)
	if !strings.HasSuffix(settledEnv.Note, fmt.Sprintf("; %s settled", cf.Key)) {
		t.Fatalf("note = %q", settledEnv.Note)
	}

	var (
		status, resolution int64
		note               string
		resolvedAt         time.Time
	)
	if err := hs.core.DB().QueryRow(
		"SELECT `status`, COALESCE(`resolution`, 0), COALESCE(`resolution_note`, ''), `resolved_at` FROM `conflict` WHERE `team_uuid` = ? AND `key` = ?",
		hs.teamID.String(), cf.Key).Scan(&status, &resolution, &note, &resolvedAt); err != nil {
		t.Fatal(err)
	}
	t.Logf("the settled conflict: status=%s resolution=%s\n%s",
		enums.ConflictStatus(status).String(), enums.ConflictResolution(resolution).String(), note)
	if enums.ConflictStatus(status) != enums.CONFLICT_STATUS_RESOLVED ||
		enums.ConflictResolution(resolution) != enums.CONFLICT_RESOLUTION_CONVERGED {
		t.Fatalf("conflict = %s / %s", enums.ConflictStatus(status).String(), enums.ConflictResolution(resolution).String())
	}
	want := fmt.Sprintf("Settled by the agents: %s (test) revised %s at %s and judged it no longer contradicts #auth-jwt-cookie (r1): \"plan now relies on the httpOnly cookie\".",
		sB.Key, intent, clock(resolvedAt))
	if note != want {
		t.Errorf("resolution note = %q\nwant              %q", note, want)
	}

	// 7. Exactly one interruption to Ana over the whole run.
	var anaSession uuid.UUID
	if who, err := hs.h.RequireSession(ana.ctx, sA.Key); err == nil {
		anaSession = who.Session.ID
	} else {
		t.Fatal(err)
	}
	if n := hs.decInstructionsFor(t, anaSession); n != 1 {
		t.Errorf("Ana was interrupted %d time(s), want exactly 1", n)
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		hs.teamID.String(), int64(enums.EVENT_KIND_JUDGEMENT_REPORTED)); n != 2 {
		t.Errorf("%d judgement_reported events, want 2", n)
	}
}

// TestIntegrationDecisionToolsNeedTeamSlugAcrossTeams: session keys are per
// team, so all three tools take team_slug and resolve through
// RequireSessionOnTeam (§3.0).
func TestIntegrationDecisionToolsNeedTeamSlugAcrossTeams(t *testing.T) {
	hs := newHarness(t)
	endpoint := decisionServer(t, hs)
	slugA := hs.teamSlug(t)
	teamB, slugB, codeB := hs.otherTeam(t)

	ana := hs.join(t, "Ana", "client-a")
	hs.joinWith(t, ana.ctx, ana.token, codeB, "Ana", "client-a")
	cs := connectAs(t, endpoint, ana.token)

	a, b := startSessionOn(t, cs, slugA, nil), startSessionOn(t, cs, slugB, nil)
	rekey(t, hs, hs.teamID, map[string]string{a.Key: "S-77"})
	rekey(t, hs, teamB, map[string]string{b.Key: "S-77"})

	record := map[string]any{
		"session_key": "S-77", "key": "auth-jwt-cookie", "title": "Auth is a JWT in an httpOnly cookie",
		"statement": "Sessions are a signed JWT in an httpOnly, Secure cookie and nowhere else.",
	}
	for tool, args := range map[string]map[string]any{
		"record_decision":    record,
		"get_review_context": {"session_key": "S-77"},
		"report_judgement": {"session_key": "S-77", "pair_key": "nope", "verdict": "conflict",
			"confidence": 0.9, "rationale": "the plan breaks it"},
	} {
		text := mustRefuse(t, cs, tool, args)
		t.Logf("%s with a bare session_key: %s", tool, text)
		if !strings.Contains(text, slugA) || !strings.Contains(text, slugB) || !strings.Contains(text, "team_slug") {
			t.Errorf("%s should name both teams and team_slug: %s", tool, text)
		}
	}
	if n := countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `decision`"); n != 0 {
		t.Fatalf("a refused record_decision wrote %d decision(s)", n)
	}

	// With the team named, each one lands on that team.
	onB := map[string]any{"team_slug": slugB}
	for k, v := range record {
		onB[k] = v
	}
	t.Logf("record_decision with team_slug: %s", mustOK(t, cs, "record_decision", onB))
	var team string
	if err := hs.core.DB().QueryRow("SELECT `team_uuid` FROM `decision` WHERE `key` = ?", "#auth-jwt-cookie").Scan(&team); err != nil {
		t.Fatal(err)
	}
	if team != teamB.String() {
		t.Errorf("the decision landed on %s, want team B", team)
	}
	t.Logf("get_review_context with team_slug: %s", mustOK(t, cs, "get_review_context", map[string]any{"session_key": "S-77", "team_slug": slugB}))
	text := mustRefuse(t, cs, "report_judgement", map[string]any{"session_key": "S-77", "team_slug": slugB,
		"pair_key": "nope", "verdict": "no_conflict"})
	if !strings.Contains(text, "is not a pair on this team") {
		t.Errorf("report_judgement on team B: %s", text)
	}
}
