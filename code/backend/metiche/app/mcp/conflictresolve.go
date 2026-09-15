package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/gofrs/uuid"

	"github.com/mklfarha/metiche/backend/app/coordination"
	payload_entity "github.com/mklfarha/metiche/backend/entity/event_payload"
	"github.com/mklfarha/metiche/backend/enums"
)

// conflictresolve.go closes a path_overlap conflict once the agents have
// settled it themselves.
//
// The owner's rule: agents settle a collision between them — split the file,
// sequence the work — and ask their human only when they genuinely cannot.
// Without this file a collision the agents HAD settled still showed as open
// on the board forever, because nothing ever moved conflict.status off open.
//
// # When it runs
//
// After every path release, inside the same locked transaction as the release:
//
//   - update_intent drop_paths, and update_intent status done/abandoned/superseded
//     (intents.go, inside commit's Apply);
//   - end_session (sessions.go, inside commit's Apply);
//   - claim expiry and session abandonment (app/sweeper, inside appendEvent's
//     locked extra hook).
//
// Each open or acknowledged path_overlap conflict the releasing session takes
// part in is re-evaluated. It is closed only when its participants no longer
// hold ANY overlapping live claim paths. A conflict whose two sides still hold
// overlapping paths stays open: that is the case the agents did not settle,
// and it is where asking the human is right.
//
// # The budget
//
// Everything below runs inside the team lock, so every query is bounded: the
// releasing session's own conflicts (settleMaxConflicts), each conflict's
// participants, each participant's own live paths (settleMaxPathsPerSession)
// and the handful of report_back notes answering the conflict's notice.

const (
	// settleMaxConflicts bounds how many conflicts one release re-evaluates.
	settleMaxConflicts = 16

	// settleMaxParticipants bounds the participants read per conflict. Three
	// agents in one directory is normal; eight is a different problem.
	settleMaxParticipants = 8

	// settleMaxPathsPerSession bounds one participant's live paths. A
	// declaration is capped at MaxDeclaredPaths, so this is a few claims' worth.
	settleMaxPathsPerSession = 64

	// settleMaxNotes bounds the report_back notes quoted in one explanation.
	settleMaxNotes = 4

	// resolutionNoteChars is conflict.resolution_note, VARCHAR(400).
	resolutionNoteChars = 400

	// settleQuoteChars caps one quoted report_back note.
	settleQuoteChars = 200

	// settleOutcomeNoteChars caps a session's outcome_note inside the note.
	settleOutcomeNoteChars = 60
)

// ReleaseKind says what just let go of paths. It decides both the resolution
// (a drop can be a split, an expiry never is) and the wording of the note.
type ReleaseKind int

const (
	// ReleaseDropped is update_intent drop_paths on a still-open intent.
	ReleaseDropped ReleaseKind = iota + 1
	// ReleaseIntentEnded is update_intent status done, abandoned or superseded.
	ReleaseIntentEnded
	// ReleaseSessionEnded is end_session.
	ReleaseSessionEnded
	// ReleaseClaimExpired is the sweeper flipping a claim whose TTL lapsed.
	ReleaseClaimExpired
	// ReleaseSessionAbandoned is the sweeper writing off a silent session.
	ReleaseSessionAbandoned
)

// byAgent reports whether an agent's own call released the paths, as opposed
// to metiche noticing that a claim or a session had lapsed.
func (k ReleaseKind) byAgent() bool {
	return k == ReleaseDropped || k == ReleaseIntentEnded || k == ReleaseSessionEnded
}

// Release is one path release, as the re-evaluation needs to know it.
type Release struct {
	TeamUUID    uuid.UUID
	SessionUUID uuid.UUID
	Kind        ReleaseKind

	// Paths are the patterns dropped, for ReleaseDropped.
	Paths []string
	// IntentKey and IntentStatus name the intent that ended, for
	// ReleaseIntentEnded.
	IntentKey    string
	IntentStatus string

	// At is when it happened: the transaction's clock.
	At time.Time
}

// SettledConflict is one conflict this release closed.
type SettledConflict struct {
	ID uuid.UUID
	// Kind is the conflict's kind; the summary is worded for it.
	Kind        enums.ConflictKind
	Key         string
	ProjectUUID uuid.UUID
	Severity    enums.ConflictSeverity
	Previous    enums.ConflictStatus
	Resolution  enums.ConflictResolution
	Note        string
	OverlapPath string
}

// ─────────────────────────────────────────────
// Finding and settling
// ─────────────────────────────────────────────

// OpenConflictsOfSession lists the open or acknowledged path_overlap
// conflicts a session takes part in, oldest first. It is the candidate set for
// re-evaluation after that session released paths.
//
// Driven by idx_participant_session (session_uuid, conflict_uuid) and capped.
func OpenConflictsOfSession(ctx context.Context, q queryer, teamUUID, sessionUUID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT c.`id` FROM `conflict_participant` p JOIN `conflict` c ON c.`id` = p.`conflict_uuid` "+
			"WHERE p.`session_uuid` = ? AND c.`team_uuid` = ? AND c.`kind` = ? AND c.`status` IN (?, ?) "+
			"ORDER BY c.`created_at`, c.`key` LIMIT ?",
		sessionUUID.String(), teamUUID.String(), int64(enums.CONFLICT_KIND_PATH_OVERLAP),
		int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED), settleMaxConflicts)
	if err != nil {
		return nil, retryable(err, "finding the conflicts this release may have settled")
	}
	defer func() { _ = rows.Close() }()
	var out []uuid.UUID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, retryable(err, "reading the conflicts this release may have settled")
		}
		if id, err := uuid.FromString(raw); err == nil {
			out = append(out, id)
		}
	}
	return out, retryable(rows.Err(), "reading the conflicts this release may have settled")
}

// SettleConflict re-evaluates one conflict after rel and closes it when its
// participants no longer overlap. It reports false, and writes nothing, when
// the conflict is already closed or still a real overlap.
//
// It must run on the transaction that holds the team lock: the overlap check
// and the conditional UPDATE are one decision, and an agent re-declaring the
// file between them would otherwise be closed out of a live collision.
func SettleConflict(ctx context.Context, q queryer, conflictID uuid.UUID, rel Release) (SettledConflict, bool, error) {
	out := SettledConflict{ID: conflictID, Kind: enums.CONFLICT_KIND_PATH_OVERLAP}
	var (
		project     string
		sev, status int64
	)
	err := q.QueryRowContext(ctx,
		"SELECT `key`, `project_uuid`, `severity`, `status`, "+
			"COALESCE(JSON_UNQUOTE(JSON_EXTRACT(`evidence`, '$.overlap_path')), '') "+
			"FROM `conflict` WHERE `id` = ? AND `team_uuid` = ? AND `kind` = ? AND `status` IN (?, ?)",
		conflictID.String(), rel.TeamUUID.String(), int64(enums.CONFLICT_KIND_PATH_OVERLAP),
		int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED)).
		Scan(&out.Key, &project, &sev, &status, &out.OverlapPath)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return out, false, nil
	case err != nil:
		return out, false, retryable(err, "reading the conflict to re-evaluate")
	}
	if out.OverlapPath == "null" {
		out.OverlapPath = ""
	}
	out.ProjectUUID, _ = uuid.FromString(project)
	out.Severity = enums.ConflictSeverity(sev)
	out.Previous = enums.ConflictStatus(status)

	sides, err := loadSettleSides(ctx, q, conflictID, rel.At)
	if err != nil {
		return out, false, err
	}
	// A conflict is two sides that overlap. Fewer than two participants means
	// something was deleted under it (retention), and there is nothing
	// truthful to say about how it was settled.
	if len(sides) < 2 {
		return out, false, nil
	}

	// THE rule. Any pair of participants still holding overlapping live paths
	// keeps the conflict open, whatever anybody said in a note.
	if sidesStillOverlap(sides) {
		return out, false, nil
	}

	notes, err := loadSettleNotes(ctx, q, conflictID, sides)
	if err != nil {
		return out, false, err
	}
	out.Resolution = chooseResolution(sides, notes, rel)
	out.Note = buildResolutionNote(out, sides, notes, rel)

	res, err := q.ExecContext(ctx,
		"UPDATE `conflict` SET `status` = ?, `resolution` = ?, `resolution_note` = ?, `resolved_at` = ?, `updated_at` = ? "+
			"WHERE `id` = ? AND `status` IN (?, ?)",
		int64(enums.CONFLICT_STATUS_RESOLVED), int64(out.Resolution), out.Note, rel.At, rel.At,
		conflictID.String(), int64(enums.CONFLICT_STATUS_OPEN), int64(enums.CONFLICT_STATUS_ACKNOWLEDGED))
	if err != nil {
		return out, false, retryable(err, "closing the settled conflict")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return out, false, nil
	}
	return out, true, nil
}

// ─────────────────────────────────────────────
// The rules
// ─────────────────────────────────────────────

// chooseResolution picks the resolution from what actually happened. The first
// rule that fits wins:
//
//  1. coordinated — a participant answered this conflict's notice with
//     report_back done or acknowledged and a note: they said what they
//     agreed, and the overlap has since cleared.
//  2. superseded — every participant's session is over (ended or abandoned):
//     nobody is working on it any more.
//  3. split — the releasing side dropped paths with update_intent and is
//     still live and still holding other paths: it moved to other files and
//     kept working.
//  4. yielded — anything else: one side dropped the contested path, finished
//     its intent, ended its session or let its claim lapse while the other
//     still held it.
//
// Only reached once the overlap is gone; see sidesStillOverlap.
func chooseResolution(sides []settleSide, notes []settleNote, rel Release) enums.ConflictResolution {
	for _, n := range notes {
		if n.Agrees {
			return enums.CONFLICT_RESOLUTION_COORDINATED
		}
	}
	allOver := true
	for _, s := range sides {
		if !s.over() {
			allOver = false
			break
		}
	}
	if allOver {
		return enums.CONFLICT_RESOLUTION_SUPERSEDED
	}
	if rel.Kind == ReleaseDropped {
		if r := releaser(sides, rel); r != nil && !r.over() && len(r.Paths) > 0 {
			return enums.CONFLICT_RESOLUTION_SPLIT
		}
	}
	return enums.CONFLICT_RESOLUTION_YIELDED
}

// sidesStillOverlap reports whether any two participants still hold live paths
// that collide. read x read never collides, exactly as in detection.
func sidesStillOverlap(sides []settleSide) bool {
	for i := range sides {
		for j := i + 1; j < len(sides); j++ {
			if sides[i].SessionUUID == sides[j].SessionUUID {
				continue
			}
			for _, a := range sides[i].Paths {
				for _, b := range sides[j].Paths {
					if a.Mode == coordination.ModeRead && b.Mode == coordination.ModeRead {
						continue
					}
					if coordination.PathsOverlap(a.Path, b.Path) {
						return true
					}
				}
			}
		}
	}
	return false
}

// ─────────────────────────────────────────────
// Loading the evidence
// ─────────────────────────────────────────────

type settlePath struct {
	Mode coordination.ClaimMode
	Path coordination.NormalizedPath
}

// settleSide is one participant, as the rules and the note need it.
type settleSide struct {
	SessionUUID string
	Key         string
	Label       string
	Status      enums.SessionStatus
	Outcome     enums.SessionOutcome
	OutcomeNote string
	EndedAt     sql.NullTime
	// Paths are this session's live paths right now: held and unexpired.
	Paths []settlePath
}

// over reports a session that is no longer working. A participant whose
// session row is gone (retention) is over by definition.
func (s settleSide) over() bool {
	switch s.Status {
	case enums.SESSION_STATUS_LIVE, enums.SESSION_STATUS_STALE:
		return false
	}
	return true
}

func (s settleSide) name() string {
	key := s.Key
	if key == "" {
		key = "a session"
	}
	if s.Label != "" {
		return fmt.Sprintf("%s (%s)", key, s.Label)
	}
	return key
}

func releaser(sides []settleSide, rel Release) *settleSide {
	for i := range sides {
		if sides[i].SessionUUID == rel.SessionUUID.String() {
			return &sides[i]
		}
	}
	return nil
}

func loadSettleSides(ctx context.Context, q queryer, conflictID uuid.UUID, now time.Time) ([]settleSide, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT p.`session_uuid`, COALESCE(s.`key`, ''), COALESCE(a.`label`, ''), COALESCE(s.`status`, 0), "+
			"COALESCE(s.`outcome`, 0), COALESCE(s.`outcome_note`, ''), s.`ended_at` "+
			"FROM `conflict_participant` p "+
			"LEFT JOIN `session` s ON s.`id` = p.`session_uuid` "+
			"LEFT JOIN `agent` a ON a.`id` = p.`agent_uuid` "+
			"WHERE p.`conflict_uuid` = ? ORDER BY p.`created_at`, p.`session_uuid` LIMIT ?",
		conflictID.String(), settleMaxParticipants)
	if err != nil {
		return nil, retryable(err, "reading the conflict's participants")
	}
	var sides []settleSide
	seen := map[string]bool{}
	for rows.Next() {
		var (
			s               settleSide
			status, outcome int64
		)
		if err := rows.Scan(&s.SessionUUID, &s.Key, &s.Label, &status, &outcome, &s.OutcomeNote, &s.EndedAt); err != nil {
			_ = rows.Close()
			return nil, retryable(err, "reading the conflict's participants")
		}
		if seen[s.SessionUUID] {
			continue
		}
		seen[s.SessionUUID] = true
		s.Status = enums.SessionStatus(status)
		s.Outcome = enums.SessionOutcome(outcome)
		sides = append(sides, s)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, retryable(err, "reading the conflict's participants")
	}
	_ = rows.Close()

	// Closed before the next query: one connection, one open result set.
	for i := range sides {
		paths, err := loadLivePaths(ctx, q, sides[i].SessionUUID, now)
		if err != nil {
			return nil, err
		}
		sides[i].Paths = paths
	}
	return sides, nil
}

// loadLivePaths reads one session's held, unexpired paths. The expiry filter is
// the same lazy one detection trusts, so a lapsed claim the sweeper has not
// flipped yet already counts as released here.
func loadLivePaths(ctx context.Context, q queryer, sessionUUID string, now time.Time) ([]settlePath, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT `mode`, `pattern`, `pattern_norm`, `kind`, `prefix`, COALESCE(`suffix_pattern`, ''), `depth`, COALESCE(`ext`, '') "+
			"FROM `claim_path` WHERE `session_uuid` = ? AND `status` = ? AND `expires_at` > ? "+
			"ORDER BY `created_at`, `pattern_norm` LIMIT ?",
		sessionUUID, int64(enums.CLAIM_STATUS_HELD), now, settleMaxPathsPerSession)
	if err != nil {
		return nil, retryable(err, "reading a participant's live paths")
	}
	defer func() { _ = rows.Close() }()
	var out []settlePath
	for rows.Next() {
		var (
			mode, kind, depth                  int64
			pattern, norm, prefix, suffix, ext string
		)
		if err := rows.Scan(&mode, &pattern, &norm, &kind, &prefix, &suffix, &depth, &ext); err != nil {
			return nil, retryable(err, "reading a participant's live paths")
		}
		out = append(out, settlePath{
			Mode: claimModeToCoordination(enums.ClaimMode(mode)),
			Path: normalizedPathFromRow(pattern, norm, enums.ClaimPathKind(kind), prefix, suffix, depth, ext),
		})
	}
	return out, retryable(rows.Err(), "reading a participant's live paths")
}

// settleNote is one participant's report_back on this conflict's notice.
type settleNote struct {
	SessionKey string
	Outcome    string
	Text       string
	// Agrees is a done or acknowledged report that says something: the agent
	// told the board what it agreed. blocked, refused and not_applicable do
	// not count, and neither does a bare outcome with no note.
	Agrees bool
}

func loadSettleNotes(ctx context.Context, q queryer, conflictID uuid.UUID, sides []settleSide) ([]settleNote, error) {
	if len(sides) == 0 {
		return nil, nil
	}
	keyBySession := make(map[string]string, len(sides))
	ph := make([]string, 0, len(sides))
	args := make([]any, 0, len(sides)+4)
	for _, s := range sides {
		keyBySession[s.SessionUUID] = s.Key
		ph = append(ph, "?")
		args = append(args, s.SessionUUID)
	}
	// Driven by idx_instruction_pending (target_session_uuid, status): only the
	// participants' own acted instructions are read, then narrowed to this
	// conflict.
	args = append(args, int64(enums.INSTRUCTION_STATUS_ACTED), int64(enums.SUBJECT_KIND_CONFLICT),
		conflictID.String(), settleMaxNotes)
	rows, err := q.QueryContext(ctx,
		"SELECT `target_session_uuid`, COALESCE(`action`, 0), COALESCE(`action_note`, '') FROM `instruction` "+
			"WHERE `target_session_uuid` IN ("+strings.Join(ph, ",")+") AND `status` = ? "+
			"AND `ref_kind` = ? AND `ref_uuid` = ? ORDER BY `acted_at`, `key` LIMIT ?",
		args...)
	if err != nil {
		return nil, retryable(err, "reading the agents' answers to the conflict notice")
	}
	defer func() { _ = rows.Close() }()
	var out []settleNote
	for rows.Next() {
		var (
			session, stored string
			action          int64
		)
		if err := rows.Scan(&session, &action, &stored); err != nil {
			return nil, retryable(err, "reading the agents' answers to the conflict notice")
		}
		outcome, text := splitReportNote(stored)
		n := settleNote{SessionKey: keyBySession[session], Outcome: outcome, Text: text}
		act := enums.InstructionAction(action)
		n.Agrees = text != "" && outcome != "blocked" &&
			(act == enums.INSTRUCTION_ACTION_COMPLIED || act == enums.INSTRUCTION_ACTION_PARTIAL)
		out = append(out, n)
	}
	return out, retryable(rows.Err(), "reading the agents' answers to the conflict notice")
}

// splitReportNote undoes report_back's storage format, "<outcome>: <note>".
func splitReportNote(stored string) (outcome, text string) {
	stored = strings.TrimSpace(stored)
	head, rest, found := strings.Cut(stored, ": ")
	switch head {
	case "done", "acknowledged", "refused", "blocked", "not_applicable":
		if found {
			return head, strings.TrimSpace(rest)
		}
		return head, ""
	}
	if stored == "done" || stored == "acknowledged" || stored == "refused" || stored == "blocked" || stored == "not_applicable" {
		return stored, ""
	}
	return "", stored
}

// ─────────────────────────────────────────────
// The explanation
// ─────────────────────────────────────────────

// buildResolutionNote writes the one paragraph the board shows under a settled
// conflict, from real data only: who released what and when, how the other
// sides stand or ended, and what the agents said in report_back.
//
// It never carries a token: every piece of free text goes through
// sanitizeNoteText, and nothing here reads a credential column. It always fits
// conflict.resolution_note.
func buildResolutionNote(c SettledConflict, sides []settleSide, notes []settleNote, rel Release) string {
	path := c.OverlapPath
	if path == "" {
		path = "the shared path"
	}
	at := clock(rel.At)

	var b strings.Builder
	if rel.Kind.byAgent() {
		b.WriteString("Settled by the agents: ")
	} else {
		b.WriteString("Cleared by metiche: ")
	}

	var clauses []string
	r := releaser(sides, rel)
	if r != nil {
		clauses = append(clauses, releaseClause(*r, rel, path, at))
	}
	overlap, overlapOK := contestedPath(c.OverlapPath)
	for _, s := range sides {
		if r != nil && s.SessionUUID == r.SessionUUID {
			continue
		}
		clauses = append(clauses, standingClause(s, overlap, overlapOK, path))
	}
	b.WriteString(strings.Join(clauses, "; "))
	b.WriteString(".")

	base := b.String()
	var quoted []settleNote
	for _, n := range notes {
		if n.Text != "" {
			quoted = append(quoted, n)
		}
	}
	if len(quoted) > 0 {
		room := resolutionNoteChars - len([]rune(base))
		per := room/len(quoted) - 18 // ` S-20 said: "…"` costs this much around the text
		if per > settleQuoteChars {
			per = settleQuoteChars
		}
		if per >= 24 {
			for _, n := range quoted {
				who := n.SessionKey
				if who == "" {
					who = "an agent"
				}
				fmt.Fprintf(&b, " %s said: \"%s\"", who, clip(sanitizeNoteText(n.Text), per))
			}
		}
	}
	return clip(b.String(), resolutionNoteChars)
}

func releaseClause(s settleSide, rel Release, path, at string) string {
	switch rel.Kind {
	case ReleaseDropped:
		released := path
		if len(rel.Paths) > 0 {
			released = listPatterns(rel.Paths, 2)
		}
		out := fmt.Sprintf("%s released %s at %s", s.name(), released, at)
		if len(s.Paths) > 0 {
			out += " and kept working in " + describeLivePaths(s.Paths, 2)
		}
		return out
	case ReleaseIntentEnded:
		intent := firstNonEmpty(rel.IntentKey, "its intent")
		status := firstNonEmpty(rel.IntentStatus, "done")
		return fmt.Sprintf("%s marked %s %s at %s, releasing %s", s.name(), intent, status, at, path)
	case ReleaseSessionEnded:
		return fmt.Sprintf("%s ended its session at %s%s, releasing %s", s.name(), at, outcomeSuffix(s), path)
	case ReleaseClaimExpired:
		return fmt.Sprintf("%s let its hold on %s expire at %s (TTL elapsed, no heartbeat)", s.name(), path, at)
	case ReleaseSessionAbandoned:
		return fmt.Sprintf("%s was abandoned at %s after no heartbeat, releasing %s", s.name(), at, path)
	}
	return fmt.Sprintf("%s released %s at %s", s.name(), path, at)
}

func standingClause(s settleSide, overlap coordination.NormalizedPath, overlapOK bool, path string) string {
	switch {
	case s.Status == enums.SESSION_STATUS_ABANDONED:
		return fmt.Sprintf("%s was abandoned%s", s.name(), endedAt(s))
	case s.over():
		return fmt.Sprintf("%s ended%s%s", s.name(), endedAt(s), outcomeSuffix(s))
	case overlapOK && holdsPath(s, overlap):
		return fmt.Sprintf("%s still holds %s", s.name(), path)
	case len(s.Paths) > 0:
		return fmt.Sprintf("%s is still working in %s", s.name(), describeLivePaths(s.Paths, 2))
	}
	return fmt.Sprintf("%s is still live, holding nothing there", s.name())
}

func endedAt(s settleSide) string {
	if !s.EndedAt.Valid {
		return ""
	}
	return " at " + clock(s.EndedAt.Time)
}

// outcomeSuffix renders " (succeeded: shipped the handler)".
func outcomeSuffix(s settleSide) string {
	if s.Outcome == enums.SESSION_OUTCOME_INVALID {
		return ""
	}
	note := clip(sanitizeNoteText(s.OutcomeNote), settleOutcomeNoteChars)
	if note == "" {
		return fmt.Sprintf(" (%s)", s.Outcome.String())
	}
	return fmt.Sprintf(" (%s: %s)", s.Outcome.String(), note)
}

func contestedPath(overlap string) (coordination.NormalizedPath, bool) {
	if overlap == "" {
		return coordination.NormalizedPath{}, false
	}
	np, err := coordination.NormalizePath(overlap, nil, false)
	if err != nil {
		return coordination.NormalizedPath{}, false
	}
	return np, true
}

func holdsPath(s settleSide, target coordination.NormalizedPath) bool {
	for _, p := range s.Paths {
		if coordination.PathsOverlap(p.Path, target) {
			return true
		}
	}
	return false
}

func describeLivePaths(paths []settlePath, n int) string {
	patterns := make([]string, 0, len(paths))
	for _, p := range paths {
		patterns = append(patterns, p.Path.PatternNorm)
	}
	return listPatterns(patterns, n)
}

// listPatterns renders "a, b +3 more", de-duplicated, in the order given.
func listPatterns(patterns []string, n int) string {
	seen := map[string]bool{}
	var uniq []string
	for _, p := range patterns {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		uniq = append(uniq, p)
	}
	if len(uniq) <= n {
		return strings.Join(uniq, ", ")
	}
	return fmt.Sprintf("%s +%d more", strings.Join(uniq[:n], ", "), len(uniq)-n)
}

// clock renders the time the board reads next to it: "16:47 UTC".
func clock(t time.Time) string {
	return t.UTC().Format("15:04") + " UTC"
}

// clip shortens to max runes, marking the cut with an ellipsis.
func clip(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	if max <= 1 {
		return string(r[:max])
	}
	return strings.TrimSpace(string(r[:max-1])) + "…"
}

// noteSecretPatterns are the shapes of credential an agent could paste into a
// report_back or end_session note. The explanation is public on the board, so
// anything that looks like one is masked before it is quoted.
var noteSecretPatterns = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`mtk_[A-Za-z0-9_\-]+`), "[redacted]"},
	{regexp.MustCompile(`mbs_[A-Za-z0-9_\-]+`), "[redacted]"},
	{regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=\-]+`), "[redacted]"},
	{regexp.MustCompile(`://[^\s/@:]+:[^\s/@]+@`), "://[redacted]@"},
	{regexp.MustCompile(`(?i)\b(password|passwd|pswd|secret|token|api[_-]?key)\b\s*[:=]\s*\S+`), "$1=[redacted]"},
}

// sanitizeNoteText collapses whitespace, masks credential-shaped text, and
// swaps double quotes so a quoted note cannot close its own quotation.
func sanitizeNoteText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	for _, p := range noteSecretPatterns {
		s = p.re.ReplaceAllString(s, p.with)
	}
	return strings.ReplaceAll(s, `"`, "'")
}

// ─────────────────────────────────────────────
// Telling the board
// ─────────────────────────────────────────────

// ConflictResolvedSummary is the timeline line for a settled conflict.
func ConflictResolvedSummary(s SettledConflict) string {
	switch s.Kind {
	case enums.CONFLICT_KIND_CONTRACT_MISMATCH, enums.CONFLICT_KIND_CONTRACT_UNCLAIMED, enums.CONFLICT_KIND_CONTRACT_NAMING_VARIANT:
		what := strings.ReplaceAll(s.Kind.String(), "_", " ")
		return truncate(fmt.Sprintf("%s settled (%s): the %s on %s", s.Key, s.Resolution.String(), what, firstNonEmpty(s.OverlapPath, "the contract")), 240)
	case enums.CONFLICT_KIND_DECISION_CONTRADICTION:
		return truncate(fmt.Sprintf("%s settled (%s): the plan no longer contradicts %s", s.Key, s.Resolution.String(), firstNonEmpty(s.OverlapPath, "the decision")), 240)
	}
	where := s.OverlapPath
	if where == "" {
		where = "the shared path"
	}
	return truncate(fmt.Sprintf("%s settled (%s): the overlap on %s cleared", s.Key, s.Resolution.String(), where), 240)
}

// ConflictResolvedPayload is the conflict_resolved event's payload: the
// resolution in detail, the explanation in message, and the status change the
// board already understands.
func ConflictResolvedPayload(s SettledConflict) payload_entity.EventPayload {
	id := s.ID
	p := payload_entity.EventPayload{
		Message:        nullString(s.Note),
		PreviousStatus: nullString(s.Previous.String()),
		NewStatus:      nullString(enums.ConflictStatus(enums.CONFLICT_STATUS_RESOLVED).String()),
		Severity:       s.Severity,
		ConflictUUID:   &id,
		Detail:         nullString(s.Resolution.String()),
	}
	if s.OverlapPath != "" {
		p.Paths = []string{s.OverlapPath}
	}
	return p
}

// eventActor is who a derived event is attributed to: the session whose
// release settled the conflict, so the run history shows it.
type eventActor struct {
	ProjectUUID uuid.UUID
	SessionUUID uuid.UUID
	AgentUUID   uuid.UUID
	MemberUUID  uuid.UUID
}

// settleAfterRelease re-evaluates the releasing session's conflicts from
// inside a tool's Apply, closes the settled ones, and appends one
// conflict_resolved event for each.
//
// THE SEQUENCE. commit writes exactly one event for the tool call, at
// tc.Sequence, after Apply returns. A conflict_resolved event is a second,
// derived event in the same transaction, so it is written here at tc.Sequence
// and tc.Sequence then moves on by one: the tool's own event lands on the next
// number, and commit advances team.sequence to it. The log stays gapless and
// ordered — the settlement precedes the call that caused it by one — and both
// commit or neither does.
//
// It is STRUCTURAL: a settled conflict leaves the lane badges and the open
// list, which is a change to the board's shape. So tc.Revision moves too, and
// that is what makes the board refresh its state and repaint without a reload.
//
// A retried call replays its stored response before Apply ever runs, so this
// never runs twice for one call, and the conditional UPDATE in SettleConflict
// means a conflict is closed once whoever gets there first.
func (h *Handler) settleAfterRelease(ctx context.Context, tc *TxContext, rel Release, actor eventActor) error {
	ids, err := OpenConflictsOfSession(ctx, tc.Tx, rel.TeamUUID, rel.SessionUUID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		settled, ok, err := SettleConflict(ctx, tc.Tx, id, rel)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := appendConflictResolvedEvent(ctx, tc, rel.TeamUUID, settled, actor); err != nil {
			return err
		}
	}
	return nil
}

func appendConflictResolvedEvent(ctx context.Context, tc *TxContext, teamUUID uuid.UUID, s SettledConflict, actor eventActor) error {
	evID, err := uuid.NewV4()
	if err != nil {
		return err
	}
	project := actor.ProjectUUID
	if project.IsNil() {
		project = s.ProjectUUID
	}
	// string(), not []byte: see the note on JSON columns in detector.go.
	payload := string(ConflictResolvedPayload(s).ToJSON())
	subject := s.ID
	if _, err := tc.Tx.ExecContext(ctx,
		"INSERT INTO `team_event` (`id`, `team_uuid`, `sequence`, `project_uuid`, `session_uuid`, `agent_uuid`, `member_uuid`, "+
			"`kind`, `subject_kind`, `subject_uuid`, `subject_key`, `structural`, `summary`, `payload`, "+
			"`idempotency_key`, `occurred_at`, `created_at`, `updated_at`) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		evID.String(), teamUUID.String(), tc.Sequence,
		nilIfNilUUID(project), nilIfNilUUID(actor.SessionUUID), nilIfNilUUID(actor.AgentUUID), nilIfNilUUID(actor.MemberUUID),
		int64(enums.EVENT_KIND_CONFLICT_RESOLVED), int64(enums.SUBJECT_KIND_CONFLICT), subject.String(),
		truncate(s.Key, 64), true, ConflictResolvedSummary(s), payload,
		"conflict_resolved:"+s.ID.String(), tc.Now, tc.Now, tc.Now); err != nil {
		return retryable(err, "recording that the conflict was settled")
	}
	tc.Sequence++
	tc.Revision++
	return nil
}

func nilIfNilUUID(id uuid.UUID) any {
	if id.IsNil() {
		return nil
	}
	return id.String()
}

// bothWriting is the two-agents-in-one-file case the wording speaks to.
func bothWriting(a, b claimSide) bool {
	return a.Mode == coordination.ModeWrite && b.Mode == coordination.ModeWrite
}

// ─────────────────────────────────────────────
// Participant bookkeeping
// ─────────────────────────────────────────────

// markConflictNoticesDelivered stamps conflict_participant.notified_at for the
// session that just collected conflict notices. One single-table UPDATE on
// idx_participant_session, run outside any transaction.
func markConflictNoticesDelivered(ctx context.Context, q queryer, sessionUUID uuid.UUID,
	waiting []waitingInstruction, delivered []string, now time.Time) error {
	got := make(map[string]bool, len(delivered))
	for _, id := range delivered {
		got[id] = true
	}
	seen := map[string]bool{}
	args := []any{now, now, sessionUUID.String()}
	var ph []string
	for _, w := range waiting {
		if !got[w.id] || w.kind != enums.INSTRUCTION_KIND_CONFLICT_NOTICE ||
			w.refKind != int64(enums.SUBJECT_KIND_CONFLICT) || w.refUUID == "" || seen[w.refUUID] {
			continue
		}
		seen[w.refUUID] = true
		ph = append(ph, "?")
		args = append(args, w.refUUID)
	}
	if len(ph) == 0 {
		return nil
	}
	_, err := q.ExecContext(ctx,
		"UPDATE `conflict_participant` SET `notified_at` = COALESCE(`notified_at`, ?), `updated_at` = ? "+
			"WHERE `session_uuid` = ? AND `conflict_uuid` IN ("+strings.Join(ph, ",")+")",
		args...)
	return err
}
