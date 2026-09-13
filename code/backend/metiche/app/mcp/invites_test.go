package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap/zaptest/observer"

	teammod "github.com/mklfarha/metiche/backend/core/module/team"
	team_types "github.com/mklfarha/metiche/backend/core/module/team/types"
	team_entity "github.com/mklfarha/metiche/backend/entity/team"
	"github.com/mklfarha/metiche/backend/enums"
)

// create_invite, list_invites and revoke_invite, through the real transport:
// the SDK's streamable client against mcp.Register's wiring behind httptest,
// with every log line captured. Every code in this file is an obvious fake.

// fakeCanaryCode is a valid Crockford-base32 code (no I, L, O or U), so it can
// be redeemed like a minted one.
const fakeCanaryCode = "FAKECANARY"

// ─────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────

// drawCodes makes create_invite's generator return these codes in order, the
// last one forever after.
func drawCodes(t *testing.T, codes ...string) {
	t.Helper()
	prev := mintInviteCode
	i := 0
	mintInviteCode = func() (string, error) {
		c := codes[i]
		if i < len(codes)-1 {
			i++
		}
		return c, nil
	}
	t.Cleanup(func() { mintInviteCode = prev })
}

// redeemCode is a brand-new person, with no token, redeeming a code.
func redeemCode(t *testing.T, endpoint, code, name string) (string, *mcp.CallToolResult, string) {
	t.Helper()
	anon := connectAs(t, endpoint, "")
	res, text := callTool(t, anon, "join_team", map[string]any{
		"join_code": code, "member_name": name, "agent_label": "test",
		"client_key": strings.ToLower(strings.ReplaceAll(name, " ", "-")) + "-laptop",
	})
	if res.IsError {
		return "", res, text
	}
	var out JoinTeamResult
	decodeResult(t, res, &out)
	return out.Token, res, text
}

func mustJoinWithCode(t *testing.T, endpoint, code, name string) string {
	t.Helper()
	token, res, text := redeemCode(t, endpoint, code, name)
	if res.IsError || token == "" {
		t.Fatalf("%s could not join with the code: %s", name, redactSecrets(text))
	}
	return token
}

// memberIDFor is the member row behind a token on a team.
func memberIDFor(t *testing.T, hs *harness, token string, teamID uuid.UUID) uuid.UUID {
	t.Helper()
	id, err := hs.h.resolveToken(context.Background(), token)
	if err != nil {
		t.Fatalf("the token does not resolve: %v", err)
	}
	m, found, err := hs.h.memberByAccount(context.Background(), nil, id.Account.ID, teamID)
	if err != nil || !found {
		t.Fatalf("no membership: found=%v err=%v", found, err)
	}
	return m.ID
}

func promoteToOwner(t *testing.T, hs *harness, token string) {
	t.Helper()
	if _, err := hs.core.DB().Exec("UPDATE `member` SET `role` = ? WHERE `id` = ?",
		enums.MEMBER_ROLE_OWNER, memberIDFor(t, hs, token, hs.teamID).String()); err != nil {
		t.Fatalf("promoting to owner: %v", err)
	}
}

func createInviteOK(t *testing.T, cs *mcp.ClientSession, args map[string]any) (CreateInviteResult, string) {
	t.Helper()
	res, text := callTool(t, cs, "create_invite", args)
	if res.IsError {
		t.Fatalf("create_invite %v refused: %s", args, text)
	}
	var out CreateInviteResult
	decodeResult(t, res, &out)
	return out, text
}

func listInvitesOK(t *testing.T, cs *mcp.ClientSession, args map[string]any) (ListInvitesResult, string) {
	t.Helper()
	res, text := callTool(t, cs, "list_invites", args)
	if res.IsError {
		t.Fatalf("list_invites refused: %s", text)
	}
	var out ListInvitesResult
	decodeResult(t, res, &out)
	return out, text
}

func revokeInviteOK(t *testing.T, cs *mcp.ClientSession, id string) RevokeInviteResult {
	t.Helper()
	res, text := callTool(t, cs, "revoke_invite", map[string]any{"invite_id": id})
	if res.IsError {
		t.Fatalf("revoke_invite %s refused: %s", id, text)
	}
	var out RevokeInviteResult
	decodeResult(t, res, &out)
	return out
}

// refused calls a tool that must fail and returns the error text.
func refused(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	res, text := callTool(t, cs, tool, args)
	if !res.IsError {
		t.Fatalf("%s %v was accepted, want a refusal: %s", tool, args, text)
	}
	return text
}

type inviteRowState struct {
	uses      int64
	status    enums.InviteStatus
	revokedAt sql.NullTime
	revokedBy sql.NullString
}

func readInviteRow(t *testing.T, hs *harness, id string) inviteRowState {
	t.Helper()
	var r inviteRowState
	var status int64
	if err := hs.core.DB().QueryRow(
		"SELECT `uses`, `status`, `revoked_at`, `revoked_by_member_uuid` FROM `invite` WHERE `id` = ?", id).
		Scan(&r.uses, &status, &r.revokedAt, &r.revokedBy); err != nil {
		t.Fatalf("reading invite %s: %v", id, err)
	}
	r.status = enums.InviteStatus(status)
	return r
}

func teamInviteCount(t *testing.T, hs *harness, teamID uuid.UUID) int {
	t.Helper()
	return countRows(t, hs.core.DB(), "SELECT COUNT(*) FROM `invite` WHERE `team_uuid` = ?", teamID.String())
}

// seedOtherTeam is a second team with one uncapped invite, for cross-team
// cases. Returns the team's id and slug and the invite's id.
func seedOtherTeam(t *testing.T, hs *harness, code string) (uuid.UUID, string, string) {
	t.Helper()
	teamID := uuid.Must(uuid.NewV4())
	planID := hs.planID
	slug := "other-" + teamID.String()[:8]
	if _, err := hs.core.Team().Insert(context.Background(), team_types.UpsertRequest{
		Team: team_entity.Team{
			ID: teamID, Name: "Other team", Slug: slug, Status: enums.RECORD_STATUS_ACTIVE,
			PlanUUID: &planID, PlanSource: enums.PLAN_SOURCE_INSTANCE_DEFAULT,
			Visibility: enums.TEAM_VISIBILITY_PRIVATE,
		},
	}, teammod.WithSkipCache()); err != nil {
		t.Fatalf("seeding the other team: %v", err)
	}
	inviteID := uuid.Must(uuid.NewV4()).String()
	if _, err := hs.core.DB().Exec(
		"INSERT INTO `invite` (`id`,`team_uuid`,`code`,`label`,`uses`,`status`) VALUES (?,?,?,?,?,?)",
		inviteID, teamID.String(), code, "other team", 0, enums.INVITE_STATUS_ACTIVE); err != nil {
		t.Fatalf("seeding the other team's invite: %v", err)
	}
	return teamID, slug, inviteID
}

func findInvite(list ListInvitesResult, id string) (InviteInfo, bool) {
	for _, inv := range list.Invites {
		if inv.InviteID == id {
			return inv, true
		}
	}
	return InviteInfo{}, false
}

// logsMention reports whether any captured log line — message or any field —
// contains s, case-insensitively.
func logsMention(logs *observer.ObservedLogs, s string) bool {
	needle := strings.ToLower(s)
	for _, e := range logs.All() {
		line := strings.ToLower(e.Message + " " + fmt.Sprint(e.ContextMap()))
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────
// tests
// ─────────────────────────────────────────────

// An owner creates an invite with the defaults, a new person redeems it, the
// use is counted, and a max_uses=1 invite refuses the second person.
func TestIntegrationInviteOwnerCreatesAndTeammateRedeems(t *testing.T) {
	hs := newHarness(t)
	endpoint, _ := loginServer(t, hs)
	ownerToken := mustJoinWithCode(t, endpoint, hs.code, "Owner Person")
	promoteToOwner(t, hs, ownerToken)
	owner := connectAs(t, endpoint, ownerToken)

	before := time.Now().UTC()
	inv, _ := createInviteOK(t, owner, map[string]any{"label": "for a new teammate"})
	if !inv.OK || !inv.Created || inv.State != "active" || inv.Label != "for a new teammate" {
		t.Fatalf("create_invite: ok=%v created=%v state=%q label=%q", inv.OK, inv.Created, inv.State, inv.Label)
	}
	if len(inv.Code) != joinCodeLength || strings.Trim(inv.Code, joinCodeAlphabet) != "" {
		t.Fatalf("the code is not a %d-character join code (value withheld)", joinCodeLength)
	}
	if inv.MaxUses != 1 {
		t.Errorf("default max_uses = %d, want 1", inv.MaxUses)
	}
	if d := inv.ExpiresAt.Sub(before.Add(7 * 24 * time.Hour)); d < -2*time.Second || d > time.Minute {
		t.Errorf("default expires_at = %s, want about 7 days after %s", inv.ExpiresAt, before)
	}
	for _, want := range []string{"door code", "installer", "It works 1 time until " + inv.ExpiresAt.UTC().Format(time.RFC3339)} {
		if !strings.Contains(inv.ShareNote, want) {
			t.Errorf("share_note lacks %q: %s", want, inv.ShareNote)
		}
	}

	mustJoinWithCode(t, endpoint, inv.Code, "New Teammate")
	row := readInviteRow(t, hs, inv.InviteID)
	if row.uses != 1 || row.status != enums.INVITE_STATUS_EXHAUSTED {
		t.Fatalf("after one redemption: uses=%d status=%s, want 1 exhausted", row.uses, row.status)
	}

	_, res, text := redeemCode(t, endpoint, inv.Code, "Second Person")
	if !res.IsError || !strings.Contains(text, ErrInviteNotUsable.Error()) {
		t.Fatalf("a second redemption of a max_uses=1 invite: error=%v %s", res.IsError, redactSecrets(text))
	}
	if row := readInviteRow(t, hs, inv.InviteID); row.uses != 1 {
		t.Errorf("a refused redemption spent a use: uses=%d", row.uses)
	}

	list, _ := listInvitesOK(t, owner, nil)
	got, ok := findInvite(list, inv.InviteID)
	if !ok || got.State != "exhausted" || got.Uses != 1 || got.MaxUses == nil || *got.MaxUses != 1 ||
		got.CreatedBy != "Owner Person" || !got.CreatedByYou || got.LastUsedAt == nil {
		t.Errorf("list_invites entry: found=%v %+v", ok, got)
	}

	// An owner may go to the ceiling and no further.
	big, _ := createInviteOK(t, owner, map[string]any{"max_uses": 100, "expires_in_hours": 720})
	if big.MaxUses != 100 || big.ExpiresAt.Sub(time.Now().UTC()) < 719*time.Hour {
		t.Errorf("owner ceiling invite: max_uses=%d expires_at=%s", big.MaxUses, big.ExpiresAt)
	}
	for _, args := range []map[string]any{{"max_uses": 101}, {"expires_in_hours": 721}, {"max_uses": -1}} {
		if text := refused(t, owner, "create_invite", args); !strings.HasPrefix(text, "invalid_argument:") {
			t.Errorf("owner %v: %s", args, text)
		}
	}
}

// A member may create capped invites, and one above the cap is refused
// without writing anything.
func TestIntegrationInviteMemberIsCapped(t *testing.T) {
	hs := newHarness(t)
	endpoint, _ := loginServer(t, hs)
	member := connectAs(t, endpoint, mustJoinWithCode(t, endpoint, hs.code, "Plain Member"))

	def, _ := createInviteOK(t, member, nil)
	if def.MaxUses != 1 || def.ExpiresAt.Sub(time.Now().UTC()) > 7*24*time.Hour+time.Minute {
		t.Errorf("member default: max_uses=%d expires_at=%s", def.MaxUses, def.ExpiresAt)
	}
	capped, _ := createInviteOK(t, member, map[string]any{"max_uses": 25, "expires_in_hours": 168})
	if capped.MaxUses != 25 {
		t.Errorf("member at the cap: max_uses=%d", capped.MaxUses)
	}

	before := teamInviteCount(t, hs, hs.teamID)
	for _, args := range []map[string]any{
		{"max_uses": 26},
		{"expires_in_hours": 169},
		{"max_uses": 100, "expires_in_hours": 720},
	} {
		text := refused(t, member, "create_invite", args)
		if !strings.HasPrefix(text, "not_permitted: members may create invites with at most 25 uses and 168 hours") {
			t.Errorf("member above the cap %v: %s", args, text)
		}
	}
	if after := teamInviteCount(t, hs, hs.teamID); after != before {
		t.Errorf("a refused create wrote an invite: %d -> %d", before, after)
	}

	// The member's capped invite really admits someone.
	mustJoinWithCode(t, endpoint, capped.Code, "Invited By Member")
}

// A member sees only their own invites, and cannot revoke another member's,
// the owner's, another team's or a nonexistent one — all the same not_found.
func TestIntegrationInviteMemberCannotListOrRevokeOthers(t *testing.T) {
	hs := newHarness(t)
	endpoint, _ := loginServer(t, hs)
	ownerToken := mustJoinWithCode(t, endpoint, hs.code, "Owner Person")
	promoteToOwner(t, hs, ownerToken)
	owner := connectAs(t, endpoint, ownerToken)
	ana := connectAs(t, endpoint, mustJoinWithCode(t, endpoint, hs.code, "Ana"))
	bob := connectAs(t, endpoint, mustJoinWithCode(t, endpoint, hs.code, "Bob"))

	ownerInv, _ := createInviteOK(t, owner, map[string]any{"label": "owner's"})
	anaInv, _ := createInviteOK(t, ana, map[string]any{"label": "ana's"})
	bobInv, _ := createInviteOK(t, bob, map[string]any{"label": "bob's"})

	bobList, bobText := listInvitesOK(t, bob, nil)
	if bobList.Scope != "created_by_you" || len(bobList.Invites) != 1 || bobList.Invites[0].InviteID != bobInv.InviteID ||
		!bobList.Invites[0].CreatedByYou {
		t.Fatalf("bob's list_invites: %s", bobText)
	}
	if strings.Contains(bobText, anaInv.InviteID) || strings.Contains(bobText, ownerInv.InviteID) {
		t.Fatal("bob's list_invites shows somebody else's invite")
	}
	ownerList, _ := listInvitesOK(t, owner, nil)
	if ownerList.Scope != "team" || len(ownerList.Invites) != 4 { // three here plus the harness invite
		t.Errorf("owner's list_invites: scope=%q %d invites, want team and 4", ownerList.Scope, len(ownerList.Invites))
	}

	_, _, otherInviteID := seedOtherTeam(t, hs, "FAKETEAM22")
	unknown := uuid.Must(uuid.NewV4()).String()

	want := refused(t, bob, "revoke_invite", map[string]any{"invite_id": unknown})
	if !strings.HasPrefix(want, "not_found:") {
		t.Fatalf("unknown invite: %s", want)
	}
	for name, id := range map[string]string{
		"ana's invite":         anaInv.InviteID,
		"the owner's invite":   ownerInv.InviteID,
		"another team's":       otherInviteID,
		"a malformed id":       "not-a-uuid",
		"an upper-cased known": strings.ToUpper(anaInv.InviteID),
	} {
		if got := refused(t, bob, "revoke_invite", map[string]any{"invite_id": id}); got != want {
			t.Errorf("bob revoking %s: %q, want the same answer as an unknown id: %q", name, got, want)
		}
	}
	for _, id := range []string{anaInv.InviteID, ownerInv.InviteID, otherInviteID} {
		if row := readInviteRow(t, hs, id); row.revokedAt.Valid || row.status == enums.INVITE_STATUS_REVOKED {
			t.Errorf("invite %s was revoked by a refused call", id)
		}
	}

	// Even the owner gets not_found for another team's invite.
	ownerUnknown := refused(t, owner, "revoke_invite", map[string]any{"invite_id": unknown})
	if got := refused(t, owner, "revoke_invite", map[string]any{"invite_id": otherInviteID}); got != ownerUnknown {
		t.Errorf("owner revoking another team's invite: %q, want %q", got, ownerUnknown)
	}

	// The owner may revoke a member's invite, and a member their own.
	if out := revokeInviteOK(t, owner, anaInv.InviteID); out.State != "revoked" || out.AlreadyRevoked {
		t.Errorf("owner revoking ana's invite: %+v", out)
	}
	if out := revokeInviteOK(t, bob, bobInv.InviteID); out.State != "revoked" || out.AlreadyRevoked {
		t.Errorf("bob revoking his own invite: %+v", out)
	}
}

// list_invites never carries a code: not the canary this test minted, not the
// harness's, and not a "code" key at all.
func TestIntegrationListInvitesNeverContainsTheCode(t *testing.T) {
	hs := newHarness(t)
	endpoint, _ := loginServer(t, hs)
	drawCodes(t, fakeCanaryCode, "FAKESECND2")
	ownerToken := mustJoinWithCode(t, endpoint, hs.code, "Owner Person")
	promoteToOwner(t, hs, ownerToken)
	owner := connectAs(t, endpoint, ownerToken)
	member := connectAs(t, endpoint, mustJoinWithCode(t, endpoint, hs.code, "Plain Member"))

	if inv, _ := createInviteOK(t, owner, nil); inv.Code != fakeCanaryCode {
		t.Fatal("setup: the owner's invite did not draw the canary")
	}
	if inv, _ := createInviteOK(t, member, nil); inv.Code != "FAKESECND2" {
		t.Fatal("setup: the member's invite did not draw the second fake")
	}

	for who, cs := range map[string]*mcp.ClientSession{"owner": owner, "member": member} {
		list, text := listInvitesOK(t, cs, nil)
		if len(list.Invites) == 0 {
			t.Fatalf("%s's list is empty, so the scan below proves nothing", who)
		}
		lower := strings.ToLower(text)
		for _, code := range []string{fakeCanaryCode, "FAKESECND2", hs.code} {
			if strings.Contains(lower, strings.ToLower(code)) {
				t.Errorf("%s's list_invites contains a join code", who)
			}
		}
		if strings.Contains(text, `"code"`) {
			t.Errorf("%s's list_invites has a code field", who)
		}
	}
}

// The canary code never reaches team_event — payload, response_snapshot,
// idempotency key or anything else in the row — nor any log line, including
// the error path where the driver's duplicate-key message quotes it.
func TestIntegrationInviteCodeIsNeverPersistedOrLogged(t *testing.T) {
	hs := newHarness(t)
	endpoint, logs := loginServer(t, hs)
	drawCodes(t, fakeCanaryCode)
	ownerToken := mustJoinWithCode(t, endpoint, hs.code, "Owner Person")
	promoteToOwner(t, hs, ownerToken)
	owner := connectAs(t, endpoint, ownerToken)

	inv, _ := createInviteOK(t, owner, map[string]any{"max_uses": 5, "idempotency_key": "canary-key-0001"})
	if inv.Code != fakeCanaryCode {
		t.Fatal("setup: the invite did not draw the canary")
	}
	if _, replay := createInviteOK(t, owner, map[string]any{"max_uses": 5, "idempotency_key": "canary-key-0001"}); strings.Contains(replay, fakeCanaryCode) {
		t.Error("the replay carries the code")
	}
	// Redeeming writes member_joined and agent_joined events: the rows most
	// likely to pick up anything the join saw.
	redeemer := connectAs(t, endpoint, mustJoinWithCode(t, endpoint, fakeCanaryCode, "Canary Redeemer"))
	listInvitesOK(t, owner, nil)
	listInvitesOK(t, redeemer, nil)

	// The error path. The canary is now a live code, so a generator that
	// keeps drawing it collides on uq_invite_code every time.
	errText := refused(t, owner, "create_invite", map[string]any{"label": "collides"})
	if !strings.Contains(errText, "creating the invite") || !strings.Contains(errText, "<redacted>") {
		t.Errorf("the collision did not fail the way this test means to exercise: %s", errText)
	}
	if strings.Contains(strings.ToLower(errText), strings.ToLower(fakeCanaryCode)) {
		t.Errorf("the create_invite error carries the code: %s", strings.ReplaceAll(errText, fakeCanaryCode, "<CANARY>"))
	}

	rows, err := hs.core.DB().Query(
		"SELECT CONCAT_WS('|', `id`, `kind`, IFNULL(`subject_key`, ''), IFNULL(`summary`, ''), " +
			"IFNULL(CAST(`payload` AS CHAR), ''), `idempotency_key`, IFNULL(CAST(`response_snapshot` AS CHAR), '')) FROM `team_event`")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	scanned := 0
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		scanned++
		if strings.Contains(strings.ToLower(line), strings.ToLower(fakeCanaryCode)) {
			t.Errorf("a team_event row carries the code")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("no team_event rows were written, so the scan proves nothing")
	}

	if len(logs.All()) == 0 || logs.FilterMessage("mcp tool failed").Len() == 0 {
		t.Fatal("the failed create_invite wrote no log line, so the log scan proves nothing")
	}
	if logsMention(logs, fakeCanaryCode) {
		t.Error("a log line carries the code")
	}
	t.Logf("scanned %d team_event rows and %d log lines", scanned, len(logs.All()))
}

// A revoked invite cannot be redeemed, and revoking is idempotent.
func TestIntegrationRevokedInviteCannotBeRedeemed(t *testing.T) {
	hs := newHarness(t)
	endpoint, _ := loginServer(t, hs)
	ownerToken := mustJoinWithCode(t, endpoint, hs.code, "Owner Person")
	promoteToOwner(t, hs, ownerToken)
	owner := connectAs(t, endpoint, ownerToken)

	inv, _ := createInviteOK(t, owner, map[string]any{"max_uses": 5, "label": "to revoke"})
	first := revokeInviteOK(t, owner, inv.InviteID)
	if first.State != "revoked" || first.AlreadyRevoked || first.RevokedAt.IsZero() || first.Label != "to revoke" {
		t.Fatalf("revoke_invite: %+v", first)
	}
	row := readInviteRow(t, hs, inv.InviteID)
	if !row.revokedAt.Valid || row.status != enums.INVITE_STATUS_REVOKED ||
		row.revokedBy.String != memberIDFor(t, hs, ownerToken, hs.teamID).String() {
		t.Fatalf("the row after revoking: %+v", row)
	}

	again := revokeInviteOK(t, owner, inv.InviteID)
	if !again.AlreadyRevoked || again.State != "revoked" || !again.RevokedAt.Equal(first.RevokedAt) {
		t.Errorf("revoking again: %+v", again)
	}

	_, res, text := redeemCode(t, endpoint, inv.Code, "Too Late")
	if !res.IsError || !strings.Contains(text, ErrInviteNotUsable.Error()) {
		t.Fatalf("redeeming a revoked invite: error=%v %s", res.IsError, redactSecrets(text))
	}
	if row := readInviteRow(t, hs, inv.InviteID); row.uses != 0 {
		t.Errorf("a refused redemption spent a use: %d", row.uses)
	}
	list, _ := listInvitesOK(t, owner, nil)
	if got, ok := findInvite(list, inv.InviteID); !ok || got.State != "revoked" {
		t.Errorf("list_invites after revoking: found=%v state=%q", ok, got.State)
	}
}

// An expired invite cannot be redeemed.
func TestIntegrationExpiredInviteCannotBeRedeemed(t *testing.T) {
	hs := newHarness(t)
	endpoint, _ := loginServer(t, hs)
	member := connectAs(t, endpoint, mustJoinWithCode(t, endpoint, hs.code, "Plain Member"))

	inv, _ := createInviteOK(t, member, map[string]any{"max_uses": 3, "expires_in_hours": 1})
	if _, err := hs.core.DB().Exec(
		"UPDATE `invite` SET `expires_at` = UTC_TIMESTAMP() - INTERVAL 1 HOUR WHERE `id` = ?", inv.InviteID); err != nil {
		t.Fatal(err)
	}
	_, res, text := redeemCode(t, endpoint, inv.Code, "Too Late")
	if !res.IsError || !strings.Contains(text, ErrInviteNotUsable.Error()) {
		t.Fatalf("redeeming an expired invite: error=%v %s", res.IsError, redactSecrets(text))
	}
	if row := readInviteRow(t, hs, inv.InviteID); row.uses != 0 {
		t.Errorf("a refused redemption spent a use: %d", row.uses)
	}
	list, _ := listInvitesOK(t, member, nil)
	if got, ok := findInvite(list, inv.InviteID); !ok || got.State != "expired" {
		t.Errorf("list_invites for an expired invite: found=%v state=%q", ok, got.State)
	}
}

// The same idempotency_key returns the same invite, once, and never the code
// again.
func TestIntegrationCreateInviteReplayOmitsTheCode(t *testing.T) {
	hs := newHarness(t)
	endpoint, _ := loginServer(t, hs)
	ownerToken := mustJoinWithCode(t, endpoint, hs.code, "Owner Person")
	promoteToOwner(t, hs, ownerToken)
	owner := connectAs(t, endpoint, ownerToken)
	ana := connectAs(t, endpoint, mustJoinWithCode(t, endpoint, hs.code, "Ana"))

	const key = "retry-key-00000001"
	before := teamInviteCount(t, hs, hs.teamID)
	first, _ := createInviteOK(t, owner, map[string]any{"label": "retried", "max_uses": 3, "idempotency_key": key})
	if first.Code == "" || !first.Created {
		t.Fatalf("the first call: created=%v code present=%v", first.Created, first.Code != "")
	}

	// A retry, even one whose other arguments drifted: the key names the invite.
	replay, text := createInviteOK(t, owner, map[string]any{"label": "retried again", "max_uses": 9, "idempotency_key": key})
	if replay.InviteID != first.InviteID || replay.Created || replay.Code != "" || strings.Contains(text, `"code"`) {
		t.Fatalf("the replay: %s", redactSecrets(text))
	}
	if replay.Label != "retried" || replay.MaxUses != 3 || !replay.ExpiresAt.Equal(first.ExpiresAt) || replay.State != "active" {
		t.Errorf("the replay does not describe the stored invite: %+v", replay)
	}
	if !strings.Contains(replay.ShareNote, "shown once") || !strings.Contains(replay.ShareNote, "revoke_invite") {
		t.Errorf("the replay's note: %s", replay.ShareNote)
	}
	if after := teamInviteCount(t, hs, hs.teamID); after != before+1 {
		t.Errorf("two calls with one key wrote %d invites, want 1", after-before)
	}

	// The key is per member: Ana's same key is Ana's own invite.
	anas, _ := createInviteOK(t, ana, map[string]any{"idempotency_key": key})
	if anas.InviteID == first.InviteID || anas.Code == "" || !anas.Created {
		t.Errorf("ana's call with the owner's key: same invite=%v created=%v", anas.InviteID == first.InviteID, anas.Created)
	}
	// A new key is a new invite.
	if other, _ := createInviteOK(t, owner, map[string]any{"idempotency_key": "retry-key-00000002"}); other.InviteID == first.InviteID || other.Code == "" {
		t.Error("a new idempotency_key replayed the old invite")
	}
	// A replay of a revoked invite reports it revoked, still without the code.
	revokeInviteOK(t, owner, first.InviteID)
	if gone, _ := createInviteOK(t, owner, map[string]any{"idempotency_key": key}); gone.State != "revoked" || gone.Code != "" {
		t.Errorf("replaying a revoked invite: state=%q code present=%v", gone.State, gone.Code != "")
	}
	if text := refused(t, owner, "create_invite", map[string]any{"idempotency_key": "short"}); !strings.HasPrefix(text, "invalid_argument:") {
		t.Errorf("a short key: %s", text)
	}
}

// create_invite is limited per account.
func TestIntegrationCreateInviteRateLimit(t *testing.T) {
	hs := newHarness(t)
	t.Setenv("METICHE_CREATE_INVITE_PER_HOUR", "3")
	endpoint, _ := loginServer(t, hs)
	ana := connectAs(t, endpoint, mustJoinWithCode(t, endpoint, hs.code, "Ana"))
	bob := connectAs(t, endpoint, mustJoinWithCode(t, endpoint, hs.code, "Bob"))

	for i := 0; i < 3; i++ {
		createInviteOK(t, ana, nil)
	}
	before := teamInviteCount(t, hs, hs.teamID)
	text := refused(t, ana, "create_invite", nil)
	if !strings.HasPrefix(text, "rate_limited: too many invites created by this account in the last hour") {
		t.Errorf("the fourth create: %s", text)
	}
	if after := teamInviteCount(t, hs, hs.teamID); after != before {
		t.Errorf("a rate-limited create wrote an invite")
	}
	// Keyed by account: Bob's budget is his own.
	createInviteOK(t, bob, nil)
}

// Nobody but a live member of the team reaches any of the three tools.
func TestIntegrationInviteToolsRefuseNonMembers(t *testing.T) {
	hs := newHarness(t)
	endpoint, _ := loginServer(t, hs)
	slug := hs.teamSlug(t)
	ownerToken := mustJoinWithCode(t, endpoint, hs.code, "Owner Person")
	promoteToOwner(t, hs, ownerToken)
	inv, _ := createInviteOK(t, connectAs(t, endpoint, ownerToken), nil)

	_, _, _ = seedOtherTeam(t, hs, "FAKETEAM33")
	stranger := connectAs(t, endpoint, mustJoinWithCode(t, endpoint, "FAKETEAM33", "Stranger"))
	anon := connectAs(t, endpoint, "")
	revokedToken := mustJoinWithCode(t, endpoint, hs.code, "Removed Member")
	if _, err := hs.core.DB().Exec("UPDATE `member` SET `revoked_at` = UTC_TIMESTAMP() WHERE `id` = ?",
		memberIDFor(t, hs, revokedToken, hs.teamID).String()); err != nil {
		t.Fatal(err)
	}
	removed := connectAs(t, endpoint, revokedToken)

	before := teamInviteCount(t, hs, hs.teamID)
	calls := []struct {
		tool string
		args map[string]any
	}{
		{"create_invite", map[string]any{"team_slug": slug}},
		{"list_invites", map[string]any{"team_slug": slug}},
		{"revoke_invite", map[string]any{"team_slug": slug, "invite_id": inv.InviteID}},
	}
	for _, c := range calls {
		if text := refused(t, stranger, c.tool, c.args); !strings.Contains(text, "you are not a member of that team") {
			t.Errorf("a member of another team calling %s: %s", c.tool, text)
		}
		if text := refused(t, anon, c.tool, c.args); !strings.Contains(text, "this tool needs your metiche token") {
			t.Errorf("no token calling %s: %s", c.tool, text)
		}
		if text := refused(t, removed, c.tool, c.args); !strings.Contains(text, "your membership of that team has been revoked") {
			t.Errorf("a revoked member calling %s: %s", c.tool, text)
		}
	}
	if after := teamInviteCount(t, hs, hs.teamID); after != before {
		t.Errorf("a refused call wrote an invite: %d -> %d", before, after)
	}
	if row := readInviteRow(t, hs, inv.InviteID); row.revokedAt.Valid {
		t.Error("a refused call revoked the owner's invite")
	}
}

// The share note's wording, pinned without a database.
func TestInviteShareNoteAndBounds(t *testing.T) {
	at := time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC)
	if n := inviteShareNote("hack-night", 5, at); !strings.Contains(n, "It works 5 times until 2026-09-20T18:00:00Z") {
		t.Errorf("plural note: %s", n)
	}
	cases := []struct {
		owner         bool
		uses, hours   int
		wantU, wantH  int
		wantErrPrefix string
	}{
		{true, 0, 0, 1, 168, ""},
		{false, 0, 0, 1, 168, ""},
		{true, 100, 720, 100, 720, ""},
		{false, 25, 168, 25, 168, ""},
		{false, 26, 0, 0, 0, "not_permitted:"},
		{false, 0, 169, 0, 0, "not_permitted:"},
		{true, 101, 0, 0, 0, "invalid_argument:"},
		{false, 101, 0, 0, 0, "invalid_argument:"},
		{true, 0, 721, 0, 0, "invalid_argument:"},
		{true, -1, 0, 0, 0, "invalid_argument:"},
	}
	for _, c := range cases {
		u, h, err := inviteBounds(c.owner, c.uses, c.hours)
		switch {
		case c.wantErrPrefix != "":
			if err == nil || !strings.HasPrefix(err.Error(), c.wantErrPrefix) {
				t.Errorf("inviteBounds(%v,%d,%d) err=%v, want %s", c.owner, c.uses, c.hours, err, c.wantErrPrefix)
			}
		case err != nil || u != c.wantU || h != c.wantH:
			t.Errorf("inviteBounds(%v,%d,%d) = %d,%d,%v", c.owner, c.uses, c.hours, u, h, err)
		}
	}
	if err := redactInviteCode(fmt.Errorf("Duplicate entry 'fakecanary' for key"), fakeCanaryCode); strings.Contains(strings.ToLower(err.Error()), "fakecanary") {
		t.Errorf("redaction is case-sensitive: %v", err)
	}
}
