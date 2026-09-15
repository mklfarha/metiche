package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mklfarha/metiche/backend/app/coordination"
)

// checkpaths.go is the pre-flight: who else is in these files, answered
// without claiming anything.
//
// It exists because the alternative is worse. Without it an agent that wants
// to know whether a directory is busy has to declare an intent on it, which
// takes the files, tells everybody, and then has to be undone if the agent
// decides to work somewhere else. That turns "I was looking around" into a
// conflict on somebody's board.
//
// So: readOnly, no transaction, no team lock, no rows written, no conflict
// raised, no instruction queued, and nothing an agent has to undo. The
// queries are the SAME ones declare_intent runs — a pre-flight that answers
// differently from the real call is worse than no pre-flight at all — they
// just run against the pool instead of inside the write transaction, and
// nothing is recorded.
//
// The honest caveat, stated in the response rather than hidden: the answer is
// true at the instant it was read and is not a reservation. Two agents that
// both check and both like what they see still collide; the only thing that
// decides who was first is declare_intent, which does its checking inside the
// lock.

// CheckPathsMaxFindings bounds what comes back. A pre-flight that returns
// forty holders is not a pre-flight, it is a report nobody reads.
const CheckPathsMaxFindings = 10

type CheckPathsParams struct {
	SessionKey string   `json:"session_key" jsonschema:"The session_key start_session gave you."`
	TeamSlug   string   `json:"team_slug,omitempty" jsonschema:"The team this session is on, by slug: the team_slug start_session returned. Optional on one team; pass team_slug when you are on more than one team, because session keys are per team."`
	Paths      []string `json:"paths" jsonschema:"The specific files, or the narrowest folder, you are thinking about touching: 'app/rest.go', 'app/mcp/*.go'. Relative to the git root ('git rev-parse --show-toplevel'), NOT to your working directory: send 'app/rest.go', never 'myrepo/app/rest.go' or 'rest.go'. An absolute path is refused. '*' does not cross '/', so '*.go' means files at the repo root only. A repo-wide pattern ('**', '**/*.go') comes back scored low against everyone, which tells you little. Nothing is claimed and nobody is told you asked."`
	Mode       string   `json:"mode,omitempty" jsonschema:"What you would be doing to them - read, write (the default) or structural. It changes the answer: two readers are never a conflict, and a structural change collides with everything."`
}

// PathHolder is one other session already holding something these paths
// would touch.
type PathHolder struct {
	// Path is where the two claims meet, as close to a file as it can be
	// named without a filesystem.
	Path string `json:"path"`
	// YourPath is which of the paths you asked about hit it.
	YourPath string `json:"your_path"`
	HeldBy   string `json:"held_by"`
	Session  string `json:"session"`
	Pattern  string `json:"pattern"`
	Mode     string `json:"mode"`
	Branch   string `json:"branch,omitempty"`
	HeldFor  string `json:"held_for,omitempty"`
	// WouldBe is the severity this WOULD be scored at if you declared it.
	// Nothing has been recorded, so it is a forecast, not a conflict.
	WouldBe string `json:"would_be"`
	// SuggestedAction is never empty. A holder with no advice attached is
	// exactly the noise this whole system is trying not to produce.
	SuggestedAction string `json:"suggested_action"`
	// Doing is the other session's status line, when they set one. It is the
	// single most useful field here: "who" plus "what right now" is usually
	// enough for an agent to decide on its own.
	Doing string `json:"doing,omitempty"`
}

// CheckPathsResult is the envelope every tool returns, plus what was found.
type CheckPathsResult struct {
	Envelope
	// Checked is what the paths normalized to, so an agent can see that
	// "src/api" became "src/api/**" before it wonders why the answer is
	// broader than it expected.
	Checked []string `json:"checked,omitempty"`
	// Ignored is what the project drops: generated code, vendored trees,
	// lockfiles. Reported rather than silently removed, because an agent that
	// asked about a path deserves to know it is not being tracked.
	Ignored []string     `json:"ignored,omitempty"`
	Holders []PathHolder `json:"holders,omitempty"`
	// MoreHolders is how many were left out of the list.
	MoreHolders int `json:"more_holders,omitempty"`
	// Advisory says plainly that this reserved nothing.
	Advisory string `json:"advisory"`
}

// CheckPaths answers "who else is in these files" and commits the caller to
// nothing.
func (h *Handler) CheckPaths(ctx context.Context, _ *mcp.CallToolRequest, args CheckPathsParams) (*mcp.CallToolResult, any, error) {
	who, err := h.RequireSessionOnTeam(ctx, args.SessionKey, args.TeamSlug)
	if err != nil {
		return nil, nil, err
	}
	ag, sess := who.Agent, who.Session

	mode, err := parseClaimMode(args.Mode)
	if err != nil {
		return nil, nil, err
	}
	proj, err := h.loadProjectByUUID(ctx, sess.ProjectUUID)
	if err != nil {
		return nil, nil, err
	}
	paths, ignored, err := normalizeClaimPaths(args.Paths, proj)
	if err != nil {
		return nil, nil, err
	}
	if len(paths) == 0 && len(ignored) == 0 {
		return nil, nil, fmt.Errorf(
			"paths is required — pass the files or globs you are thinking about, like internal/auth/token.go or src/api/**")
	}

	now := time.Now().UTC()
	db := h.core.DB()

	// The same request the write path builds, with one difference: the claim
	// uuids are nil, because there are no claims. Everything downstream —
	// the candidate scan, the doublestar confirmation, the severity matrix,
	// the suggested action — is the identical code path declare_intent runs.
	req := &pathDetectionRequest{
		TeamUUID:        who.Team.ID,
		ProjectUUID:     sess.ProjectUUID,
		SessionUUID:     sess.ID,
		AgentUUID:       ag.ID,
		MemberUUID:      who.Member.ID,
		SessionKey:      sess.Key,
		Branch:          sess.Branch.ValueOrZero(),
		MemberName:      who.Member.DisplayName,
		AgentLabel:      ag.Label,
		HotspotPatterns: proj.HotspotPatterns,
	}
	for _, p := range paths {
		req.Declared = append(req.Declared, pathDeclaration{Mode: mode, Path: p})
	}

	found, err := findPathOverlaps(ctx, db, req, now)
	if err != nil {
		return nil, nil, err
	}

	// One holder line per session per path, and the worst first. Two of their
	// claims on the same file is one thing to know, not two.
	seen := map[string]bool{}
	holders := make([]PathHolder, 0, len(found))
	more := 0
	for _, o := range found {
		k := o.Theirs.SessionUUID + "|" + o.Verdict.OverlapPath
		if seen[k] {
			continue
		}
		seen[k] = true
		if len(holders) >= CheckPathsMaxFindings {
			more++
			continue
		}
		holders = append(holders, PathHolder{
			Path:            o.Verdict.OverlapPath,
			YourPath:        o.Mine.Path.PatternNorm,
			HeldBy:          describeHolder(o.Theirs),
			Session:         o.Theirs.SessionKey,
			Pattern:         o.Theirs.Path.Pattern,
			Mode:            string(o.Theirs.Mode),
			Branch:          o.Theirs.Branch,
			HeldFor:         humanAge(o.Theirs.HeldFor),
			WouldBe:         o.Verdict.Severity.String(),
			SuggestedAction: o.Verdict.ActionForInitiator,
		})
	}

	// The cursors and the pending counts ride on every response, including
	// this one. Read without locking: a read-only tool that queued behind
	// every writer on the team to report a number it does not change would be
	// the most expensive cheap call in the system.
	var seq, rev int64
	if err := db.QueryRowContext(ctx,
		"SELECT `sequence`, `board_revision` FROM `team` WHERE `id` = ?", who.Team.ID.String()).
		Scan(&seq, &rev); err != nil {
		return nil, nil, retryable(err, "reading the team cursors")
	}
	pending, err := h.pendingCounts(ctx, db, uuidPtr(sess.ID))
	if err != nil {
		return nil, nil, retryable(err, "counting pending work")
	}

	env := Envelope{OK: true, Key: sess.Key, Sequence: seq, Revision: rev}.withPending(pending)
	if env.Note == "" {
		env.Note = checkNote(len(paths), len(holders)+more, mode)
	}

	return jsonValue(CheckPathsResult{
		Envelope:    env,
		Checked:     patternList(paths),
		Ignored:     ignored,
		Holders:     holders,
		MoreHolders: more,
		Advisory: "Nothing was claimed and nobody was told you asked. This is true as of now, not a reservation — " +
			"call declare_intent to actually take the files, which is also the only check that runs inside the lock.",
	})
}

func checkNote(checked, holders int, mode coordination.ClaimMode) string {
	if holders == 0 {
		return fmt.Sprintf("%d path(s) checked for %s: nobody else is holding them right now — declare_intent to take them",
			checked, mode)
	}
	return fmt.Sprintf("%d path(s) checked for %s: %s already held — read holders[] before you decide",
		checked, mode, pluralPaths(holders))
}

func pluralPaths(n int) string {
	if n == 1 {
		return "1 is"
	}
	return strings.TrimSpace(fmt.Sprintf("%d are", n))
}
