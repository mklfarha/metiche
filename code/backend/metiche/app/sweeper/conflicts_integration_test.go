package sweeper

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/enums"
)

// seedPathConflict writes an open path_overlap conflict between two sessions'
// claims, with the participant rows detection would have attached.
func (h *harness) seedPathConflict(key, overlap string, initiatorSession, initiatorClaim, incumbentSession, incumbentClaim uuid.UUID) uuid.UUID {
	h.t.Helper()
	cid := uuid.Must(uuid.NewV4())
	h.exec("INSERT INTO `conflict` (`id`,`team_uuid`,`project_uuid`,`key`,`kind`,`dedupe_key`,`severity`,`status`,`detected_by`,"+
		"`detector_rule`,`evidence`,`suggested_action`,`occurrence_count`,`first_detected_at`,`last_detected_at`) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		cid.String(), h.teamUUID.String(), h.projectUUID.String(), key, int64(enums.CONFLICT_KIND_PATH_OVERLAP),
		"dedupe-"+cid.String()[:8], int64(enums.CONFLICT_SEVERITY_HIGH), int64(enums.CONFLICT_STATUS_OPEN),
		int64(enums.DETECTED_BY_SERVER), "path_overlap.same_path", `{"overlap_path":"`+overlap+`"}`,
		"settle it between you", 1, time.Now().UTC(), time.Now().UTC())
	for _, p := range []struct {
		session, claim uuid.UUID
		role           enums.ParticipantRole
	}{
		{initiatorSession, initiatorClaim, enums.PARTICIPANT_ROLE_INITIATOR},
		{incumbentSession, incumbentClaim, enums.PARTICIPANT_ROLE_INCUMBENT},
	} {
		h.exec("INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,"+
			"`subject_kind`,`subject_uuid`,`role`) VALUES (?,?,?,?,?,?,?,?,?)",
			id(), cid.String(), h.teamUUID.String(), p.session.String(), h.agentUUID.String(), h.memberUUID.String(),
			int64(enums.SUBJECT_KIND_CLAIM), p.claim.String(), int64(p.role))
	}
	return cid
}

// TestIntegrationClaimExpirySettlesTheConflict is test 3: a claim lapsing is a
// release too. When it was the last thing keeping two sessions overlapping,
// the sweeper closes the conflict, explains it, and tells the board.
func TestIntegrationClaimExpirySettlesTheConflict(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()

	codex := h.seedSession("S-19", enums.SESSION_STATUS_LIVE, now.Add(-10*time.Second))
	claude := h.seedSession("S-20", enums.SESSION_STATUS_LIVE, now.Add(-10*time.Second))
	lapsed := h.seedClaim(codex, "C-1", "app/rest.go", now.Add(-time.Minute))
	held := h.seedClaim(claude, "C-2", "app/rest.go", now.Add(time.Hour))
	cf := h.seedPathConflict("CF-23", "app/rest.go", codex, lapsed, claude, held)

	// A second conflict whose sides still both hold their file: the same pass
	// must leave it open.
	other := h.seedSession("S-21", enums.SESSION_STATUS_LIVE, now.Add(-10*time.Second))
	otherClaim := h.seedClaim(other, "C-3", "app/worker.go", now.Add(time.Hour))
	claudeWorker := h.seedClaim(claude, "C-4", "app/worker.go", now.Add(time.Hour))
	still := h.seedPathConflict("CF-24", "app/worker.go", other, otherClaim, claude, claudeWorker)
	// ...and it involves the lapsed session too, through a participant row, so
	// it IS re-evaluated rather than simply never looked at.
	h.exec("INSERT INTO `conflict_participant` (`id`,`conflict_uuid`,`team_uuid`,`session_uuid`,`agent_uuid`,`member_uuid`,"+
		"`subject_kind`,`subject_uuid`,`role`) VALUES (?,?,?,?,?,?,?,?,?)",
		id(), still.String(), h.teamUUID.String(), codex.String(), h.agentUUID.String(), h.memberUUID.String(),
		int64(enums.SUBJECT_KIND_CLAIM), lapsed.String(), int64(enums.PARTICIPANT_ROLE_INCUMBENT))

	rep := h.runOnce(h.sweeper(Options{}))
	if rep.ClaimsExpired != 1 {
		t.Fatalf("report says %d claims expired, want 1", rep.ClaimsExpired)
	}
	if rep.ConflictsResolved != 1 {
		t.Errorf("report says %d conflicts resolved, want 1", rep.ConflictsResolved)
	}

	var (
		status, resolution int64
		note               sql.NullString
		resolvedAt         sql.NullTime
	)
	if err := h.db.QueryRow("SELECT `status`, COALESCE(`resolution`, 0), `resolution_note`, `resolved_at` FROM `conflict` WHERE `id` = ?",
		cf.String()).Scan(&status, &resolution, &note, &resolvedAt); err != nil {
		t.Fatal(err)
	}
	if enums.ConflictStatus(status) != enums.CONFLICT_STATUS_RESOLVED {
		t.Fatalf("CF-23 is %v after its only overlap expired, want resolved", enums.ConflictStatus(status))
	}
	if enums.ConflictResolution(resolution) != enums.CONFLICT_RESOLUTION_YIELDED {
		t.Errorf("resolution = %v, want yielded", enums.ConflictResolution(resolution))
	}
	if !resolvedAt.Valid {
		t.Error("resolved_at is not set")
	}
	for _, want := range []string{"Cleared by metiche:", "S-19 (claude) let its hold on app/rest.go expire", "TTL elapsed", "S-20 (claude) still holds app/rest.go"} {
		if !strings.Contains(note.String, want) {
			t.Errorf("resolution_note is missing %q:\n%s", want, note.String)
		}
	}
	t.Logf("expiry resolution_note: %s", note.String)

	var stillStatus int64
	if err := h.db.QueryRow("SELECT `status` FROM `conflict` WHERE `id` = ?", still.String()).Scan(&stillStatus); err != nil {
		t.Fatal(err)
	}
	if enums.ConflictStatus(stillStatus) != enums.CONFLICT_STATUS_OPEN {
		t.Errorf("CF-24 is %v; S-20 and S-21 both still hold app/worker.go, so it must stay open", enums.ConflictStatus(stillStatus))
	}

	var structural bool
	if err := h.db.QueryRow("SELECT `structural` FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ? AND `subject_uuid` = ?",
		h.teamUUID.String(), int64(enums.EVENT_KIND_CONFLICT_RESOLVED), cf.String()).Scan(&structural); err != nil {
		t.Fatalf("no conflict_resolved event for CF-23: %v", err)
	}
	if !structural {
		t.Error("conflict_resolved is not structural; the board would not repaint")
	}
	if n := h.count("SELECT COUNT(*) FROM `team_event` WHERE `team_uuid` = ? AND `kind` = ?",
		h.teamUUID.String(), int64(enums.EVENT_KIND_CONFLICT_RESOLVED)); n != 1 {
		t.Errorf("%d conflict_resolved events, want 1 (CF-24 must not consume one)", n)
	}
	var events, maxSeq, teamSeq int64
	if err := h.db.QueryRow("SELECT COUNT(*), COALESCE(MAX(`sequence`),0) FROM `team_event` WHERE `team_uuid` = ?", h.teamUUID.String()).
		Scan(&events, &maxSeq); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow("SELECT `sequence` FROM `team` WHERE `id` = ?", h.teamUUID.String()).Scan(&teamSeq); err != nil {
		t.Fatal(err)
	}
	if events != maxSeq || teamSeq != maxSeq {
		t.Errorf("event log has a gap: %d events, max sequence %d, team.sequence %d", events, maxSeq, teamSeq)
	}

	// Idempotent: a second pass settles nothing again.
	rep2 := h.runOnce(h.sweeper(Options{}))
	if rep2.ConflictsResolved != 0 {
		t.Errorf("the second pass resolved %d conflicts, want 0", rep2.ConflictsResolved)
	}
}
