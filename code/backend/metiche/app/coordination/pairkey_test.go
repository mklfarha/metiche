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
