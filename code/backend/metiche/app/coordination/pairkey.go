package coordination

// The pair key: the anti-re-judging ledger's identity.
//
// Semantic judging costs an LLM call in somebody's agent loop, so a pair must
// be judged once and then never surfaced again — unless one of its subjects
// materially changes, which mints a new key and earns exactly one more look.
// Three properties carry that whole design and each is proven by test:
// symmetric (the pair is unordered — whoever declares second must not mint a
// second key for the same two things), revision-scoped (a material edit is a
// new pair), and kind-scoped (the same two subjects judged for duplicate work
// and for contract mismatch are two independent questions).

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// PairSubject is one side of a judgeable pair.
type PairSubject struct {
	// Kind is the entity kind: "intent", "decision", "contract_assertion", …
	Kind string
	UUID string
	// Revision is the subject's material-change counter. Cosmetic edits must
	// not bump it, or every typo fix costs the team another round of judging.
	Revision int
}

// pairToken renders one subject.
//
// Case and surrounding space are normalized because a uuid that made one round
// trip through a formatter that upper-cased it would otherwise mint a second
// key for the same pair — and the unique index on (team_uuid, pair_key) would
// happily accept it, which is exactly the duplicated judging this mechanism
// exists to prevent.
func (s PairSubject) pairToken() string {
	kind := strings.ToLower(strings.TrimSpace(s.Kind))
	id := strings.ToLower(strings.TrimSpace(s.UUID))
	return kind + ":" + id + "@" + strconv.Itoa(s.Revision)
}

// PairKey is sha256( kind | sorted subject tokens ), hex.
//
// Server-computed, always: an agent that computed its own would be free to
// re-mint a key and re-ask a question the team already answered.
func PairKey(kind string, a, b PairSubject) string {
	tokens := []string{a.pairToken(), b.pairToken()}
	// Sorting is what makes the key symmetric. Without it the later declarer
	// mints a different key for the same two subjects and both agents burn
	// tokens judging one pair.
	sort.Strings(tokens)

	h := sha256.New()
	h.Write([]byte(strings.ToLower(strings.TrimSpace(kind))))
	h.Write([]byte("|"))
	h.Write([]byte(strings.Join(tokens, "|")))
	return hex.EncodeToString(h.Sum(nil))
}
