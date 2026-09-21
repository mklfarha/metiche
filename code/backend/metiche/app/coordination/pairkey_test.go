package coordination

import "testing"

func TestPairKeySymmetric(t *testing.T) {
	a := PairSubject{Kind: "intent", UUID: "11111111-1111-4111-8111-111111111111", Revision: 3}
	b := PairSubject{Kind: "decision", UUID: "22222222-2222-4222-8222-222222222222", Revision: 1}

	ab := PairKey("intent_decision", a, b)
	ba := PairKey("intent_decision", b, a)
	if ab != ba {
		t.Fatalf("PairKey is not symmetric:\n a,b = %s\n b,a = %s", ab, ba)
	}
	if len(ab) != 64 {
		t.Errorf("PairKey length = %d, want 64 hex chars", len(ab))
	}
	// Repeat calls are stable; the ledger's unique index depends on it.
	if again := PairKey("intent_decision", a, b); again != ab {
		t.Errorf("PairKey is not stable: %s then %s", ab, again)
	}
}

func TestPairKeyRevisionScoped(t *testing.T) {
	a := PairSubject{Kind: "intent", UUID: "11111111-1111-4111-8111-111111111111", Revision: 3}
	b := PairSubject{Kind: "intent", UUID: "22222222-2222-4222-8222-222222222222", Revision: 1}
	base := PairKey("duplicate_work", a, b)

	bumpedA := a
	bumpedA.Revision++
	if got := PairKey("duplicate_work", bumpedA, b); got == base {
		t.Error("bumping a.Revision must mint a new key, or a materially edited intent is never re-judged")
	}

	bumpedB := b
	bumpedB.Revision = 9
	if got := PairKey("duplicate_work", a, bumpedB); got == base {
		t.Error("bumping b.Revision must mint a new key")
	}

	// Symmetry survives a revision bump.
	if PairKey("duplicate_work", bumpedA, b) != PairKey("duplicate_work", b, bumpedA) {
		t.Error("PairKey must stay symmetric after a revision bump")
	}

	// And a cosmetic edit - which by definition does not bump revision -
	// produces the same key, so the pair stays judged.
	if got := PairKey("duplicate_work", a, b); got != base {
		t.Error("an unchanged revision must reproduce the same key")
	}
}

func TestPairKeyKindScoped(t *testing.T) {
	a := PairSubject{Kind: "contract_assertion", UUID: "11111111-1111-4111-8111-111111111111", Revision: 2}
	b := PairSubject{Kind: "contract_assertion", UUID: "22222222-2222-4222-8222-222222222222", Revision: 2}

	first := PairKey("contract_mismatch", a, b)
	second := PairKey("duplicate_work", a, b)
	if first == second {
		t.Fatal("the same two subjects under a different pair kind must produce different keys")
	}
	if empty := PairKey("", a, b); empty == first || empty == second {
		t.Error("an empty kind must not collide with a named one")
	}
}

func TestPairKeySubjectKindScoped(t *testing.T) {
	base := PairKey("x",
		PairSubject{Kind: "intent", UUID: "aaaa", Revision: 1},
		PairSubject{Kind: "decision", UUID: "bbbb", Revision: 1},
	)
	swapped := PairKey("x",
		PairSubject{Kind: "decision", UUID: "aaaa", Revision: 1},
		PairSubject{Kind: "intent", UUID: "bbbb", Revision: 1},
	)
	if base == swapped {
		t.Error("subject kind is part of the identity; two different entity kinds must not collide")
	}
}

// A decision and the plan judged against it: the pair key record_decision and
// the reviewer mint (docs/DECISIONS.md §3.0).
func TestPairKeyDecisionAndIntent(t *testing.T) {
	decision := PairSubject{Kind: "decision", UUID: "33333333-3333-4333-8333-333333333333", Revision: 1}
	intent := PairSubject{Kind: "intent", UUID: "44444444-4444-4444-8444-444444444444", Revision: 1}

	key := PairKey("decision_contradiction", decision, intent)
	if key != PairKey("decision_contradiction", intent, decision) {
		t.Fatal("a decision and intent pair must be symmetric")
	}
	bumped := intent
	bumped.Revision = 2
	if PairKey("decision_contradiction", decision, bumped) == key {
		t.Error("an intent revision bump must mint a new pair: a changed plan earns one more look")
	}
	revised := decision
	revised.Revision = 2
	if PairKey("decision_contradiction", revised, intent) == key {
		t.Error("a decision revision bump must mint a new pair")
	}
	if PairKey("duplicate_work", decision, intent) == key {
		t.Error("the same subjects under duplicate_work are a different question and a different key")
	}
}

// A uuid that made a round trip through something that upper-cased it must not
// mint a second key for a pair the team already judged.
func TestPairKeyNormalizesCasing(t *testing.T) {
	lower := PairSubject{Kind: "intent", UUID: "abcd-ef01", Revision: 1}
	upper := PairSubject{Kind: "Intent", UUID: " ABCD-EF01 ", Revision: 1}
	other := PairSubject{Kind: "decision", UUID: "22222222", Revision: 4}

	if PairKey("contract_mismatch", lower, other) != PairKey("CONTRACT_MISMATCH", upper, other) {
		t.Error("case and surrounding space must not change the pair key")
	}
}

// A duplicate_work pair: two intents at their wording_revision
// (docs/DUPLICATES.md §3.0, §7.1). Subject b is always the judge's plan in
// storage, but the key must not care which side surfaced the pair.
func TestPairKeyDuplicateWork(t *testing.T) {
	other := PairSubject{Kind: "intent", UUID: "55555555-5555-4555-8555-555555555555", Revision: 1}
	judge := PairSubject{Kind: "intent", UUID: "66666666-6666-4666-8666-666666666666", Revision: 1}

	key := PairKey("duplicate_work", other, judge)
	if key != PairKey("duplicate_work", judge, other) {
		t.Fatal("a duplicate pair must be symmetric: whichever plan surfaced it, one row")
	}

	// A material rewording bumps wording_revision and earns one more look,
	// on either side.
	reworded := judge
	reworded.Revision = 2
	if PairKey("duplicate_work", other, reworded) == key {
		t.Error("a wording_revision bump on the judge's plan must mint a new key")
	}
	rewordedOther := other
	rewordedOther.Revision = 2
	if PairKey("duplicate_work", rewordedOther, judge) == key {
		t.Error("a wording_revision bump on the other plan must mint a new key")
	}
	if PairKey("duplicate_work", rewordedOther, judge) == PairKey("duplicate_work", other, reworded) {
		t.Error("bumping a and bumping b are different pairs")
	}

	// The same two uuids judged for a decision are a different question.
	if PairKey("decision_contradiction", other, judge) == key {
		t.Error("duplicate_work and decision_contradiction over the same uuids must differ")
	}
}
