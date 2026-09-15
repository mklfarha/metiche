package coordination

// Decisions: the pure half of record_decision, the reviewer and judgement
// severity (docs/DECISIONS.md §3, §4.2, §4.7, §9.1).
//
// Nothing here decides whether a plan contradicts a decision. The server never
// does: the plan's own agent judges every pair (PLAN.md "the caller's own LLM
// judges semantics"). What is here is everything around that verdict that is
// deterministic — the key, the content hash that tells a revision from a
// cosmetic edit, candidate ranking, the severity a verdict earns, and when an
// unsettled conflict is worth a person.
//
// Pure: no database, no clock (callers pass now), no network.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

// ─────────────────────────────────────────────
// Keys and content
// ─────────────────────────────────────────────

var (
	// ErrDecisionKeyEmpty is a key with nothing in it but space and '#'.
	ErrDecisionKeyEmpty = errors.New("decision key is empty")
	// ErrDecisionKeyInvalid is a key that is not 3-60 of [a-z0-9-] once
	// normalized.
	ErrDecisionKeyInvalid = errors.New("decision key must be 3-60 lowercase letters, digits and dashes")
)

var decisionKeyPattern = regexp.MustCompile(`^[a-z0-9-]{3,60}$`)

// NormalizeDecisionKey turns what an agent typed into the stored key.
//
// Trim, strip one leading '#', lowercase, spaces and underscores become '-',
// repeated dashes collapse, leading and trailing dashes go. The result must be
// 3-60 of [a-z0-9-], and is returned with its '#': "#Auth JWT_cookie" is
// "#auth-jwt-cookie".
func NormalizeDecisionKey(raw string) (string, error) {
	s := strings.TrimPrefix(strings.TrimSpace(raw), "#")
	if strings.TrimSpace(s) == "" {
		return "", ErrDecisionKeyEmpty
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '_' {
			return '-'
		}
		return unicode.ToLower(r)
	}, s)
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if !decisionKeyPattern.MatchString(s) {
		return "", ErrDecisionKeyInvalid
	}
	return "#" + s, nil
}

// DecisionContentHash is what separates a revision (a new pair for every plan
// already judged against the old wording) from a cosmetic edit. The scope is
// order-free and de-duplicated; team_wide is part of the content because it
// changes which plans the decision governs.
func DecisionContentHash(title, statement string, scopeNorm []string, teamWide bool) string {
	scope := append([]string(nil), scopeNorm...)
	sort.Strings(scope)
	uniq := scope[:0]
	for i, p := range scope {
		if i > 0 && p == scope[i-1] {
			continue
		}
		uniq = append(uniq, p)
	}
	wide := "0"
	if teamWide {
		wide = "1"
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		strings.TrimSpace(title), strings.TrimSpace(statement), strings.Join(uniq, "\n"), wide,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// ─────────────────────────────────────────────
// Candidates
// ─────────────────────────────────────────────

// CandidateSignal is why a decision and a plan were paired. The numeric order
// is the ranking order.
type CandidateSignal int

const (
	SignalWords      CandidateSignal = 1
	SignalAlwaysShow CandidateSignal = 2
	SignalScope      CandidateSignal = 3
)

// String is the review block's why: "scope", "always_show" or "words".
func (s CandidateSignal) String() string {
	switch s {
	case SignalScope:
		return "scope"
	case SignalAlwaysShow:
		return "always_show"
	case SignalWords:
		return "words"
	}
	return "unknown"
}

// DecisionCandidate is one (decision, plan) pair worth a judgement.
type DecisionCandidate struct {
	DecisionUUID string
	DecisionKey  string
	IntentUUID   string
	IntentKey    string
	Signal       CandidateSignal // the strongest signal for this pair
	SharedTokens int
	Why          string // the ReviewItem.why text
}

// RankDecisionCandidates orders candidates scope > always_show > words, then
// more shared tokens first, then intent key, then decision key, and keeps at
// most max. The input is not modified.
func RankDecisionCandidates(in []DecisionCandidate, max int) []DecisionCandidate {
	out := append([]DecisionCandidate(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Signal != b.Signal {
			return a.Signal > b.Signal
		}
		if a.SharedTokens != b.SharedTokens {
			return a.SharedTokens > b.SharedTokens
		}
		if a.IntentKey != b.IntentKey {
			return a.IntentKey < b.IntentKey
		}
		if a.DecisionKey != b.DecisionKey {
			return a.DecisionKey < b.DecisionKey
		}
		if a.IntentUUID != b.IntentUUID {
			return a.IntentUUID < b.IntentUUID
		}
		return a.DecisionUUID < b.DecisionUUID
	})
	if max >= 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

// ─────────────────────────────────────────────
// Severity
// ─────────────────────────────────────────────

// JudgeMinConfidence is PLAN.md's "model verdicts interrupt only at confidence
// ≥ 0.7". Inclusive.
const JudgeMinConfidence = 0.70

// JudgedSeverityInput is one verdict as §4.2 reads it.
type JudgedSeverityInput struct {
	Verdict    string // "conflict" | "unsure"
	Confidence float64
	Requested  Severity // SeverityLow..SeverityHigh; zero means medium
	SameMember bool
	AlwaysShow bool
}

// JudgedSeverity is §4.2's table. An agent verdict never reaches critical.
//
//  1. unsure → low.
//  2. conflict below 0.70 → low.
//  3. the requested severity, default medium, clamped to low..high;
//  4. the plan owner is the decider's own member → one step down, floor low;
//  5. an always-show decision → floor medium.
//
// Any other verdict earns no conflict at all (SeverityNone).
func JudgedSeverity(in JudgedSeverityInput) Severity {
	switch in.Verdict {
	case "unsure":
		return SeverityLow
	case "conflict":
	default:
		return SeverityNone
	}
	if in.Confidence < JudgeMinConfidence {
		return SeverityLow
	}
	sev := in.Requested
	if sev == SeverityNone {
		sev = SeverityMedium
	}
	if sev < SeverityLow {
		sev = SeverityLow
	}
	if sev > SeverityHigh {
		sev = SeverityHigh
	}
	if in.SameMember && sev > SeverityLow {
		sev--
	}
	if in.AlwaysShow && sev < SeverityMedium {
		sev = SeverityMedium
	}
	return sev
}

// ─────────────────────────────────────────────
// Asking a person
// ─────────────────────────────────────────────

// EscalationBudget is how long agents get to settle a decision conflict
// before a person is asked. cadence is enums.ProjectCadence.String().
func EscalationBudget(cadence string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(cadence)) {
	case "hackathon":
		return 10 * time.Minute
	case "steady":
		return 2 * time.Hour
	default:
		return 30 * time.Minute
	}
}

// ShouldEscalate is §4.7's rule: once per conflict, at or above the human
// floor, while the plan is still live, after a full budget since the last
// verdict or notice. A zero humanFloor is the default, high.
func ShouldEscalate(severity, humanFloor Severity, openSince, now time.Time, cadence string, intentLive, alreadyEscalated bool) bool {
	if alreadyEscalated || !intentLive {
		return false
	}
	if humanFloor == SeverityNone {
		humanFloor = SeverityHigh
	}
	if severity < humanFloor {
		return false
	}
	return now.Sub(openSince) >= EscalationBudget(cadence)
}
