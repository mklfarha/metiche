// Package wording holds every cadence-dependent string on the board, in one
// map, so the register can be tuned in one place instead of being rediscovered
// inside five templates.
//
// Why cadence changes the words at all: the finding is identical, the right
// response is not. "Worth assigning at standup" is good advice on a steady
// project and actively harmful at a hackathon, where the next planning meeting
// is never. Copy that points at a ceremony which will not happen is worse than
// no copy, because it reads as an answer.
//
// Rules that hold in every register, and are the reason these are phrases and
// not templates full of hedging:
//
//   - name who is affected and what to do; never just state the fact;
//   - no "consider" at hackathon pace — it is an instruction or it is noise;
//   - the facts (names, paths, counts, elapsed time) are substituted in, never
//     invented here.
package wording

import (
	"fmt"
	"strings"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// Kind names one message the board needs in three registers.
type Kind string

// The messages.
const (
	// KindUnclaimed: a consumer with no producer, still inside its grace period.
	KindUnclaimed Kind = "contract_unclaimed"
	// KindUnclaimedOverdue: the same, past the cadence's threshold.
	KindUnclaimedOverdue Kind = "contract_unclaimed.overdue"
	KindPathOverlap      Kind = "path_overlap"
	KindContractMismatch Kind = "contract_mismatch"
	KindDecision         Kind = "decision_contradiction"
	KindDuplicate        Kind = "duplicate_work"
	// KindCadenceHint explains what the current cadence is doing to the board.
	KindCadenceHint Kind = "cadence.hint"
	// KindCalm is the line shown when nothing is wrong.
	KindCalm Kind = "calm"
)

type key struct {
	cadence model.Cadence
	kind    Kind
}

// Placeholders substituted into the phrases below:
//
//	{who}       the affected people, already joined into prose
//	{waiting}   what is waiting on it, e.g. "three components"
//	{what}      the artifact — a contract key, a path, a decision key
//	{elapsed}   how long it has been like this, e.g. "12m"
//	{budget}    the cadence's threshold, e.g. "5m"
//	{holder}    the session that was there first
//	{other}     the session that arrived into it
var deck = map[key]string{
	// --- a consumer with no producer -----------------------------------
	{model.CadenceHackathon, KindUnclaimed}: "Nobody is building {what}. {who} is coding against it. Somebody take it now.",
	{model.CadenceSprint, KindUnclaimed}:    "{what} has a consumer and no producer. {who} is building against it — pick it up, or drop what depends on it.",
	{model.CadenceSteady, KindUnclaimed}:    "{what} is consumed by {who} and produced by nobody. Worth an owner before it reaches integration.",

	{model.CadenceHackathon, KindUnclaimedOverdue}: "{elapsed} and still nobody is building {what}. {who} has {waiting} waiting on it. Take it or cut it — now, not at integration.",
	{model.CadenceSprint, KindUnclaimedOverdue}:    "{what} has been unclaimed for {elapsed}, past the {budget} this project allows. {who} has {waiting} depending on it. Pick it up or drop them.",
	{model.CadenceSteady, KindUnclaimedOverdue}:    "{what} has been unclaimed for {elapsed}. {who} has {waiting} depending on it. Worth assigning at the next standup.",

	// --- two write claims on the same paths -----------------------------
	{model.CadenceHackathon, KindPathOverlap}: "{holder} has {what} open. {other}: say what you need in it and let {holder} make the edit — do not take the file.",
	{model.CadenceSprint, KindPathOverlap}:    "{holder} holds {what}. {other} wants the same file — agree who owns it now, or {other} rebases onto {holder}'s branch.",
	{model.CadenceSteady, KindPathOverlap}:    "{holder} and {other} both hold {what}. Sequence the two changes, or split the file before either lands.",

	// --- producer and consumer disagree ---------------------------------
	{model.CadenceHackathon, KindContractMismatch}: "{who} disagree about {what}. Fix the producer now — {other} is already rendering the response.",
	{model.CadenceSprint, KindContractMismatch}:    "{who} disagree about the shape of {what}. Settle it on the producer side and re-publish; the consumer converges on the next call.",
	{model.CadenceSteady, KindContractMismatch}:    "{who} disagree about the shape of {what}. Resolve it before either side merges.",

	// --- an intent that contradicts a recorded decision -----------------
	{model.CadenceHackathon, KindDecision}: "{what} says otherwise. {who}: follow it, or change the decision on purpose — do not route around it.",
	{model.CadenceSprint, KindDecision}:    "{who} is about to contradict {what}. Read it first; if the team has genuinely changed its mind, supersede the decision rather than working around it.",
	{model.CadenceSteady, KindDecision}:    "{who} is about to contradict {what}. Supersede the decision deliberately or follow it.",

	// --- two people building the same thing ------------------------------
	{model.CadenceHackathon, KindDuplicate}: "{who} are building the same thing. One of you stop, right now.",
	{model.CadenceSprint, KindDuplicate}:    "{who} are building the same thing. Decide who keeps it before either gets further in.",
	{model.CadenceSteady, KindDuplicate}:    "{who} appear to be building the same thing. Worth reconciling the two intents.",

	// --- the cadence indicator's own explanation --------------------------
	{model.CadenceHackathon, KindCadenceHint}: "Hackathon: nothing waits. A contract with no producer is alarming after 5m.",
	{model.CadenceSprint, KindCadenceHint}:    "Sprint: the default. A contract with no producer is alarming after 2h.",
	{model.CadenceSteady, KindCadenceHint}:    "Steady: a ceremony is a legitimate answer here. A contract with no producer is alarming after a day.",

	// --- nothing is wrong ---------------------------------------------
	{model.CadenceHackathon, KindCalm}: "Nothing colliding. Keep going.",
	{model.CadenceSprint, KindCalm}:    "Nothing colliding.",
	{model.CadenceSteady, KindCalm}:    "Nothing colliding.",
}

// Phrase renders one message. Unknown combinations return "" so the caller can
// fall back to whatever the backend supplied, rather than the board inventing
// prose it cannot stand behind.
func Phrase(c model.Cadence, k Kind, vars map[string]string) string {
	if !c.Valid() {
		c = model.CadenceSprint
	}
	tmpl, ok := deck[key{c, k}]
	if !ok {
		return ""
	}
	pairs := make([]string, 0, len(vars)*2)
	for name, value := range vars {
		pairs = append(pairs, "{"+name+"}", value)
	}
	out := strings.NewReplacer(pairs...).Replace(tmpl)
	// Any placeholder we were not given makes the sentence wrong rather than
	// merely incomplete, so the phrase is discarded instead of shipped with a
	// brace in it.
	if strings.ContainsAny(out, "{}") {
		return ""
	}
	return out
}

// Threshold is how long a finding may sit before the board calls it overdue.
// The unclaimed contract is the one this actually governs; the rest inherit it
// as a general sense of "how long is too long here".
func Threshold(c model.Cadence) time.Duration {
	switch c {
	case model.CadenceHackathon:
		return 5 * time.Minute
	case model.CadenceSteady:
		return 24 * time.Hour
	}
	return 2 * time.Hour
}

// Short renders a duration the way a board should: two characters where it can.
func Short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// Overdue reports whether something that started at `since` has outlived this
// cadence's patience.
func Overdue(c model.Cadence, now, since time.Time) bool {
	if since.IsZero() {
		return false
	}
	return now.Sub(since) > Threshold(c)
}

// Join renders a list of names the way a person would say it.
func Join(names []string) string {
	switch len(names) {
	case 0:
		return "somebody"
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// Count renders a small number as a word, which reads better inside a sentence
// than a digit does.
func Count(n int, noun string) string {
	words := []string{"no", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine"}
	plural := noun + "s"
	if n == 1 {
		plural = noun
	}
	if n >= 0 && n < len(words) {
		return words[n] + " " + plural
	}
	return fmt.Sprintf("%d %s", n, plural)
}
