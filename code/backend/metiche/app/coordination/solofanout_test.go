package coordination

import "testing"

// Solo fan-out: one person running several agents AT ONCE.
//
// The same-member adjuster used to fire on identity alone, on the reasoning
// that "two agents driven by the same person are coordinated by that person".
// That holds when the person is driving one agent at a time. It is exactly
// false when they fan out five in parallel -- which is the moment they have
// the least idea what each of their own agents is touching, and is a
// first-class way to use metiche rather than an edge case.
//
// The rule now keys on concurrency, not identity.

func sideFor(t *testing.T, member, branch, pattern string, mode ClaimMode, live bool) PathClaimSide {
	t.Helper()
	p, err := NormalizePath(pattern, nil, false)
	if err != nil {
		t.Fatalf("normalize %q: %v", pattern, err)
	}
	return PathClaimSide{Mode: mode, Path: p, MemberUUID: member, Branch: branch, LiveNow: live}
}

func TestSoloFanOutIsNotSoftened(t *testing.T) {
	const me = "member-1"

	// The shape that matters, and the one that used to go silent: one
	// person's two agents, same branch, both writing, overlapping globs.
	// Two -1s (same member, same branch) dropped this under the notify
	// floor, so nobody was told about either side of it.
	both := func(live bool) (Severity, []string) {
		return PathConflictSeverity(SeverityInput{
			A: sideFor(t, me, "main", "internal/auth/**", ModeWrite, live),
			B: sideFor(t, me, "main", "internal/auth/token.go", ModeWrite, live),
		})
	}

	idleSev, idleAdj := both(false)
	liveSev, liveAdj := both(true)

	if liveSev <= idleSev {
		t.Fatalf("two live agents of one person should not be softened: live=%v idle=%v", liveSev, idleSev)
	}
	if liveSev < SeverityMedium {
		t.Fatalf("solo fan-out landed at %v, under the notify floor -- this is the silence the change exists to remove", liveSev)
	}
	if !hasName(liveAdj, PathAdjusterSameMemberConcurrent) {
		t.Errorf("the trail must record that the same-member rule was considered and declined: %v", liveAdj)
	}
	if hasName(liveAdj, PathAdjusterSameMember) {
		t.Errorf("the softening adjuster must not fire when both sessions are live: %v", liveAdj)
	}
	if !hasName(idleAdj, PathAdjusterSameMember) {
		t.Errorf("with one side idle the softening must still apply: %v", idleAdj)
	}
}

func TestABackgroundAgentIsStillSoftened(t *testing.T) {
	const me = "member-1"
	// The case the original rule was actually written for: I left one agent
	// running and I am working in another. I am serializing this myself, so
	// soften it. Only one side is live.
	sev, adj := PathConflictSeverity(SeverityInput{
		A: sideFor(t, me, "feat/a", "internal/auth/token.go", ModeWrite, true),
		B: sideFor(t, me, "feat/b", "internal/auth/token.go", ModeWrite, false),
	})
	if !hasName(adj, PathAdjusterSameMember) {
		t.Fatalf("one live and one idle agent of one person must still soften: %v", adj)
	}
	full, _ := PathConflictSeverity(SeverityInput{
		A: sideFor(t, "member-1", "feat/a", "internal/auth/token.go", ModeWrite, true),
		B: sideFor(t, "member-2", "feat/b", "internal/auth/token.go", ModeWrite, true),
	})
	if sev >= full {
		t.Fatalf("softened %v should be below two different people at %v", sev, full)
	}
}

func TestTwoPeopleAreNeverTouchedByThisRule(t *testing.T) {
	for _, live := range []bool{true, false} {
		_, adj := PathConflictSeverity(SeverityInput{
			A: sideFor(t, "member-1", "main", "internal/auth/token.go", ModeWrite, live),
			B: sideFor(t, "member-2", "main", "internal/auth/token.go", ModeWrite, live),
		})
		if hasName(adj, PathAdjusterSameMember) || hasName(adj, PathAdjusterSameMemberConcurrent) {
			t.Fatalf("live=%v: a same-member adjuster fired for two different people: %v", live, adj)
		}
	}
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
