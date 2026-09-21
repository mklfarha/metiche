package coordination

// Duplicate work: two live plans that may be the same change (docs/DUPLICATES.md).
//
// In a hackathon there are no issue numbers. Plans are three to six informal
// words ("add login page", "build the login screen"), so the wording of two
// summaries is the whole signal there. This file turns a summary into concept
// keys and decides which pairs of live plans are worth putting to the later
// declarer's own model. It never decides that two plans ARE the same work: a
// candidate costs one judgement, and the judge answers it.
//
// Pure and deterministic: no database, no clock, no network. The same inputs
// always give the same keys, the same score and the same order.

import (
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ─────────────────────────────────────────────
// The material-change rule for intent.wording_revision (§2.2)
// ─────────────────────────────────────────────

// wordingKeyTokens is how many summary tokens the wording key compares.
const wordingKeyTokens = 64

// WordingKey is what a summary and an issue id mean for duplicate pairing:
// the summary's normalized tokens and the trimmed, lowercased external_ref.
// Case, punctuation, folded plurals and stopwords do not change it.
func WordingKey(summary, externalRef string) string {
	return strings.Join(Tokenize(summary, wordingKeyTokens), " ") + "\x00" + strings.ToLower(strings.TrimSpace(externalRef))
}

// WordingChanged reports a material wording change: the one thing that bumps
// intent.wording_revision and so earns a duplicate pair one more look.
func WordingChanged(oldSummary, oldRef, newSummary, newRef string) bool {
	return WordingKey(oldSummary, oldRef) != WordingKey(newSummary, newRef)
}

// ─────────────────────────────────────────────
// Concept keys (§4.2)
// ─────────────────────────────────────────────

// DuplicateKeyRunes is the comparison key length: the first five runes of the
// canonical word, so summary/summarize and deploy/deployment meet.
const DuplicateKeyRunes = 5

// DuplicateFrequencyMinScanned is the smallest scan, caller included, on
// which the frequency filter runs. A hackathon project with a handful of live
// plans never filters, so two plans both saying "login" are never hidden.
const DuplicateFrequencyMinScanned = 8

// DuplicateFrequencyMinCount is the floor of the frequency bar: a key in at
// least max(DuplicateFrequencyMinCount, ⌈n/3⌉) of n summaries is dropped.
const DuplicateFrequencyMinCount = 4

// duplicateTermTokens is how many tokens of one summary are read
// (duplicateMaxTokens in docs/DUPLICATES.md §3.0).
const duplicateTermTokens = 32

// The rule thresholds (§4.2). The ratio is shared keys over the smaller count
// of non-vague keys.
const (
	duplicateWordsMinShared = 2
	duplicateWordsMinRatio  = 0.6
	duplicateLongMinCore    = 3
	duplicateLongMinRatio   = 0.4
	duplicateSingleMaxOther = 2
	duplicatePathsMinRatio  = 0.5
)

// Layer is the part of the stack a layer word names. Two plans on disjoint
// layers are a producer and a consumer (contracts' business), not duplicates.
type Layer string

const (
	LayerUI   Layer = "ui"
	LayerAPI  Layer = "api"
	LayerData Layer = "data"
)

// DuplicateTerms is one summary as the duplicate scorer reads it.
type DuplicateTerms struct {
	Keys   []string          // comparison keys, first-seen order, de-duplicated
	Words  map[string]string // key -> the first original word the summary used
	Vague  map[string]bool   // keys that never count as shared
	Layers map[string]Layer  // key -> layer, for layer words only
}

// duplicatePhraseJoins glue the phrases that Tokenize would otherwise split
// into a stopword and a verb ("sign in" is sign + in). Matched case-
// insensitively on the original text, across a space, hyphen or underscore,
// or none ("SignIn"), and only on whole words ("signing in" is untouched).
var duplicatePhraseJoins = []struct {
	re     *regexp.Regexp
	joined string
}{
	{regexp.MustCompile(`(?i)\bsign[\s_-]*in\b`), "signin"},
	{regexp.MustCompile(`(?i)\bsign[\s_-]*up\b`), "signup"},
	{regexp.MustCompile(`(?i)\bsign[\s_-]*out\b`), "signout"},
	{regexp.MustCompile(`(?i)\blog[\s_-]*in\b`), "login"},
	{regexp.MustCompile(`(?i)\blog[\s_-]*out\b`), "logout"},
	{regexp.MustCompile(`(?i)\bset[\s_-]*up\b`), "setup"},
	{regexp.MustCompile(`(?i)\bcheck[\s_-]*out\b`), "checkout"},
	// "actions" and "workflow" alone are too common to fold; with github in
	// front they are CI.
	{regexp.MustCompile(`(?i)\b(github|gh)[\s_-]*(actions?|workflows?)\b`), "githubci"},
	{regexp.MustCompile(`(?i)\bsocket[\s._-]*io\b`), "socketio"},
	{regexp.MustCompile(`(?i)\breal[\s_-]*time\b`), "realtime"},
}

func duplicateJoinPhrases(text string) string {
	for _, p := range duplicatePhraseJoins {
		text = p.re.ReplaceAllString(text, " "+p.joined+" ")
	}
	return text
}

// duplicateStoplist are task verbs and filler that name no deliverable. They
// are dropped after Tokenize, which already drops add, new, make and use.
var duplicateStoplist = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		build create implement write wire hook fix update handle support improve
		refactor clean cleanup setup set get start finish do try quick initial
		first basic simple
		fixe fixes fixed fixing
	`) {
		m[w] = true
	}
	return m
}()

// duplicateSynonymRows fold words that name one thing to one canonical word.
// One row per line: the tuning surface, extended by adding §7.1 vectors first.
var duplicateSynonymRows = []struct {
	canonical string
	layer     Layer
	words     []string
}{
	{"page", LayerUI, []string{"page", "screen", "view", "ui", "frontend", "form", "modal", "dialog"}},
	{"endpoint", LayerAPI, []string{"endpoint", "route", "api", "apis", "handler", "backend", "server", "controller"}},
	{"schema", LayerData, []string{"schema", "table", "migration", "database", "db", "sql"}},
	{"login", "", []string{"login", "signin", "logon"}},
	{"signup", "", []string{"signup", "register", "registration"}},
	{"logout", "", []string{"logout", "signout"}},
	{"auth", "", []string{"auth", "authentication", "authn"}},
	{"image", "", []string{"image", "img", "picture", "photo"}},
	{"button", "", []string{"button", "btn"}},
	{"reset", "", []string{"reset", "forgot", "recover", "recovery"}},
	{"ci", "", []string{"ci", "cd", "cicd", "pipeline", "githubci"}},
	{"theme", "", []string{"theme", "mode", "darkmode"}},
	{"toggle", "", []string{"toggle", "switcher"}},
	{"realtime", "", []string{"realtime", "websocket", "socket", "socketio", "live", "pubsub"}},
}

type duplicateSynonym struct {
	canonical string
	layer     Layer
}

var duplicateSynonyms = func() map[string]duplicateSynonym {
	m := map[string]duplicateSynonym{}
	for _, row := range duplicateSynonymRows {
		for _, w := range row.words {
			m[w] = duplicateSynonym{canonical: row.canonical, layer: row.layer}
		}
	}
	return m
}()

// duplicateVagueKeys never count as shared and block the single-concept rule.
// Matched on the 5-rune key after folding: "styles" is vague, "styling" is not.
var duplicateVagueKeys = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		test bug issue error feature stuff thing code work task part flow demo
		mvp app user style lint typo doc
	`) {
		m[duplicateKey(w)] = true
	}
	return m
}()

// duplicateKey is the first DuplicateKeyRunes runes of a canonical word.
func duplicateKey(word string) string {
	if utf8.RuneCountInString(word) <= DuplicateKeyRunes {
		return word
	}
	return string([]rune(word)[:DuplicateKeyRunes])
}

// DuplicateTermsOf builds a summary's concept keys (§4.2): phrases joined,
// Tokenize, the duplicate stoplist and the project key's tokens dropped,
// synonyms folded, each word cut to its 5-rune key, de-duplicated in
// first-seen order.
func DuplicateTermsOf(summary, projectKey string) DuplicateTerms {
	t := DuplicateTerms{
		Words:  map[string]string{},
		Vague:  map[string]bool{},
		Layers: map[string]Layer{},
	}
	project := map[string]bool{}
	for _, tok := range Tokenize(duplicateJoinPhrases(projectKey), duplicateTermTokens) {
		project[tok] = true
	}
	for _, tok := range Tokenize(duplicateJoinPhrases(summary), duplicateTermTokens) {
		if duplicateStoplist[tok] || project[tok] {
			continue
		}
		canonical, layer := tok, Layer("")
		if syn, ok := duplicateSynonyms[tok]; ok {
			canonical, layer = syn.canonical, syn.layer
		}
		key := duplicateKey(canonical)
		if _, seen := t.Words[key]; seen {
			continue
		}
		t.Keys = append(t.Keys, key)
		t.Words[key] = tok
		if duplicateVagueKeys[key] {
			t.Vague[key] = true
		}
		if layer != "" {
			t.Layers[key] = layer
		}
	}
	return t
}

// FrequentTerms returns the keys to drop from every comparison in one scan
// (caller included in all); nil below DuplicateFrequencyMinScanned.
func FrequentTerms(all []DuplicateTerms) map[string]bool {
	n := len(all)
	if n < DuplicateFrequencyMinScanned {
		return nil
	}
	bar := (n + 2) / 3 // ⌈n/3⌉
	if bar < DuplicateFrequencyMinCount {
		bar = DuplicateFrequencyMinCount
	}
	counts := map[string]int{}
	for _, t := range all {
		seen := map[string]bool{}
		for _, k := range t.Keys {
			if !seen[k] {
				seen[k] = true
				counts[k]++
			}
		}
	}
	drop := map[string]bool{}
	for k, c := range counts {
		if c >= bar {
			drop[k] = true
		}
	}
	return drop
}

// ─────────────────────────────────────────────
// Scoring (§4.2)
// ─────────────────────────────────────────────

// DuplicateScore is why a pair of summaries is, or is not, a candidate.
type DuplicateScore struct {
	Candidate bool
	Rule      string   // "words" | "words_long" | "words_single" | "words_layer" | "words_and_paths" | ""
	Shared    []string // non-vague shared keys, in mine's order
	Core      int      // shared keys that are not layer words
	Ratio     float64  // len(Shared) / smaller non-vague key count
}

// duplicateLayerGuard is true when both plans name a layer and their layer
// sets are disjoint: a page against an endpoint, a page against a schema.
//
// It reads each plan's layers before the frequency filter: a layer is what a
// plan is, not a similarity signal, and a project where "page" is frequent
// must still not pair "login page" with "login endpoint".
func duplicateLayerGuard(mine, theirs DuplicateTerms) bool {
	if len(mine.Layers) == 0 || len(theirs.Layers) == 0 {
		return false
	}
	have := map[Layer]bool{}
	for _, l := range mine.Layers {
		have[l] = true
	}
	for _, l := range theirs.Layers {
		if have[l] {
			return false
		}
	}
	return true
}

func duplicateKeep(keys []string, drop map[string]bool) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if !drop[k] {
			out = append(out, k)
		}
	}
	return out
}

func duplicateNonVague(keys []string, vague map[string]bool) int {
	n := 0
	for _, k := range keys {
		if !vague[k] {
			n++
		}
	}
	return n
}

// duplicateIsOnly reports whether keys is exactly the one key k.
func duplicateIsOnly(keys []string, k string) bool {
	return len(keys) == 1 && keys[0] == k
}

// duplicateSameNonVague reports whether two key lists hold the same non-vague
// keys as sets.
func duplicateSameNonVague(a []string, aVague map[string]bool, b []string, bVague map[string]bool) bool {
	setOf := func(keys []string, vague map[string]bool) map[string]bool {
		out := map[string]bool{}
		for _, k := range keys {
			if !vague[k] {
				out[k] = true
			}
		}
		return out
	}
	sa, sb := setOf(a, aVague), setOf(b, bVague)
	if len(sa) != len(sb) {
		return false
	}
	for k := range sa {
		if !sb[k] {
			return false
		}
	}
	return true
}

// ScoreDuplicate applies §4.2's rules to two summaries' terms, after dropping
// the scan's frequent keys. The first rule that fits names the result.
func ScoreDuplicate(mine, theirs DuplicateTerms, drop map[string]bool, pathsOverlap bool) DuplicateScore {
	m := duplicateKeep(mine.Keys, drop)
	t := duplicateKeep(theirs.Keys, drop)
	inT := make(map[string]bool, len(t))
	for _, k := range t {
		inT[k] = true
	}

	var score DuplicateScore
	core := ""
	for _, k := range m {
		if !inT[k] || mine.Vague[k] || theirs.Vague[k] {
			continue
		}
		score.Shared = append(score.Shared, k)
		if mine.Layers[k] == "" && theirs.Layers[k] == "" {
			score.Core++
			core = k
		}
	}
	smaller := min(duplicateNonVague(m, mine.Vague), duplicateNonVague(t, theirs.Vague))
	if smaller > 0 {
		score.Ratio = float64(len(score.Shared)) / float64(smaller)
	}

	if duplicateLayerGuard(mine, theirs) {
		return score
	}
	if smaller == 0 {
		return score
	}

	shared, c := len(score.Shared), score.Core
	switch {
	case c >= 1 && shared >= duplicateWordsMinShared && score.Ratio >= duplicateWordsMinRatio:
		score.Rule = "words"
	case c >= duplicateLongMinCore && score.Ratio >= duplicateLongMinRatio:
		score.Rule = "words_long"
	case c == 1 && ((duplicateIsOnly(m, core) && duplicateNonVague(t, theirs.Vague) <= duplicateSingleMaxOther) ||
		(duplicateIsOnly(t, core) && duplicateNonVague(m, mine.Vague) <= duplicateSingleMaxOther)):
		score.Rule = "words_single"
	// Layer only, but the same thing: both plans reduce to the identical
	// non-vague key set ("set up the database" and "database schema and
	// migrations" are both {schem}). A set that merely shares a layer word
	// ("login page", "signup page") never fires.
	case c == 0 && shared >= 1 && duplicateSameNonVague(m, mine.Vague, t, theirs.Vague):
		score.Rule = "words_layer"
	case pathsOverlap && c >= 1 && score.Ratio >= duplicatePathsMinRatio:
		score.Rule = "words_and_paths"
	}
	score.Candidate = score.Rule != ""
	return score
}

// ─────────────────────────────────────────────
// Ranking
// ─────────────────────────────────────────────

// DuplicateSignal is why two plans were paired. The numeric order is the
// ranking order.
type DuplicateSignal int

const (
	DupSignalWords         DuplicateSignal = 1
	DupSignalWordsAndPaths DuplicateSignal = 2
	DupSignalSameIssue     DuplicateSignal = 3
)

// String is the review block's why: "words", "words_and_paths" or "same_issue".
func (s DuplicateSignal) String() string {
	switch s {
	case DupSignalWords:
		return "words"
	case DupSignalWordsAndPaths:
		return "words_and_paths"
	case DupSignalSameIssue:
		return "same_issue"
	}
	return "unknown"
}

// DuplicateCandidate is one other plan worth a duplicate judgement.
type DuplicateCandidate struct {
	OtherIntentUUID string
	OtherIntentKey  string
	Signal          DuplicateSignal
	Core            int
	Shared          int
}

// RankDuplicateCandidates orders candidates same_issue > words_and_paths >
// words, then more core shared keys, then more shared keys, then intent key
// ascending (uuid as the last tie-break), and keeps at most max. The input is
// not modified.
func RankDuplicateCandidates(in []DuplicateCandidate, max int) []DuplicateCandidate {
	out := append([]DuplicateCandidate(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Signal != b.Signal {
			return a.Signal > b.Signal
		}
		if a.Core != b.Core {
			return a.Core > b.Core
		}
		if a.Shared != b.Shared {
			return a.Shared > b.Shared
		}
		if a.OtherIntentKey != b.OtherIntentKey {
			return a.OtherIntentKey < b.OtherIntentKey
		}
		return a.OtherIntentUUID < b.OtherIntentUUID
	})
	if max >= 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

// ─────────────────────────────────────────────
// Severity (§4.4)
// ─────────────────────────────────────────────

// DuplicateSeverityInput is one duplicate verdict as §4.4 reads it.
type DuplicateSeverityInput struct {
	Verdict    string // "conflict" | "unsure"
	Confidence float64
	Requested  Severity // SeverityLow..SeverityHigh; zero means medium
	SameIssue  bool
	Demoted    bool // the rule is in team_settings.demoted_rules
}

// DuplicateSeverity is §4.4's table. An agent verdict never reaches critical.
//
//  1. unsure → low.
//  2. conflict below 0.70 → low.
//  3. the requested severity, default medium, clamped to low..high;
//  4. a shared external_ref → floor medium;
//  5. no same-member softening: one person's two live agents duplicating
//     each other is solo fan-out, exactly who needs telling.
//
// A demoted rule records low. Any other verdict (no_conflict) earns no
// conflict at all: SeverityNone.
func DuplicateSeverity(in DuplicateSeverityInput) Severity {
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
	if in.SameIssue && sev < SeverityMedium {
		sev = SeverityMedium
	}
	if in.Demoted {
		return SeverityLow
	}
	return sev
}

// DuplicateNoticeGrace is how long a duplicate must stand before the
// incumbent is told: EscalationBudget(cadence) / 5, so hackathon 2m, sprint
// 6m, steady 24m, anything else 6m.
func DuplicateNoticeGrace(cadence string) time.Duration {
	return EscalationBudget(cadence) / 5
}
