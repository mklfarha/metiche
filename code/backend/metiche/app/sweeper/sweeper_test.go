package sweeper

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/enums"
)

// These are the table-driven tests that need no database. The ones that
// matter — that a claim really stops appearing in a detection query, that
// retention really deletes and really moves the floor — are in
// integration_test.go and run against real MySQL, because they are
// properties of the schema rather than of this code.

func TestOptionsDefaultsAreThePlanNumbers(t *testing.T) {
	o := Options{}.withDefaults()

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"interval", o.Interval, 30 * time.Second},
		{"session stale", o.SessionStale, 180 * time.Second},
		{"session abandoned", o.SessionAbandoned, 600 * time.Second},
		{"claim hard ceiling", o.ClaimHardCeiling, 4 * time.Hour},
		{"unclaimed hackathon", o.UnclaimedHackathon, 5 * time.Minute},
		{"unclaimed sprint", o.UnclaimedSprint, 2 * time.Hour},
		{"unclaimed steady", o.UnclaimedSteady, 24 * time.Hour},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestRetentionIsOffByDefault is the one default worth a test of its own.
//
// Every other zero value here costs a little efficiency if it drifts. This
// one costs a self-hoster their history.
func TestRetentionIsOffByDefault(t *testing.T) {
	if (Options{}).withDefaults().RetentionEnabled {
		t.Fatal("retention enforcement defaulted to ON; with nothing configured, nothing may be deleted")
	}
	// And the same through the constructor's own normalisation path.
	s := &Sweeper{logger: zap.NewNop(), opts: Options{}.withDefaults()}
	if s.Options().RetentionEnabled {
		t.Fatal("the effective options turned retention enforcement on")
	}
}

func TestAbandonNeverPrecedesStale(t *testing.T) {
	o := Options{SessionStale: 10 * time.Minute, SessionAbandoned: time.Minute}.withDefaults()
	if o.SessionAbandoned < o.SessionStale {
		t.Fatalf("abandon threshold %s is before the stale threshold %s; a session would skip stale entirely",
			o.SessionAbandoned, o.SessionStale)
	}
}

func TestUnclaimedThresholdFollowsCadence(t *testing.T) {
	o := Options{}.withDefaults()
	cases := []struct {
		cadence enums.ProjectCadence
		want    time.Duration
	}{
		{enums.PROJECT_CADENCE_HACKATHON, 5 * time.Minute},
		{enums.PROJECT_CADENCE_SPRINT, 2 * time.Hour},
		{enums.PROJECT_CADENCE_STEADY, 24 * time.Hour},
		// Unknown must land on sprint, not on the impatient end: guessing
		// "hackathon" for an unrecognised value is how a detector starts
		// crying wolf on a team that never opted into five-minute urgency.
		{enums.ProjectCadence(99), 2 * time.Hour},
		{enums.PROJECT_CADENCE_INVALID, 2 * time.Hour},
	}
	for _, c := range cases {
		if got := o.unclaimedThreshold(c.cadence); got != c.want {
			t.Errorf("cadence %v: threshold %s, want %s", c.cadence, got, c.want)
		}
	}
}

func TestUnclaimedDedupeKeyIsStableAndSpecific(t *testing.T) {
	const contract = "11111111-1111-1111-1111-111111111111"
	const session = "22222222-2222-2222-2222-222222222222"
	const other = "33333333-3333-3333-3333-333333333333"

	a := UnclaimedDedupeKey(contract, session)
	if a != UnclaimedDedupeKey(contract, session) {
		t.Fatal("the same pair produced two different dedupe keys; re-detection would spam a new conflict every pass")
	}
	if len(a) != 64 {
		t.Fatalf("dedupe key is %d characters, the column holds 64", len(a))
	}
	if a == UnclaimedDedupeKey(contract, other) {
		t.Fatal("two different sessions share a dedupe key; one consumer's conflict would hide another's")
	}
	if a == UnclaimedDedupeKey(other, session) {
		t.Fatal("two different contracts share a dedupe key")
	}
}

func TestRuleDemotedMatchesLoosely(t *testing.T) {
	tr := teamRow{demotedRules: []string{" Contract_Unclaimed ", "path_overlap"}}
	if !tr.ruleDemoted(DetectorRuleUnclaimed) {
		t.Error("a demoted rule with stray case and spaces was not recognised")
	}
	if tr.ruleDemoted("duplicate_work") {
		t.Error("a rule that was never demoted was reported as demoted")
	}
	if (teamRow{}).ruleDemoted(DetectorRuleUnclaimed) {
		t.Error("a team with no settings demoted a rule")
	}
}

func TestEventBudgetStopsAtZero(t *testing.T) {
	b := &eventBudget{left: 2}
	if !b.take() || !b.take() {
		t.Fatal("the budget refused a take it had room for")
	}
	if b.take() {
		t.Fatal("the budget allowed a take past its limit")
	}
	var nilBudget *eventBudget
	if !nilBudget.take() {
		t.Fatal("a nil budget must mean unbounded, not zero")
	}
}

func TestPlaceholders(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{{0, ""}, {1, "?"}, {3, "?,?,?"}} {
		if got := placeholders(c.n); got != c.want {
			t.Errorf("placeholders(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// TestCloseIsSafeWithoutStart and the lifecycle test below use a Sweeper
// built by hand: Start must not touch the database before its first tick, and
// proving that needs a sweeper whose db is nil.
func TestCloseIsSafeWithoutStart(t *testing.T) {
	s := &Sweeper{logger: zap.NewNop(), opts: Options{}.withDefaults()}
	s.Close()
	s.Close() // twice, because a shutdown path that panics on the second call is a shutdown path nobody can call safely
}

func TestStartReturnsImmediatelyAndCloseStops(t *testing.T) {
	s := &Sweeper{logger: zap.NewNop(), opts: Options{Interval: time.Hour}.withDefaults()}

	done := make(chan struct{})
	go func() {
		s.Start(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return immediately; it must spawn and hand control back")
	}

	stopped := make(chan struct{})
	go func() {
		s.Close()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not stop the loop")
	}
}

func TestStartAfterCloseDoesNothing(t *testing.T) {
	s := &Sweeper{logger: zap.NewNop(), opts: Options{Interval: time.Hour}.withDefaults()}
	s.Close()
	s.Start(context.Background())
	s.Close()
}
