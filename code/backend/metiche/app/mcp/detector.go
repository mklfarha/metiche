package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/app/coordination"
	"github.com/mklfarha/metiche/backend/core"
	projectmod "github.com/mklfarha/metiche/backend/core/module/project"
	project_types "github.com/mklfarha/metiche/backend/core/module/project/types"
	conflict_evidence_entity "github.com/mklfarha/metiche/backend/entity/conflict_evidence"
	project_entity "github.com/mklfarha/metiche/backend/entity/project"
	"github.com/mklfarha/metiche/backend/enums"
)

// detector.go is the bridge between the pure path rules in app/coordination
// and the write path in sequence.go.
//
// The whole point of it is WHERE it runs. PLAN.md: "detection and insertion
// happen inside the same transaction as the sequence lock. Check overlap
// outside the lock and you get exactly the TOCTOU race where both agents see
// a clean world and both insert." So everything here is handed the open
// transaction by commit(), under the team row lock, after the caller's claim
// rows exist and before the event is appended.
//
// The consequence is the budget. Inside the lock nothing may do network I/O
// and nothing may walk an unbounded row set, so every query below is either a
// point lookup or one index range scan with an explicit LIMIT.

const (
	// detectCandidateLimit bounds the candidate scan per claimed path. A team
	// is ten people times five agents, so a few hundred live claim_path rows
	// is the whole universe; this exists to keep a pathological project from
	// turning one declaration into a table scan while holding the lock.
	detectCandidateLimit = 200

	// detectMaxPathsScanned bounds how many of the caller's own paths get a
	// scan, for the same reason. The tools cap the accepted path count lower
	// than this; this is the backstop.
	detectMaxPathsScanned = 64

	// detectMaxSessionsHydrated bounds the one lookup that is not part of the
	// scan: turning candidate session uuids into names and branches.
	detectMaxSessionsHydrated = 32

	// detectMaxNotices is how many collisions ride back in one response. A
	// model that gets twenty of them reads none of them.
	detectMaxNotices = 5

	// detectInstructionTTL is how long a conflict notice waits for the
	// incumbent before it expires unread. Matched to the claim hard cap: a
	// notice about a claim that can no longer exist is noise.
	detectInstructionTTL = 4 * time.Hour
)

// notifySeverityFloor is the noise rule that matters most, and it is
// deliberately NOT the recording floor.
//
// PLAN.md: "record floor low but notify floor medium". Everything confirmed
// gets a conflict row, so the board and the audit trail are complete; only
// medium and up interrupts anybody. A conflict system that cries wolf gets
// ignored, and an LLM that learns to skim your tool output is unrecoverable.
const notifySeverityFloor = coordination.SeverityMedium

// ─────────────────────────────────────────────
// The request a tool hands the detector
// ─────────────────────────────────────────────

// pathDeclaration is one path this call just claimed, with the claim it
// belongs to. Filled in by the tool's Apply hook, because the claim uuid does
// not exist until then.
type pathDeclaration struct {
	ClaimUUID uuid.UUID
	Mode      coordination.ClaimMode
	Path      coordination.NormalizedPath
}

// pathDetectionRequest is what travels from a tool into the detection hook.
//
// It rides on the context rather than on the Mutation, for one reason: the
// Mutation type is shared by every tool in the package and owned by
// sequence.go, and the detection layer must be installable without editing
// it. The tool creates the request, puts it on the context it passes to
// commit, and fills in Declared from inside Apply — the same transaction the
// hook then reads it in.
//
// A call that declares no paths leaves this off the context entirely, and the
// hook returns immediately. That is the common case: heartbeats, joins and
// session lifecycle calls must not pay for a detector they do not use.
type pathDetectionRequest struct {
	TeamUUID    uuid.UUID
	ProjectUUID uuid.UUID
	SessionUUID uuid.UUID
	AgentUUID   uuid.UUID
	MemberUUID  uuid.UUID

	SessionKey string
	Branch     string
	MemberName string
	AgentLabel string

	// HotspotPatterns comes from the project row. Hotspots escalate: two
	// agents in go.mod merge cleanly and are still wrong.
	HotspotPatterns []string

	// IntentUUID and IntentKey name what the claim was declared for, so the
	// conflict evidence says what the other agent was trying to do rather
	// than just which file they touched.
	IntentUUID    *uuid.UUID
	IntentKey     string
	IntentSummary string

	// Declared is filled inside the transaction by the tool's Apply hook.
	Declared []pathDeclaration
}

type pathDetectionCtxKey struct{}

// withPathDetection attaches a detection request to the context a tool passes
// to commit. The hook reads the same pointer, so Apply can fill in the claim
// uuids it mints inside the transaction.
func withPathDetection(ctx context.Context, req *pathDetectionRequest) context.Context {
	return context.WithValue(ctx, pathDetectionCtxKey{}, req)
}

func pathDetectionFrom(ctx context.Context) *pathDetectionRequest {
	req, _ := ctx.Value(pathDetectionCtxKey{}).(*pathDetectionRequest)
	return req
}

// ─────────────────────────────────────────────
// The hook
// ─────────────────────────────────────────────

type pathDetector struct {
	core   *core.Implementation
	logger *zap.Logger
}

// NewPathDetector returns the DetectHook to install with h.SetDetector.
//
// One hook serves every mutating tool. It looks for a detection request on
// the context and does nothing at all when there is none, which is what keeps
// heartbeat and the session lifecycle calls free of it.
func NewPathDetector(coreImpl *core.Implementation, logger *zap.Logger) DetectHook {
	if logger == nil {
		logger = zap.NewNop()
	}
	d := &pathDetector{core: coreImpl, logger: logger}
	return d.detect
}

func (d *pathDetector) detect(ctx context.Context, tc *TxContext, m *Mutation) ([]ConflictNotice, error) {
	req := pathDetectionFrom(ctx)
	if req == nil || len(req.Declared) == 0 {
		return nil, nil
	}
	if req.ProjectUUID.IsNil() || req.SessionUUID.IsNil() {
		return nil, errors.New("detection was asked to run without a project and a session")
	}

	found, err := findPathOverlaps(ctx, tc.Tx, req, tc.Now)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, nil
	}

	notices := make([]ConflictNotice, 0, len(found))
	var top *conflictRow
	for i := range found {
		row, err := d.record(ctx, tc, req, found[i], i)
		if err != nil {
			return nil, err
		}
		if top == nil || row.Severity > top.Severity {
			r := row
			top = &r
		}
		if !found[i].Verdict.NotifyInitiator || len(notices) >= detectMaxNotices {
			continue
		}
		notices = append(notices, ConflictNotice{
			Key:             row.Key,
			Kind:            "path_overlap",
			Severity:        found[i].Verdict.Severity.String(),
			With:            describeHolder(found[i].Theirs),
			Paths:           []string{found[i].Verdict.OverlapPath},
			SuggestedAction: found[i].Verdict.ActionForInitiator,
		})
	}

	// The event this call is about to write carries the worst collision it
	// caused. Safe to set here: the hook is handed the Mutation by pointer
	// precisely because it runs before the event row is built.
	if top != nil {
		if m.Payload.ConflictUUID == nil {
			id := top.ID
			m.Payload.ConflictUUID = &id
		}
		if m.Payload.Severity == enums.CONFLICT_SEVERITY_INVALID {
			m.Payload.Severity = severityEnum(top.Severity)
		}
	}
	if len(notices) > 0 {
		// The collision is the most important line in the response, so it
		// wins the note. Appended rather than replacing, because what the
		// tool already wrote there is how the agent knows its own call
		// applied.
		lead := fmt.Sprintf("%d collision(s) on paths you just claimed — read conflicts[] before you edit", len(notices))
		if m.Envelope.Note == "" {
			m.Envelope.Note = lead
		} else {
			m.Envelope.Note = m.Envelope.Note + "; " + lead
		}
	}
	return notices, nil
}

// ─────────────────────────────────────────────
// Candidate selection, confirmation and scoring
//
// Split out from the writing half so it can run read-only for check_paths,
// which must take no lock, write no row and commit the caller to nothing.
// ─────────────────────────────────────────────

// pathOverlap is one confirmed collision between a path this caller declared
// and a path somebody else is holding.
type pathOverlap struct {
	Mine    claimSide
	Theirs  claimSide
	Verdict overlapVerdict
}

// findPathOverlaps runs the candidate scan for every declared path, confirms
// the candidates in Go, scores them, and returns them worst-first.
//
// q is the open transaction when this runs inside commit, and the pool when
// check_paths runs it. The queries are identical on purpose: a pre-flight that
// answers differently from the real thing is worse than no pre-flight.
func findPathOverlaps(ctx context.Context, q queryer, req *pathDetectionRequest, now time.Time) ([]pathOverlap, error) {
	declared := req.Declared
	if len(declared) > detectMaxPathsScanned {
		declared = declared[:detectMaxPathsScanned]
	}

	// Scan first, hydrate second. The scan is the hot query and touches one
	// index; turning the handful of sessions it finds into names is a
	// separate bounded lookup, and doing it per candidate would be the join
	// the denormalization exists to avoid.
	type scanned struct {
		mine pathDeclaration
		cand candidateRow
	}
	var pairs []scanned
	seenCandidate := map[string]bool{}
	for _, mine := range declared {
		rows, err := scanClaimPathCandidates(ctx, q, req.ProjectUUID, req.SessionUUID, mine.Path, now)
		if err != nil {
			return nil, err
		}
		for _, c := range rows {
			// The same claim_path can be a candidate for two of my paths.
			// Keep both pairings — they may overlap on different files — but
			// only hydrate the session once.
			pairs = append(pairs, scanned{mine: mine, cand: c})
			seenCandidate[c.SessionUUID] = true
		}
	}
	if len(pairs) == 0 {
		return nil, nil
	}

	ids := make([]string, 0, len(seenCandidate))
	for id := range seenCandidate {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > detectMaxSessionsHydrated {
		ids = ids[:detectMaxSessionsHydrated]
	}
	sessions, err := hydrateSessions(ctx, q, ids)
	if err != nil {
		return nil, err
	}

	mineSide := func(d pathDeclaration) claimSide {
		return claimSide{
			ClaimUUID:   d.ClaimUUID.String(),
			SessionUUID: req.SessionUUID.String(),
			AgentUUID:   req.AgentUUID.String(),
			MemberUUID:  req.MemberUUID.String(),
			SessionKey:  req.SessionKey,
			MemberName:  req.MemberName,
			AgentLabel:  req.AgentLabel,
			Branch:      req.Branch,
			Mode:        d.Mode,
			Path:        d.Path,
		}
	}

	out := make([]pathOverlap, 0, len(pairs))
	seenDedupe := map[string]bool{}
	for _, p := range pairs {
		info, ok := sessions[p.cand.SessionUUID]
		if !ok {
			// Hydration was capped, or the session vanished under us. Skip
			// rather than guess: a conflict participant needs a real agent
			// uuid, and an invented one would fail the insert anyway.
			continue
		}
		theirs := claimSide{
			ClaimUUID:   p.cand.ClaimUUID,
			SessionUUID: p.cand.SessionUUID,
			MemberUUID:  p.cand.MemberUUID,
			AgentUUID:   info.AgentUUID,
			SessionKey:  info.Key,
			MemberName:  info.MemberName,
			AgentLabel:  info.AgentLabel,
			Branch:      info.Branch,
			Mode:        p.cand.Mode,
			Path:        p.cand.Path,
			HeldFor:     now.Sub(p.cand.Since),
		}
		v := assessOverlap(mineSide(p.mine), theirs, req.HotspotPatterns)
		if !v.Conflict {
			continue
		}
		// Two of my paths can confirm against the same claim on the same
		// file. That is one collision, not two, and bumping its counter twice
		// in one call would make occurrence_count a lie.
		if seenDedupe[v.DedupeKey] {
			continue
		}
		seenDedupe[v.DedupeKey] = true
		out = append(out, pathOverlap{Mine: mineSide(p.mine), Theirs: theirs, Verdict: v})
	}

	// Worst first, then by a stable tiebreak so the same world always
	// produces the same response — which is what makes the stored snapshot
	// worth replaying.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Verdict.Severity != out[j].Verdict.Severity {
			return out[i].Verdict.Severity > out[j].Verdict.Severity
		}
		if out[i].Verdict.OverlapPath != out[j].Verdict.OverlapPath {
			return out[i].Verdict.OverlapPath < out[j].Verdict.OverlapPath
		}
		return out[i].Verdict.DedupeKey < out[j].Verdict.DedupeKey
	})
	return out, nil
}

// candidateRow is one live claim_path row that COULD overlap, straight off the
// index. Nothing is confirmed yet.
type candidateRow struct {
	ID          string
	ClaimUUID   string
	SessionUUID string
	MemberUUID  string
	Mode        coordination.ClaimMode
	Path        coordination.NormalizedPath
	Since       time.Time
}

// scanClaimPathCandidates is THE hot query, and the reason claim_path repeats
// six columns from claim.
//
//	WHERE project_uuid = ? AND status = held AND expires_at > now
//	  AND session_uuid <> me
//	  AND (prefix IN (:ancestors) OR prefix LIKE CONCAT(:my_prefix,'%'))
//
// Two patterns can only touch the same file if one prefix is an ancestor of
// the other, so those two branches are the complete candidate set: the IN list
// is at most a dozen point lookups up my own path, and the LIKE is a
// left-anchored range scan over everything at or below me. One index
// (project_uuid, status, prefix, expires_at), no joins, and a hard LIMIT.
//
// Expiry is filtered here rather than trusted to a sweeper. The lazy filter is
// authoritative: a claim whose TTL lapsed is invisible to detection the
// instant it lapses, whether or not anything has got around to marking it.
func scanClaimPathCandidates(ctx context.Context, q queryer, projectUUID, excludeSession uuid.UUID, mine coordination.NormalizedPath, now time.Time) ([]candidateRow, error) {
	ancestors := coordination.PathAncestors(mine.Prefix)
	placeholders := make([]string, len(ancestors))
	args := make([]any, 0, len(ancestors)+5)
	args = append(args, projectUUID.String(), int64(enums.CLAIM_STATUS_HELD), now, excludeSession.String())
	for i, a := range ancestors {
		placeholders[i] = "?"
		args = append(args, a)
	}
	args = append(args, likePrefixPattern(mine.Prefix))

	query := "SELECT `id`, `claim_uuid`, `session_uuid`, `member_uuid`, `mode`, `pattern`, `pattern_norm`, " +
		"`kind`, `prefix`, COALESCE(`suffix_pattern`, ''), `depth`, COALESCE(`ext`, ''), `created_at` " +
		"FROM `claim_path` " +
		"WHERE `project_uuid` = ? AND `status` = ? AND `expires_at` > ? AND `session_uuid` <> ? " +
		"AND (`prefix` IN (" + strings.Join(placeholders, ",") + ") OR `prefix` LIKE ? ESCAPE '!') " +
		"LIMIT " + strconv.Itoa(detectCandidateLimit)

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, retryable(err, "scanning for overlapping claims")
	}
	defer func() { _ = rows.Close() }()

	var out []candidateRow
	for rows.Next() {
		var (
			c       candidateRow
			mode    int64
			kind    int64
			pattern string
			norm    string
			prefix  string
			suffix  string
			depth   int64
			ext     string
		)
		if err := rows.Scan(&c.ID, &c.ClaimUUID, &c.SessionUUID, &c.MemberUUID, &mode,
			&pattern, &norm, &kind, &prefix, &suffix, &depth, &ext, &c.Since); err != nil {
			return nil, retryable(err, "reading an overlapping claim")
		}
		c.Mode = claimModeToCoordination(enums.ClaimMode(mode))
		c.Path = normalizedPathFromRow(pattern, norm, enums.ClaimPathKind(kind), prefix, suffix, depth, ext)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, retryable(err, "reading the overlapping claims")
	}
	return out, nil
}

// likePrefixPattern builds the left-anchored LIKE argument, escaping the two
// characters LIKE treats as wildcards.
//
// "_" is the one that actually bites: it is legal and common in file names, so
// an unescaped "src/my_pkg/" would match "src/myXpkg/" as well. "!" is the
// escape character rather than backslash so the pattern means the same thing
// under NO_BACKSLASH_ESCAPES.
func likePrefixPattern(prefix string) string {
	r := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")
	return r.Replace(prefix) + "%"
}

// sessionInfo is who is on the other side of a candidate overlap. It exists
// because a conflict without a name and a branch is not actionable, and
// because conflict_participant needs the agent uuid.
type sessionInfo struct {
	Key        string
	Branch     string
	StatusLine string
	AgentUUID  string
	MemberUUID string
	AgentLabel string
	MemberName string
}

// hydrateSessions turns the candidate session uuids into names in ONE query.
// Bounded by the caller (detectMaxSessionsHydrated) and driven by the primary
// key, so it is a handful of point lookups.
func hydrateSessions(ctx context.Context, q queryer, ids []string) (map[string]sessionInfo, error) {
	out := make(map[string]sessionInfo, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := q.QueryContext(ctx,
		"SELECT s.`id`, s.`key`, COALESCE(s.`branch`, ''), COALESCE(s.`status_line`, ''), "+
			"s.`agent_uuid`, s.`member_uuid`, COALESCE(a.`label`, ''), COALESCE(m.`display_name`, '') "+
			"FROM `session` s "+
			"JOIN `agent` a ON a.`id` = s.`agent_uuid` "+
			"JOIN `member` m ON m.`id` = s.`member_uuid` "+
			"WHERE s.`id` IN ("+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return nil, retryable(err, "looking up who holds the overlapping claims")
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var info sessionInfo
		if err := rows.Scan(&id, &info.Key, &info.Branch, &info.StatusLine,
			&info.AgentUUID, &info.MemberUUID, &info.AgentLabel, &info.MemberName); err != nil {
			return nil, retryable(err, "reading who holds the overlapping claims")
		}
		out[id] = info
	}
	if err := rows.Err(); err != nil {
		return nil, retryable(err, "reading who holds the overlapping claims")
	}
	return out, nil
}

// ─────────────────────────────────────────────
// The decision layer — pure, and tested without a database
// ─────────────────────────────────────────────

// claimSide is one participant in a candidate overlap: everything the rules
// may look at, and nothing else. No handles, no context, no queries — the
// verdict has to be reproducible from the conflict row alone, which it would
// not be if the rules could go and look something up.
type claimSide struct {
	ClaimUUID   string
	SessionUUID string
	AgentUUID   string
	MemberUUID  string
	SessionKey  string
	MemberName  string
	AgentLabel  string
	Branch      string
	Mode        coordination.ClaimMode
	Path        coordination.NormalizedPath
	// HeldFor is how long the other side has been holding this path. Only
	// ever set for the incumbent; the caller's own claim is seconds old.
	HeldFor time.Duration
}

// overlapVerdict is the complete decision about one confirmed overlap.
type overlapVerdict struct {
	Conflict  bool
	Severity  coordination.Severity
	Adjusters []string
	// Rule names the specific rule that fired, and is stored on the conflict.
	// It is what the self-tuning loop counts dismissals against, so it has to
	// be finer-grained than "path_overlap" or a rule that is wrong for one
	// repo can never be demoted on its own.
	Rule        string
	DedupeKey   string
	OverlapPath string

	// Two actions, because the same collision means opposite things to the
	// two agents. Never empty: "you and Ana both hold auth.go" is noise.
	ActionForInitiator string
	ActionForIncumbent string

	// NotifyInitiator is the notify floor applied to the caller's own
	// response. Below it the conflict is still recorded and still shows on
	// the board; it just does not interrupt.
	NotifyInitiator bool
	// InstructIncumbent additionally requires the two agents to belong to
	// different people. One human's two agents are coordinated by that human,
	// and PLAN.md is explicit: severity -1 and no interrupt.
	InstructIncumbent bool
	SameMember        bool
}

// assessOverlap is the whole decision, and it is a pure function.
//
// Confirm, score, name the rule, mint the dedupe key, write both suggested
// actions, apply the notify floor. Everything below this line in the file is
// plumbing around it.
func assessOverlap(mine, theirs claimSide, hotspots []string) overlapVerdict {
	// The SQL selected candidates by prefix ancestry, which over-reports on
	// purpose. This is the confirmation doublestar does.
	if !coordination.PathsOverlap(mine.Path, theirs.Path) {
		return overlapVerdict{}
	}

	sev, adjusters := coordination.PathConflictSeverity(coordination.SeverityInput{
		A:               sidePathClaim(mine),
		B:               sidePathClaim(theirs),
		HotspotPatterns: hotspots,
	})
	// read x read. Not a conflict and cannot become one — not even a row.
	if sev == coordination.SeverityNone {
		return overlapVerdict{}
	}

	hotspot := hasAdjuster(adjusters, coordination.PathAdjusterHotspot)
	broad := hasAdjuster(adjusters, coordination.PathAdjusterBroadClaim)
	sameMember := hasAdjuster(adjusters, coordination.PathAdjusterSameMember)

	overlap := overlapLabel(mine.Path, theirs.Path)
	v := overlapVerdict{
		Conflict:    true,
		Severity:    sev,
		Adjusters:   adjusters,
		Rule:        overlapRule(mine, theirs, hotspot, broad),
		DedupeKey:   coordination.PathDedupeKey(mine.ClaimUUID, theirs.ClaimUUID, overlap),
		OverlapPath: overlap,
		SameMember:  sameMember,
	}
	v.ActionForInitiator = suggestPathAction(mine, theirs, overlap, hotspot, broad, true)
	v.ActionForIncumbent = suggestPathAction(theirs, mine, overlap, hotspot, broad, false)
	v.NotifyInitiator = sev >= notifySeverityFloor
	v.InstructIncumbent = v.NotifyInitiator && !sameMember
	return v
}

func sidePathClaim(s claimSide) coordination.PathClaimSide {
	return coordination.PathClaimSide{
		Mode:       s.Mode,
		Path:       s.Path,
		MemberUUID: s.MemberUUID,
		Branch:     s.Branch,
	}
}

func hasAdjuster(adjusters []string, name string) bool {
	for _, a := range adjusters {
		if a == name {
			return true
		}
	}
	return false
}

// overlapRule names the most specific rule that explains this conflict, worst
// cause first. One name per conflict, because the dismissal-rate loop divides
// by it.
func overlapRule(a, b claimSide, hotspot, broad bool) string {
	switch {
	case hotspot:
		return "path_overlap.hotspot"
	case a.Mode == coordination.ModeStructural || b.Mode == coordination.ModeStructural:
		return "path_overlap.structural"
	case broad:
		return "path_overlap.broad"
	case a.Path.PatternNorm == b.Path.PatternNorm:
		return "path_overlap.same_path"
	default:
		return "path_overlap.glob"
	}
}

// overlapLabel names the file the two claims actually meet on, as well as it
// can be named without a filesystem.
//
// It MUST be symmetric: the same pair arrives in either order depending on who
// declared second, and an order-sensitive label would feed two different
// dedupe keys for one collision and put two rows on the board.
func overlapLabel(a, b coordination.NormalizedPath) string {
	switch {
	case a.Kind == coordination.PathKindExact && b.Kind != coordination.PathKindExact:
		return a.PatternNorm
	case b.Kind == coordination.PathKindExact && a.Kind != coordination.PathKindExact:
		return b.PatternNorm
	}
	// Neither side, or both sides, pins a file. The narrower claim is the
	// better description of where they meet; ties break lexicographically so
	// the answer does not depend on argument order.
	if a.Depth != b.Depth {
		if a.Depth > b.Depth {
			return a.PatternNorm
		}
		return b.PatternNorm
	}
	if a.PatternNorm <= b.PatternNorm {
		return a.PatternNorm
	}
	return b.PatternNorm
}

// suggestPathAction writes the line that turns a collision into something the
// agent can act on without asking anybody.
//
// subject is who is being advised; other is who they collided with. initiator
// says whether subject is the one who declared second — the one ordering made
// responsible, and the only one who is told synchronously.
//
// It never returns an empty string. A conflict with no suggested action is
// noise by definition, and the schema lets one be stored, so the guarantee has
// to live here.
func suggestPathAction(subject, other claimSide, overlap string, hotspot, broad, initiator bool) string {
	holder := describeHolder(other)
	held := humanAge(other.HeldFor)

	var b strings.Builder
	fmt.Fprintf(&b, "%s holds %s (%s", holder, other.Path.Pattern, other.Mode)
	if held != "" {
		fmt.Fprintf(&b, ", %s", held)
	}
	if other.Branch != "" {
		fmt.Fprintf(&b, ", %s", other.Branch)
	}
	b.WriteString("). ")

	switch {
	case hotspot:
		b.WriteString("This is a generated or lockfile-style path: regenerate after merge rather than hand-resolving, and say in your status line when you land it.")
	case broad:
		if initiator {
			b.WriteString("One of these claims covers a whole subtree — narrow yours to the files you will actually edit and re-declare, then this stops being ambiguous.")
		} else {
			b.WriteString("One of these claims covers a whole subtree, so this may be nothing — narrow yours if you are only in a few files.")
		}
	case other.Mode == coordination.ModeStructural:
		b.WriteString("They are renaming, moving or deleting it, which breaks your copy silently — ask before you edit, or work somewhere else until they land.")
	case subject.Mode == coordination.ModeStructural:
		b.WriteString("Your rename or delete will break them without any merge conflict to warn them — tell them before you land it.")
	case subject.Mode == coordination.ModeWrite && other.Mode == coordination.ModeWrite:
		if initiator {
			b.WriteString("You are both editing it: take a different file, or agree who goes first before you start.")
		} else {
			b.WriteString("You are both editing it: they declared after you, so they are expecting an answer — say whether you are nearly done.")
		}
	case subject.Mode == coordination.ModeWrite:
		b.WriteString("They are only reading it — carry on, and tell them in your status line when the change lands.")
	default:
		b.WriteString("They are editing it — re-read after they land rather than working from your copy.")
	}

	if subject.MemberUUID != "" && subject.MemberUUID == other.MemberUUID {
		b.WriteString(" (Both agents are yours, so this is yours to sequence.)")
	}
	return truncate(b.String(), 400)
}

// describeHolder names the other side the way a teammate would: the person
// first, their agent second, the session key only when there is no person to
// name.
func describeHolder(s claimSide) string {
	switch {
	case s.MemberName != "" && s.AgentLabel != "":
		return fmt.Sprintf("%s (%s)", s.MemberName, s.AgentLabel)
	case s.MemberName != "":
		return s.MemberName
	case s.AgentLabel != "":
		return s.AgentLabel
	case s.SessionKey != "":
		return "session " + s.SessionKey
	default:
		return "another agent"
	}
}

// humanAge renders how long a claim has been held, for a reader who only
// cares about the order of magnitude.
func humanAge(d time.Duration) string {
	switch {
	case d < 0:
		return ""
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// ─────────────────────────────────────────────
// Writing it down
// ─────────────────────────────────────────────

type conflictRow struct {
	ID       uuid.UUID
	Key      string
	Severity coordination.Severity
	// Existed is true when this pair had already been recorded and this
	// detection only bumped its counter.
	Existed bool
}

// record upserts one conflict, attaches both participants, and raises the
// instruction the incumbent will collect on their next call.
//
// All of it inside the caller's transaction, under the team lock. Two agents
// cannot both be here at once for the same team, which is what makes
// "check whether this conflict exists, then insert it" safe without any
// further locking.
func (d *pathDetector) record(ctx context.Context, tc *TxContext, req *pathDetectionRequest, o pathOverlap, n int) (conflictRow, error) {
	evidence, err := json.Marshal(conflict_evidence_entity.ConflictEvidence{
		OverlapPath: nullString(o.Verdict.OverlapPath),
		ALabel:      nullString(describeHolder(o.Mine)),
		ASummary:    nullString(truncate(req.IntentSummary, 280)),
		APattern:    nullString(o.Mine.Path.PatternNorm),
		BLabel:      nullString(describeHolder(o.Theirs)),
		BPattern:    nullString(o.Theirs.Path.PatternNorm),
		Adjusters:   o.Verdict.Adjusters,
		Detail:      nullString(o.Verdict.Rule),
	})
	if err != nil {
		return conflictRow{}, err
	}
	// string(), NOT the []byte json.Marshal handed us, and this is not
	// cosmetic. go-sql-driver/mysql sends a []byte argument as a binary
	// literal, and MySQL refuses a binary string for a JSON column:
	//
	//   Error 3144 (22032): Cannot create a JSON value from a string with
	//   CHARACTER SET 'binary'
	//
	// It only bites when the DSN carries interpolateParams=true — which the
	// generated config/base.yaml recommends and production uses, while the
	// test DSN does not. So this failed in a real deployment while every
	// integration test stayed green. Any JSON written through hand-written
	// SQL in this package must be a string for the same reason.
	evidenceJSON := string(evidence)

	id, err := uuid.NewV4()
	if err != nil {
		return conflictRow{}, err
	}
	sev := severityEnum(o.Verdict.Severity)
	yield := req.SessionUUID.String()

	// Upsert on the dedupe key. uq_conflict_dedupe is what turns re-detection
	// into a counter bump instead of a second row: the same pair of claims on
	// the same file collides on every single declaration either agent makes,
	// and without this the board would fill with one collision repeated.
	//
	// What deliberately does NOT move on a duplicate: status. A conflict a
	// human dismissed must stay dismissed, or the self-tuning loop is just a
	// slower way of shouting. Severity only ever climbs, so an escalation is
	// still visible.
	if _, err := tc.Tx.ExecContext(ctx,
		"INSERT INTO `conflict` (`id`, `team_uuid`, `project_uuid`, `key`, `kind`, `dedupe_key`, `severity`, "+
			"`status`, `detected_by`, `detector_rule`, `evidence`, `suggested_action`, "+
			"`suggested_yield_session_uuid`, `suggested_yield_reason`, `occurrence_count`, "+
			"`first_detected_at`, `last_detected_at`, `created_at`, `updated_at`) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE "+
			"`occurrence_count` = `occurrence_count` + 1, "+
			"`last_detected_at` = VALUES(`last_detected_at`), "+
			"`severity` = GREATEST(`severity`, VALUES(`severity`)), "+
			"`suggested_action` = VALUES(`suggested_action`), "+
			"`detector_rule` = VALUES(`detector_rule`), "+
			"`updated_at` = VALUES(`updated_at`)",
		id.String(), req.TeamUUID.String(), req.ProjectUUID.String(), conflictKey(tc, n),
		int64(enums.CONFLICT_KIND_PATH_OVERLAP), o.Verdict.DedupeKey, int64(sev),
		int64(enums.CONFLICT_STATUS_OPEN), int64(enums.DETECTED_BY_SERVER),
		truncate(o.Verdict.Rule, 48), evidenceJSON, o.Verdict.ActionForInitiator,
		yield, "declared later", tc.Now, tc.Now, tc.Now, tc.Now); err != nil {
		return conflictRow{}, retryable(err, "recording the conflict")
	}

	var (
		row       conflictRow
		rawID     string
		rawSev    int64
		count     int64
		notified  sql.NullInt64
		statusRaw int64
	)
	if err := tc.Tx.QueryRowContext(ctx,
		"SELECT `id`, `key`, `severity`, `status`, `occurrence_count`, `max_severity_notified` "+
			"FROM `conflict` WHERE `team_uuid` = ? AND `dedupe_key` = ?",
		req.TeamUUID.String(), o.Verdict.DedupeKey).
		Scan(&rawID, &row.Key, &rawSev, &statusRaw, &count, &notified); err != nil {
		return conflictRow{}, retryable(err, "reading back the conflict")
	}
	row.ID, err = uuid.FromString(rawID)
	if err != nil {
		return conflictRow{}, err
	}
	row.Severity = coordination.Severity(rawSev)
	row.Existed = count > 1

	// Participants: one row per side, inserted once. The child table is what
	// makes "conflicts involving my session" an index lookup, and it is what
	// the pending count on every response is counting.
	if err := upsertParticipant(ctx, tc, req.TeamUUID, row.ID, o.Mine.SessionUUID, o.Mine.AgentUUID,
		o.Mine.MemberUUID, o.Mine.ClaimUUID, enums.PARTICIPANT_ROLE_INITIATOR, tc.Now); err != nil {
		return conflictRow{}, err
	}
	if err := upsertParticipant(ctx, tc, req.TeamUUID, row.ID, o.Theirs.SessionUUID, o.Theirs.AgentUUID,
		o.Theirs.MemberUUID, o.Theirs.ClaimUUID, enums.PARTICIPANT_ROLE_INCUMBENT, tc.Now); err != nil {
		return conflictRow{}, err
	}

	// The incumbent cannot be pushed to — MCP is request/response — so the
	// notice waits for them as an instruction and rides out on the pending
	// count of their very next call, including a bare heartbeat. The caller
	// is told synchronously instead, because they have the information in
	// hand and have not started yet.
	//
	// Re-notify only on escalation: the pair is re-detected on every
	// declaration either agent makes, and a second identical instruction is
	// the definition of crying wolf.
	if o.Verdict.InstructIncumbent && (!notified.Valid || int64(sev) > notified.Int64) {
		if err := d.raiseIncumbentNotice(ctx, tc, req, o, row, n); err != nil {
			return conflictRow{}, err
		}
		if _, err := tc.Tx.ExecContext(ctx,
			"UPDATE `conflict` SET `notified_at` = ?, `max_severity_notified` = ?, `updated_at` = ? WHERE `id` = ?",
			tc.Now, int64(sev), tc.Now, row.ID.String()); err != nil {
			return conflictRow{}, retryable(err, "marking the conflict notified")
		}
	}
	return row, nil
}

func (d *pathDetector) raiseIncumbentNotice(ctx context.Context, tc *TxContext, req *pathDetectionRequest, o pathOverlap, row conflictRow, n int) error {
	id, err := uuid.NewV4()
	if err != nil {
		return err
	}
	body := fmt.Sprintf("%s (%s) on %s: %s",
		row.Key, o.Verdict.Severity.String(), o.Verdict.OverlapPath, o.Verdict.ActionForIncumbent)
	if req.IntentSummary != "" {
		body += fmt.Sprintf(" They said they are about to: %s", truncate(req.IntentSummary, 160))
	}

	sessionUUID := o.Theirs.SessionUUID
	agentUUID := o.Theirs.AgentUUID
	expires := tc.Now.Add(detectInstructionTTL)
	if _, err := tc.Tx.ExecContext(ctx,
		"INSERT INTO `instruction` (`id`, `team_uuid`, `target_session_uuid`, `target_agent_uuid`, `key`, "+
			"`source`, `kind`, `body`, `ref_kind`, `ref_uuid`, `requires_report`, `status`, "+
			"`expires_at`, `created_at`, `updated_at`) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)",
		id.String(), req.TeamUUID.String(), sessionUUID, agentUUID, instructionKey(tc, n),
		int64(enums.INSTRUCTION_SOURCE_SERVER), int64(enums.INSTRUCTION_KIND_CONFLICT_NOTICE),
		truncate(body, 600), int64(enums.SUBJECT_KIND_CONFLICT), row.ID.String(),
		int64(enums.INSTRUCTION_STATUS_PENDING), expires, tc.Now, tc.Now); err != nil {
		return retryable(err, "queueing the conflict notice for the other agent")
	}
	return nil
}

// upsertParticipant attaches one side to a conflict, once.
//
// Checked then inserted rather than an ON DUPLICATE KEY, because there is no
// unique index to hang one on — and none is needed: this runs under the team
// lock, so no concurrent writer can insert between the two statements.
func upsertParticipant(ctx context.Context, tc *TxContext, teamUUID, conflictUUID uuid.UUID,
	sessionUUID, agentUUID, memberUUID, claimUUID string, role enums.ParticipantRole, now time.Time) error {
	if sessionUUID == "" || agentUUID == "" || memberUUID == "" {
		return fmt.Errorf("cannot attach a conflict participant without a session, agent and member (session=%q agent=%q member=%q)",
			sessionUUID, agentUUID, memberUUID)
	}
	var exists int
	err := tc.Tx.QueryRowContext(ctx,
		"SELECT 1 FROM `conflict_participant` WHERE `conflict_uuid` = ? AND `session_uuid` = ? LIMIT 1",
		conflictUUID.String(), sessionUUID).Scan(&exists)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return retryable(err, "checking the conflict participants")
	}

	id, err := uuid.NewV4()
	if err != nil {
		return err
	}
	if _, err := tc.Tx.ExecContext(ctx,
		"INSERT INTO `conflict_participant` (`id`, `conflict_uuid`, `team_uuid`, `session_uuid`, `agent_uuid`, "+
			"`member_uuid`, `subject_kind`, `subject_uuid`, `role`, `created_at`, `updated_at`) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		id.String(), conflictUUID.String(), teamUUID.String(), sessionUUID, agentUUID, memberUUID,
		int64(enums.SUBJECT_KIND_CLAIM), claimUUID, int64(role), now, now); err != nil {
		return retryable(err, "attaching the conflict participants")
	}
	return nil
}

// conflictKey and instructionKey mint the short human-facing keys.
//
// tc.Key is unique per team because the sequence is, but ONE call can raise
// several conflicts, so the first takes the bare key and the rest are
// suffixed. Uniqueness still comes from the sequence; the suffix only
// separates siblings within one event.
func conflictKey(tc *TxContext, n int) string {
	if n == 0 {
		return tc.Key("CF")
	}
	return fmt.Sprintf("%s.%d", tc.Key("CF"), n+1)
}

func instructionKey(tc *TxContext, n int) string {
	if n == 0 {
		return tc.Key("IN")
	}
	return fmt.Sprintf("%s.%d", tc.Key("IN"), n+1)
}

// ─────────────────────────────────────────────
// Enum plumbing
//
// coordination speaks strings because it is a pure package with no dependency
// on the generated code; the database speaks the generated int enums. These
// four functions are the whole translation, in one place so a new mode cannot
// be half-added.
// ─────────────────────────────────────────────

func claimModeEnum(m coordination.ClaimMode) enums.ClaimMode {
	switch m {
	case coordination.ModeRead:
		return enums.CLAIM_MODE_READ
	case coordination.ModeWrite:
		return enums.CLAIM_MODE_WRITE
	case coordination.ModeStructural:
		return enums.CLAIM_MODE_STRUCTURAL
	default:
		return enums.CLAIM_MODE_INVALID
	}
}

func claimModeToCoordination(m enums.ClaimMode) coordination.ClaimMode {
	switch m {
	case enums.CLAIM_MODE_READ:
		return coordination.ModeRead
	case enums.CLAIM_MODE_STRUCTURAL:
		return coordination.ModeStructural
	default:
		// write is the safe default for an unknown value: it over-reports a
		// collision rather than silently downgrading one to read x read,
		// which is the only case that is never a conflict at all.
		return coordination.ModeWrite
	}
}

func pathKindEnum(k coordination.PathKind) enums.ClaimPathKind {
	switch k {
	case coordination.PathKindExact:
		return enums.CLAIM_PATH_KIND_EXACT
	case coordination.PathKindPrefix:
		return enums.CLAIM_PATH_KIND_PREFIX
	default:
		return enums.CLAIM_PATH_KIND_GLOB
	}
}

func pathKindToCoordination(k enums.ClaimPathKind) coordination.PathKind {
	switch k {
	case enums.CLAIM_PATH_KIND_EXACT:
		return coordination.PathKindExact
	case enums.CLAIM_PATH_KIND_PREFIX:
		return coordination.PathKindPrefix
	default:
		return coordination.PathKindGlob
	}
}

func severityEnum(s coordination.Severity) enums.ConflictSeverity {
	switch s {
	case coordination.SeverityLow:
		return enums.CONFLICT_SEVERITY_LOW
	case coordination.SeverityMedium:
		return enums.CONFLICT_SEVERITY_MEDIUM
	case coordination.SeverityHigh:
		return enums.CONFLICT_SEVERITY_HIGH
	case coordination.SeverityCritical:
		return enums.CONFLICT_SEVERITY_CRITICAL
	default:
		return enums.CONFLICT_SEVERITY_INVALID
	}
}

// normalizedPathFromRow rebuilds the value the rules need from the columns
// claim_path denormalized it into, so a candidate goes through exactly the
// same overlap test as a freshly normalized pattern.
//
// BreadthScore is recomputed rather than stored: it is a pure function of
// depth and kind, and a stored copy would go stale the day the formula
// changes, silently, on rows nobody rewrites.
func normalizedPathFromRow(pattern, norm string, kind enums.ClaimPathKind, prefix, suffix string, depth int64, ext string) coordination.NormalizedPath {
	np := coordination.NormalizedPath{
		Pattern:       pattern,
		PatternNorm:   norm,
		Kind:          pathKindToCoordination(kind),
		Prefix:        prefix,
		SuffixPattern: suffix,
		Depth:         int(depth),
		Ext:           ext,
	}
	np.BreadthScore = 100 / (np.Depth + 1)
	if np.IsBroad() {
		np.BreadthScore = coordination.PathBroadBreadthScore
	}
	return np
}

// ─────────────────────────────────────────────
// Project lookup
// ─────────────────────────────────────────────

// loadProjectByUUID reads the project a session belongs to, for its ignore and
// hotspot lists.
//
// Cache skipped like every other read in this package: a project whose ignore
// patterns were edited thirty seconds ago must not still be dropping the paths
// it used to drop.
func (h *Handler) loadProjectByUUID(ctx context.Context, id uuid.UUID) (project_entity.Project, error) {
	res, err := h.core.Project().FetchProjectByID(ctx,
		project_types.FetchProjectByIDRequest{ID: id}, projectmod.WithSkipCache())
	if err != nil {
		return project_entity.Project{}, retryable(err, "looking up the project")
	}
	if len(res.Results) == 0 {
		return project_entity.Project{}, fmt.Errorf("project %s no longer exists", id)
	}
	p := res.Results[0]
	if len(p.IgnorePatterns) == 0 {
		p.IgnorePatterns = coordination.DefaultIgnorePatterns()
	}
	if len(p.HotspotPatterns) == 0 {
		p.HotspotPatterns = coordination.DefaultHotspotPatterns()
	}
	return p, nil
}
