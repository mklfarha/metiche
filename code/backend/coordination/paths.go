package coordination

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// ─────────────────────────────────────────────
// Normalization
// ─────────────────────────────────────────────

// PathKind classifies a normalized claim pattern by how it has to be matched.
// It is stored denormalized on claim_path so the hot detection query can filter
// on it without re-parsing the pattern.
type PathKind string

const (
	// PathKindExact is a single file: no wildcard anywhere in the pattern.
	PathKindExact PathKind = "exact"
	// PathKindPrefix is a whole subtree - the pattern is its literal prefix
	// followed by "**" and nothing else. "src", "src/" and "src/**" all
	// normalize to this.
	PathKindPrefix PathKind = "prefix"
	// PathKindGlob is everything else: a wildcard somewhere other than a single
	// trailing "**".
	PathKindGlob PathKind = "glob"
)

// pathWildcardChars are the characters that end the literal prefix. "[" and "{"
// are in here even though agents rarely write them, because a pattern like
// src/{api,web}/**  whose prefix was computed as "src/{api,web}/" would produce
// a prefix that can never be an ancestor of anything and would silently stop
// matching candidates.
const pathWildcardChars = `*?[{`

// PathBroadBreadthScore is the BreadthScore of a claim so wide that a conflict
// on it carries almost no information ("I am working in src/"). The severity
// rules cap those at low rather than shouting about them.
const PathBroadBreadthScore = 100

// PathMaxAncestors bounds the ancestor list handed to the SQL candidate query.
const PathMaxAncestors = 12

// NormalizedPath is one claim pattern after normalization, with everything the
// candidate query and the overlap rules need precomputed. The fields below
// (except Pattern) are what get denormalized onto claim_path.
type NormalizedPath struct {
	// Pattern is exactly what the agent sent, kept for display only. Every
	// comparison in this file uses PatternNorm.
	Pattern string
	// PatternNorm is the canonical form: forward slashes, no "./", no repeated
	// separators, directories expanded to "/**", lowercased if the project is
	// configured case-insensitive.
	PatternNorm string
	Kind        PathKind
	// Prefix is the longest literal prefix truncated back to the last "/" at or
	// before the first wildcard. Two patterns can only overlap if one Prefix is
	// an ancestor-or-equal of the other, which is what makes candidate
	// selection a sargable index range scan instead of a table scan.
	Prefix string
	// SuffixPattern is PatternNorm with Prefix removed - the part that still
	// has to go through doublestar.
	SuffixPattern string
	// Depth is the number of non-empty segments in Prefix.
	Depth int
	// Ext is the extension of the last segment, including the dot, and only
	// when that segment is literal. A wildcarded last segment leaves it empty,
	// which the glob x glob rule reads as "could be any file type".
	Ext string
	// BreadthScore is 100 for a repo-wide or top-level claim and falls off with
	// depth. It drives the "narrow this" nudge and the low-severity cap.
	BreadthScore int
}

// IsBroad reports whether the claim is at the over-broad threshold. A claim on
// "src/**" overlaps essentially everyone, so reporting it at its natural
// severity would train the team to ignore conflicts entirely.
//
// An exact path is never broad, however shallow it is. Depth counts prefix
// segments, so a file at the repo root - go.mod, .env, README.md - is depth 1
// and would otherwise be treated as over-broad, which inverts the truth: those
// are the narrowest claims expressible, and two agents writing go.mod is the
// single most actionable collision in the system.
func (n NormalizedPath) IsBroad() bool {
	if n.Kind == PathKindExact {
		return false
	}
	return n.Depth <= 1 || n.PatternNorm == "**"
}

// Sentinel reasons a pattern is not usable as a claim. All of them arrive at
// the agent as text, so they are phrased as something an agent can act on.
var (
	ErrPathEmpty     = errors.New("path is empty")
	ErrPathAbsolute  = errors.New("path is absolute; claims are repo-relative")
	ErrPathTraversal = errors.New(`path contains a ".." segment`)
	ErrPathNullByte  = errors.New("path contains a null byte")
	// ErrPathIgnored is not a user error. The pattern is well formed but the
	// project ignores it, and the caller drops the claim silently - it never
	// becomes a row. Generated code is the whole reason this exists: without
	// it every agent collides with every other agent on regenerated CRUD.
	ErrPathIgnored = errors.New("path matches an ignore pattern")
)

// PathError carries the offending pattern alongside the reason so the message
// an agent sees names the path it sent, not just the rule it broke.
type PathError struct {
	Pattern string
	Err     error
	// Detail names the specific ignore or glob entry that matched, when there
	// is one. "vendor/foo.go was ignored" is a bug report; "vendor/foo.go was
	// ignored by vendor/**" is an answer.
	Detail string
}

func (e *PathError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("path %q: %v (%s)", e.Pattern, e.Err, e.Detail)
	}
	return fmt.Sprintf("path %q: %v", e.Pattern, e.Err)
}

func (e *PathError) Unwrap() error { return e.Err }

func pathErrorf(pattern string, err error, detail string) error {
	return &PathError{Pattern: pattern, Err: err, Detail: detail}
}

// DefaultIgnorePatterns is the starting ignore list for a new project. The
// generated-code entries are the load-bearing ones: nuzur writes most of the Go
// in this repo, so without them every agent holds a claim on every other
// agent's files and the conflict feed is pure noise from the first minute.
func DefaultIgnorePatterns() []string {
	return []string{
		"vendor/**",
		"node_modules/**",
		"dist/**",
		"build/**",
		"*.lock",
		"**/*.gen.go",
		"**/*_gen.go",
		"**/*.generated.*",
		".git/**",
	}
}

// DefaultHotspotPatterns is the starting hotspot list. These are the files
// where two independent edits merge cleanly and are still wrong - the fix is
// "regenerate after merge", not "hand-resolve" - so a collision on them is
// escalated one level.
func DefaultHotspotPatterns() []string {
	return []string{
		"go.mod",
		"go.sum",
		"package.json",
		"*.lock",
		"db/migrations/**",
		"**/schema.sql",
		".env*",
	}
}

// NormalizePath turns an agent-reported path pattern into a NormalizedPath.
//
// It returns a *PathError wrapping one of the sentinels above. A pattern the
// project ignores comes back as ErrPathIgnored, which the caller must treat as
// "drop it", not as a failure of the whole declare_intent call - an agent that
// lists twelve paths of which one is generated should not have the other eleven
// rejected.
func NormalizePath(pattern string, ignore []string, caseInsensitive bool) (NormalizedPath, error) {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return NormalizedPath{}, pathErrorf(pattern, ErrPathEmpty, "")
	}
	if strings.ContainsRune(p, 0) {
		return NormalizedPath{}, pathErrorf(pattern, ErrPathNullByte, "")
	}

	// Separator rewriting happens BEFORE the absolute/traversal rejection, not
	// after. Checking the raw string first would wave `..\..\etc` and `\etc`
	// through and then rewrite them into exactly the traversal and absolute
	// path we just decided to reject.
	p = strings.ReplaceAll(p, `\`, "/")
	p = pathCollapseSlashes(p)
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	if p == "." {
		p = ""
	}
	if strings.HasPrefix(p, "/") {
		return NormalizedPath{}, pathErrorf(pattern, ErrPathAbsolute, "")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return NormalizedPath{}, pathErrorf(pattern, ErrPathTraversal, "")
		}
	}

	switch {
	case p == "":
		// "." or "./" - the agent means the whole repo. It is a legal thing to
		// mean, and the breadth cap already keeps it from being loud.
		p = "**"
	case strings.HasSuffix(p, "/"):
		p += "**"
	case !pathHasWildcard(p):
		// A wildcard-free last segment with no dot is a directory, not a file.
		// This deliberately mis-reads extensionless files (Makefile, LICENSE)
		// as directories: over-claiming a directory costs one low-severity
		// false positive, under-claiming a directory means two agents rewrite
		// the same package and neither is told.
		if last := p[strings.LastIndex(p, "/")+1:]; !strings.Contains(last, ".") {
			p += "/**"
		}
	}

	norm := p
	if caseInsensitive {
		norm = strings.ToLower(norm)
	}

	// The ignore test runs against the normalized form so that "vendor",
	// "vendor/" and "vendor/x.go" are all caught by the single entry
	// "vendor/**".
	for _, ig := range ignore {
		if pathGlobMatch(ig, norm) {
			return NormalizedPath{}, pathErrorf(pattern, ErrPathIgnored, "matched "+ig)
		}
	}

	prefix := pathLiteralPrefix(norm)
	out := NormalizedPath{
		Pattern:       pattern,
		PatternNorm:   norm,
		Prefix:        prefix,
		SuffixPattern: strings.TrimPrefix(norm, prefix),
		Depth:         pathSegmentCount(prefix),
	}

	switch {
	case !pathHasWildcard(norm):
		out.Kind = PathKindExact
	case norm == prefix+"**":
		out.Kind = PathKindPrefix
	default:
		out.Kind = PathKindGlob
	}

	if last := norm[strings.LastIndex(norm, "/")+1:]; !pathHasWildcard(last) {
		ext := path.Ext(last)
		// path.Ext(".env") is ".env": a dotfile's whole name looks like an
		// extension. Treating it as one would make .env and .envrc share a
		// "type" and collide in the glob x glob rule.
		if ext != last {
			out.Ext = ext
		}
	}

	out.BreadthScore = 100 / (out.Depth + 1)
	if out.IsBroad() {
		out.BreadthScore = PathBroadBreadthScore
	}
	return out, nil
}

func pathHasWildcard(s string) bool { return strings.ContainsAny(s, pathWildcardChars) }

func pathCollapseSlashes(s string) string {
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return s
}

// pathLiteralPrefix returns the literal head of a pattern, cut back to the last
// "/" at or before the first wildcard. Cutting to a segment boundary rather
// than to the wildcard itself is what makes prefixes composable: every prefix
// is either "" or ends in "/", so ancestry is a plain string-prefix test and
// "src/api*" cannot be mistaken for an ancestor of "src/apiv2/".
func pathLiteralPrefix(p string) string {
	i := strings.IndexAny(p, pathWildcardChars)
	if i < 0 {
		return p
	}
	if s := strings.LastIndex(p[:i], "/"); s >= 0 {
		return p[:s+1]
	}
	return ""
}

func pathSegmentCount(prefix string) int {
	n := 0
	for _, seg := range strings.Split(prefix, "/") {
		if seg != "" {
			n++
		}
	}
	return n
}

// pathGlobMatch is doublestar.Match with the error folded into "no match". The
// only error it returns is a malformed pattern, and a malformed entry in a
// project's ignore or hotspot list must not make every path claim fail.
func pathGlobMatch(pattern, name string) bool {
	ok, err := doublestar.Match(pattern, name)
	return err == nil && ok
}

// ─────────────────────────────────────────────
// Candidate selection
// ─────────────────────────────────────────────

// PathAncestors returns the prefixes that could hold a claim covering this one,
// shallowest first and always including "" (the repo-wide claim). It is the
// `prefix IN (:ancestors)` half of the candidate query; the other half,
// `prefix LIKE CONCAT(:my_prefix,'%')`, picks up equal-or-deeper prefixes.
//
// The list is capped at PathMaxAncestors because it is interpolated straight
// into an IN list: an agent reporting a pathologically nested path would
// otherwise grow the statement and the plan without bound, inside the team
// lock, which is the one place nothing may scale with user input. Truncation
// drops the DEEPEST entries on purpose - the shallow end holds the broad claims
// that actually collide, and the deepest entry is my own prefix, which the
// LIKE branch already covers.
func PathAncestors(prefix string) []string {
	out := []string{""}
	for i := range len(prefix) {
		if prefix[i] == '/' {
			out = append(out, prefix[:i+1])
		}
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		out = append(out, prefix)
	}
	if len(out) > PathMaxAncestors {
		out = out[:PathMaxAncestors]
	}
	return out
}

// ─────────────────────────────────────────────
// Overlap
// ─────────────────────────────────────────────

// PathsOverlap reports whether two normalized claims can touch the same file.
//
// stdlib path.Match has no "**", so every match here goes through doublestar.
// The glob x glob case cannot be decided exactly without a filesystem, so it
// uses the conservative rule from the plan: one prefix contains the other and
// the extensions do not positively disagree. It over-reports by design - the
// severity layer and the LLM judge are what turn a candidate into a signal, and
// a missed overlap is invisible while a spurious one is merely low.
func PathsOverlap(a, b NormalizedPath) bool {
	aExact := a.Kind == PathKindExact
	bExact := b.Kind == PathKindExact

	switch {
	case aExact && bExact:
		return a.PatternNorm == b.PatternNorm
	case aExact:
		return pathGlobMatch(b.PatternNorm, a.PatternNorm)
	case bExact:
		return pathGlobMatch(a.PatternNorm, b.PatternNorm)
	}

	if !pathPrefixCovers(a.Prefix, b.Prefix) && !pathPrefixCovers(b.Prefix, a.Prefix) {
		return false
	}
	return a.Ext == "" || b.Ext == "" || a.Ext == b.Ext
}

// pathPrefixCovers reports whether outer is an ancestor of, or equal to, inner.
func pathPrefixCovers(outer, inner string) bool {
	if outer == "" || outer == inner {
		return true
	}
	if !strings.HasSuffix(outer, "/") {
		outer += "/"
	}
	return strings.HasPrefix(inner, outer)
}

// ─────────────────────────────────────────────
// Severity
// ─────────────────────────────────────────────

// Severity is ordered: the adjusters move a conflict up and down this scale by
// one step at a time, so the numeric order is part of the contract.
type Severity int

const (
	SeverityNone Severity = iota
	SeverityLow
	SeverityMedium
	SeverityHigh
	SeverityCritical
)

func (s Severity) String() string {
	switch s {
	case SeverityNone:
		return "none"
	case SeverityLow:
		return "low"
	case SeverityMedium:
		return "medium"
	case SeverityHigh:
		return "high"
	case SeverityCritical:
		return "critical"
	default:
		return fmt.Sprintf("severity(%d)", int(s))
	}
}

// ClaimMode is what a claim intends to do with the path. structural (rename,
// move, delete) is separate from write because it breaks readers silently:
// nothing in the other agent's working copy changes, their build just stops.
type ClaimMode string

const (
	ModeRead       ClaimMode = "read"
	ModeWrite      ClaimMode = "write"
	ModeStructural ClaimMode = "structural"
)

// Names of the deterministic adjusters, recorded on the conflict so a human
// asking "why is this only low?" gets an answer instead of a number.
const (
	PathAdjusterSameMember = "same_member"
	PathAdjusterSameBranch = "same_branch"
	PathAdjusterBroadClaim = "broad_claim_cap"
	PathAdjusterHotspot    = "hotspot"
)

// PathClaimSide is one participant in a candidate overlap.
type PathClaimSide struct {
	Mode       ClaimMode
	Path       NormalizedPath
	MemberUUID string
	Branch     string
}

// SeverityInput is everything the severity rules may look at. It is a value
// type with no handles on purpose: severity must be reproducible from the
// conflict row alone, which it would not be if the rules could query.
type SeverityInput struct {
	A PathClaimSide
	B PathClaimSide
	// HotspotPatterns comes from the project row, defaulting to
	// DefaultHotspotPatterns().
	HotspotPatterns []string
}

// PathConflictSeverity scores a confirmed overlap and returns the names of the
// adjusters that matched, for the conflict's audit trail.
//
// A name is listed when its condition held, even where clamping made the change
// a no-op. The trail answers "which rules had an opinion about this pair",
// which is the question asked when a rule's dismissal rate is being reviewed.
func PathConflictSeverity(in SeverityInput) (Severity, []string) {
	// read x read is not a conflict and cannot become one. Returning here
	// rather than scoring it none is the difference between "no conflict" and
	// "a conflict recorded at severity none" - the second one still costs a
	// row, an event and a line on the board.
	if in.A.Mode == ModeRead && in.B.Mode == ModeRead {
		return SeverityNone, nil
	}

	sev := pathBaseSeverity(in.A, in.B)
	var fired []string

	// Two agents driven by the same person are coordinated by that person.
	if in.A.MemberUUID != "" && in.A.MemberUUID == in.B.MemberUUID {
		sev = pathClampSeverity(sev - 1)
		fired = append(fired, PathAdjusterSameMember)
	}
	// Same branch means git will show them the collision at merge time, which
	// is late but is not silent.
	if in.A.Branch != "" && in.A.Branch == in.B.Branch {
		sev = pathClampSeverity(sev - 1)
		fired = append(fired, PathAdjusterSameBranch)
	}
	if in.A.Path.IsBroad() || in.B.Path.IsBroad() {
		if sev > SeverityLow {
			sev = SeverityLow
		}
		fired = append(fired, PathAdjusterBroadClaim)
	}
	// Hotspots are checked against both sides: one agent claiming go.mod
	// exactly and the other claiming the repo root is still a lockfile
	// collision.
	if pathMatchesAny(in.HotspotPatterns, in.A.Path.PatternNorm) ||
		pathMatchesAny(in.HotspotPatterns, in.B.Path.PatternNorm) {
		sev = pathClampSeverity(sev + 1)
		fired = append(fired, PathAdjusterHotspot)
	}

	return sev, fired
}

func pathBaseSeverity(a, b PathClaimSide) Severity {
	samePath := a.Path.PatternNorm == b.Path.PatternNorm

	switch {
	case a.Mode == ModeStructural && b.Mode == ModeStructural && samePath:
		return SeverityCritical
	case a.Mode == ModeStructural || b.Mode == ModeStructural:
		// Against anything, including a read: a rename breaks the reader and
		// the reader has no way to notice.
		return SeverityHigh
	case a.Mode == ModeWrite && b.Mode == ModeWrite:
		if samePath && a.Path.Kind == PathKindExact && b.Path.Kind == PathKindExact {
			return SeverityCritical
		}
		return SeverityHigh
	default:
		// One write, one read. read x read already returned.
		return SeverityMedium
	}
}

// pathClampSeverity bounds an adjusted severity to [low, critical].
//
// The floor is low, not none. SeverityNone means "this is not a conflict and
// cannot become one", which is true of exactly one thing: read x read, handled
// by an early return before any adjuster runs. An adjuster only ever says a
// real collision matters less - never that it stopped existing - so letting
// two -1 adjusters stack down to none would silently drop a row the board is
// supposed to show. Recording floor is low; the notify floor is separate and
// higher, which is what actually keeps quiet conflicts quiet.
func pathClampSeverity(s Severity) Severity {
	if s < SeverityLow {
		return SeverityLow
	}
	if s > SeverityCritical {
		return SeverityCritical
	}
	return s
}

func pathMatchesAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if pathGlobMatch(p, name) {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────
// Dedupe
// ─────────────────────────────────────────────

// PathDedupeKey is the stable identity of one path-overlap conflict, unique on
// (team_uuid, dedupe_key) so re-detection bumps a counter instead of spamming
// the board with the same pair every time either agent calls a tool.
//
// The claim uuids are sorted before hashing: whichever agent declares second is
// the one that runs detection, so the same pair arrives in either order and an
// order-sensitive key would mint two rows for one collision.
//
// "|" is not escaped. It does not need to be: the two uuids cannot contain one,
// and the overlap path is last, so there is no following field for it to bleed
// into. Pass anything but a uuid for the first two arguments and that stops
// being true.
func PathDedupeKey(claimAUUID, claimBUUID, overlapPath string) string {
	lo, hi := claimAUUID, claimBUUID
	if hi < lo {
		lo, hi = hi, lo
	}
	sum := sha256.Sum256([]byte("path_overlap|" + lo + "|" + hi + "|" + overlapPath))
	return hex.EncodeToString(sum[:])
}
