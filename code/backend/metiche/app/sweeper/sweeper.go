// Package sweeper is metiche's background pass.
//
// It does three jobs, and the first thing to understand is that only the
// third one changes what is true:
//
//  1. It expires claims and marks dead sessions. Every detection query
//     already filters on `expires_at`, so a claim whose TTL has passed stops
//     producing conflicts whether or not this package ever runs. PLAN.md is
//     explicit about it — "TTL expiry (lazy filter is authoritative; sweeper
//     is for *visibility*)". The sweeper flips the stored status and emits
//     the events so the BOARD and the other agents learn; if the process
//     dies, nothing is wrong, the display is merely stale.
//
//  2. It raises `contract_unclaimed`: somebody declared they CONSUME a
//     contract and nobody has declared they PRODUCE it, for longer than the
//     project's cadence allows. PLAN.md calls this "the single highest-value
//     signal in the system", and it is the one detection that cannot run
//     inside a tool call, because what makes it true is the passage of time
//     rather than anything an agent just said.
//
//  3. It enforces retention, which really deletes rows. That is the one part
//     of this package that destroys information, so it is off unless an
//     operator turned it on, it never touches decisions or contracts, and it
//     advances `team.retention_floor_sequence` so an SSE client reconnecting
//     below the floor is told to reload instead of being handed a hole.
//
// # Safety properties
//
// Idempotent. Every write is conditional on the state it expects to find
// (`... AND status = held`), every event carries a derived idempotency key
// that the unique index on (team_uuid, idempotency_key) collapses, and every
// conflict carries a dedupe key that the unique index on (team_uuid,
// dedupe_key) collapses. Two pods running the same pass at the same instant
// produce the same end state as one, and the second one's duplicate writes
// lose in the database rather than in a race we had to reason about.
//
// Cheap on the team lock. The scans, the classification and — critically —
// every retention DELETE run OUTSIDE any transaction that touches the team
// row. The team row is the sequence lock for the whole system (see
// app/mcp/sequence.go), so anything that holds it holds up every agent on
// that team. The sweeper takes it only to append an event, for the length of
// two statements, and retention never takes it at all: the floor update is
// one single-row UPDATE with no FOR UPDATE and no transaction around it.
//
// No network, no LLM, no secrets. This package issues no outbound HTTP and
// never selects `agent.token_hash`, `invite.code` or
// `notification_channel.target_url` — the three columns in the model that
// must never reach a log line.
package sweeper

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/gofrs/uuid"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/core"
	"github.com/mklfarha/metiche/backend/enums"
)

// ─────────────────────────────────────────────
// Options
// ─────────────────────────────────────────────

// Options is the sweeper's whole configuration. Every field has a safe zero
// value: leave the struct empty and you get the PLAN.md defaults with
// retention enforcement OFF.
type Options struct {
	// Interval is how often Start runs a pass. PLAN.md's claim and session
	// expiry cadence is 30s.
	Interval time.Duration

	// SessionStale is how long a session may go without a heartbeat before it
	// is shown as stale (PLAN.md: 180s), and SessionAbandoned before it is
	// written off entirely (600s).
	//
	// Both are per-team overridable through team.settings
	// (session_stale_seconds / session_abandoned_seconds), which is why they
	// are read per team rather than captured once.
	SessionStale     time.Duration
	SessionAbandoned time.Duration

	// ClaimHardCeiling is the 4h cap a heartbeat cannot push a claim past. It
	// is enforced at write time by claim.hard_expires_at; the sweeper only
	// needs it to catch a row whose hard_expires_at was never set (a legacy
	// or hand-inserted row), so it is a backstop, not the mechanism.
	ClaimHardCeiling time.Duration

	// UnclaimedHackathon / UnclaimedSprint / UnclaimedSteady are how long a
	// `consumes` assertion may sit with no active `produces` before it is a
	// conflict, per project.cadence. PLAN.md and MODEL.md: hackathon ~5min,
	// sprint ~2h, steady ~1d.
	UnclaimedHackathon time.Duration
	UnclaimedSprint    time.Duration
	UnclaimedSteady    time.Duration

	// UnclaimedSeverity is the severity a contract_unclaimed conflict is
	// recorded at. Default medium, which is also the default notify floor —
	// PLAN.md's "record floor low but notify floor medium".
	UnclaimedSeverity enums.ConflictSeverity

	// RetentionEnabled is the operator's switch, and it is FALSE by default
	// on purpose.
	//
	// metiche is self-hostable and its event log is the only source of truth
	// in the model. Deleting a self-hoster's history because a plan row they
	// never looked at happens to carry a number would be inexcusable, so
	// retention enforcement does nothing at all until somebody says so —
	// with nothing configured, nothing is deleted.
	RetentionEnabled bool

	// RetentionInterval is how rarely a given team's retention is actually
	// enforced; `team.last_retention_sweep_at` is what makes that decision,
	// so it is correct across restarts and across pods. Zero means "every
	// pass", which is what an operator calling RunOnce by hand wants.
	RetentionInterval time.Duration

	// LoginSweepEnabled switches the login sweep (step 5, logins.go). That
	// step deletes expired sign-in links and expired, revoked or long-idle
	// browser sessions. It defaults to ON and does not depend on
	// RetentionEnabled: those rows are credentials, not history (BOARD_LOGIN.md
	// §3.3, decision 11).
	//
	// It is a pointer because a missing key must mean true. With a plain bool,
	// a YAML block that omits the key would decode to false, and nothing could
	// tell that apart from an operator writing false. Nil means on.
	// withDefaults fills it in.
	LoginSweepEnabled *bool

	// BatchSize bounds every DELETE and every scan. Retention never issues
	// one unbounded DELETE: a single statement deleting a month of a busy
	// team's events would hold row locks over the whole range while agents
	// are trying to append to it.
	BatchSize int

	// MaxBatches bounds one team's deletion work in one pass, so a team with
	// a year of backlog cannot starve the other teams. The leftovers go on
	// the next pass.
	MaxBatches int

	// MaxSessionsPerTeam and MaxClaimsPerPass bound the expiry scans.
	MaxSessionsPerTeam int
	MaxClaimsPerPass   int

	// MaxUnclaimedPerPass bounds how many contract_unclaimed conflicts one
	// pass will raise. Each one takes the team lock briefly, so this is the
	// knob that bounds the sweeper's total lock footprint.
	MaxUnclaimedPerPass int

	// MaxJudgementsPerPass bounds the open-pairs scan of docs/DECISIONS.md
	// §4.4 — the pairs one team's pass will assign, re-arm or expire. It takes
	// no team lock, so this bounds work rather than contention; the leftovers
	// come back on the next pass, a judging window being minutes long.
	MaxJudgementsPerPass int

	// MaxEscalationsPerPass bounds how many decision conflicts one team's pass
	// will put in front of a person (§4.7). Each one takes the team lock
	// briefly, the way a raised conflict does, and each one interrupts
	// somebody — so this is the knob that bounds how loud one pass can be.
	MaxEscalationsPerPass int

	// MaxEventsPerPass bounds how many events one pass will append. The
	// status flips are NOT bounded by it: the rows are corrected first and
	// the events are emitted afterwards, so hitting the budget costs
	// visibility, never correctness.
	MaxEventsPerPass int

	// Lease takes a MySQL advisory lock for the duration of a pass, so that
	// three pods running the same 30s loop do one pass between them instead
	// of three. OFF by default: it is an optimisation and nothing depends on
	// it — every write underneath is idempotent, so N pods sweeping at once
	// is correct, merely wasteful. Turn it on when running more than one
	// replica.
	Lease bool

	// LeaseName is the advisory lock's name. Change it to run two independent
	// sweepers against one database (a test, usually).
	LeaseName string

	// Clock is the test seam. Nil means time.Now().UTC().
	Clock func() time.Time
}

// Defaults are PLAN.md's numbers, in one place so a reader can check them
// against the document without reading any code.
const (
	DefaultInterval           = 30 * time.Second
	DefaultSessionStale       = 180 * time.Second
	DefaultSessionAbandoned   = 600 * time.Second
	DefaultClaimHardCeiling   = 4 * time.Hour
	DefaultUnclaimedHackathon = 5 * time.Minute
	DefaultUnclaimedSprint    = 2 * time.Hour
	DefaultUnclaimedSteady    = 24 * time.Hour
	DefaultRetentionInterval  = time.Hour
	DefaultBatchSize          = 500
	DefaultMaxBatches         = 20
	DefaultMaxSessions        = 500
	DefaultMaxClaims          = 2000
	DefaultMaxUnclaimed       = 20
	DefaultMaxEvents          = 500
	DefaultLeaseName          = "metiche:sweeper"

	// docs/DECISIONS.md §4.4 and §4.7's two LIMITs.
	DefaultMaxJudgements  = 64
	DefaultMaxEscalations = 16
)

// The escalation budgets of docs/DECISIONS.md §4.7: how long the agents get to
// settle a decision conflict between themselves before a person is asked, by
// the project's cadence.
//
// They are NOT options. coordination.EscalationBudget is the single source —
// it is pure, it is what §9.1 freezes, and it is what the rule actually calls.
// These constants exist so a reader can check the numbers against the document
// without leaving this file, and TestEscalationBudgetsAreTheSpecNumbers pins
// them to the function, so the two can never drift apart in silence.
const (
	DefaultEscalationHackathon = 10 * time.Minute
	DefaultEscalationSprint    = 30 * time.Minute
	DefaultEscalationSteady    = 2 * time.Hour
)

// withDefaults fills the zero values. It deliberately does NOT default
// RetentionEnabled to true under any circumstance.
func (o Options) withDefaults() Options {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.SessionStale <= 0 {
		o.SessionStale = DefaultSessionStale
	}
	if o.SessionAbandoned <= 0 {
		o.SessionAbandoned = DefaultSessionAbandoned
	}
	if o.SessionAbandoned < o.SessionStale {
		// A configuration that abandons before it goes stale would skip the
		// stale state entirely. Honour the intent (abandon at the later of
		// the two) rather than silently dropping a status the board renders.
		o.SessionAbandoned = o.SessionStale
	}
	if o.ClaimHardCeiling <= 0 {
		o.ClaimHardCeiling = DefaultClaimHardCeiling
	}
	if o.UnclaimedHackathon <= 0 {
		o.UnclaimedHackathon = DefaultUnclaimedHackathon
	}
	if o.UnclaimedSprint <= 0 {
		o.UnclaimedSprint = DefaultUnclaimedSprint
	}
	if o.UnclaimedSteady <= 0 {
		o.UnclaimedSteady = DefaultUnclaimedSteady
	}
	if o.UnclaimedSeverity == enums.CONFLICT_SEVERITY_INVALID {
		o.UnclaimedSeverity = enums.CONFLICT_SEVERITY_MEDIUM
	}
	if o.BatchSize <= 0 {
		o.BatchSize = DefaultBatchSize
	}
	if o.MaxBatches <= 0 {
		o.MaxBatches = DefaultMaxBatches
	}
	if o.MaxSessionsPerTeam <= 0 {
		o.MaxSessionsPerTeam = DefaultMaxSessions
	}
	if o.MaxClaimsPerPass <= 0 {
		o.MaxClaimsPerPass = DefaultMaxClaims
	}
	if o.MaxUnclaimedPerPass <= 0 {
		o.MaxUnclaimedPerPass = DefaultMaxUnclaimed
	}
	if o.MaxJudgementsPerPass <= 0 {
		o.MaxJudgementsPerPass = DefaultMaxJudgements
	}
	if o.MaxEscalationsPerPass <= 0 {
		o.MaxEscalationsPerPass = DefaultMaxEscalations
	}
	if o.MaxEventsPerPass <= 0 {
		o.MaxEventsPerPass = DefaultMaxEvents
	}
	if o.LeaseName == "" {
		o.LeaseName = DefaultLeaseName
	}
	if o.LoginSweepEnabled == nil {
		on := true
		o.LoginSweepEnabled = &on
	}
	if o.Clock == nil {
		o.Clock = func() time.Time { return time.Now().UTC() }
	}
	return o
}

// unclaimedThreshold maps a project's cadence onto how long a consumer may
// wait for a producer before it is worth saying something.
func (o Options) unclaimedThreshold(c enums.ProjectCadence) time.Duration {
	switch c {
	case enums.PROJECT_CADENCE_HACKATHON:
		return o.UnclaimedHackathon
	case enums.PROJECT_CADENCE_STEADY:
		return o.UnclaimedSteady
	default:
		// The column defaults to sprint, and an unrecognised value is treated
		// as sprint rather than as hackathon: guessing the impatient end of
		// the range is how a detector starts crying wolf.
		return o.UnclaimedSprint
	}
}

// ─────────────────────────────────────────────
// Report
// ─────────────────────────────────────────────

// Report is what one pass did. It is returned to an operator calling RunOnce
// and logged (at counts only) by the background loop.
type Report struct {
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Duration   time.Duration `json:"duration"`

	// Skipped is true when another pod held the lease, in which case every
	// other counter is zero and nothing was examined.
	Skipped bool `json:"skipped"`

	TeamsScanned int `json:"teams_scanned"`

	ClaimsExpired     int `json:"claims_expired"`
	ClaimPathsExpired int `json:"claim_paths_expired"`
	// ClaimsExpiredWithSession counts claims dropped because their session
	// was written off, rather than because their own TTL ran out.
	ClaimsExpiredWithSession int `json:"claims_expired_with_session"`

	SessionsStale     int `json:"sessions_stale"`
	SessionsAbandoned int `json:"sessions_abandoned"`

	UnclaimedRaised      int `json:"unclaimed_raised"`
	UnclaimedReDetected  int `json:"unclaimed_redetected"`
	InstructionsRaised   int `json:"instructions_raised"`
	EventsEmitted        int `json:"events_emitted"`
	EventsSkippedForDupe int `json:"events_skipped_for_dupe"`

	// ConflictsResolved counts path_overlap conflicts closed because a lapsed
	// claim or an abandoned session cleared the overlap.
	ConflictsResolved int `json:"conflicts_resolved"`

	// The decisions pass (docs/DECISIONS.md §4.4). JudgementsExpired is the
	// quiet one: those pairs asked nobody anything and wrote no event.
	JudgementsAssigned int `json:"judgements_assigned"`
	JudgementsReArmed  int `json:"judgements_rearmed"`
	JudgementsExpired  int `json:"judgements_expired"`

	// ConflictsEscalated counts the decision conflicts this pass put in front
	// of a person (§4.7). It is the loudest thing the sweeper does, so it is
	// counted on its own rather than folded into InstructionsRaised.
	ConflictsEscalated int `json:"conflicts_escalated"`

	// DuplicateNoticesSent counts the incumbents told, after the grace, that a
	// duplicate of their plan is still standing (docs/DUPLICATES.md §4.5).
	// Each one is also counted in InstructionsRaised.
	DuplicateNoticesSent int `json:"duplicate_notices_sent"`

	Retention RetentionReport `json:"retention"`

	// Logins is step 5: expired sign-in links and browser sessions deleted.
	Logins LoginsReport `json:"logins"`

	// Errors are the non-fatal ones: a pass that fails on one team still
	// sweeps the rest, because one wedged team must not freeze every other
	// team's board.
	Errors []string `json:"errors,omitempty"`
}

// RetentionReport is the deletion half, kept separate because it is the half
// an operator will actually want to read.
type RetentionReport struct {
	Enabled         bool `json:"enabled"`
	TeamsConsidered int  `json:"teams_considered"`
	TeamsEnforced   int  `json:"teams_enforced"`
	EventsDeleted   int  `json:"events_deleted"`
	SessionsDeleted int  `json:"sessions_deleted"`
	Batches         int  `json:"batches"`
	FloorsAdvanced  int  `json:"floors_advanced"`
}

func (r *Report) addErr(what string, err error) {
	if err == nil {
		return
	}
	r.Errors = append(r.Errors, what+": "+err.Error())
}

// ─────────────────────────────────────────────
// Sweeper
// ─────────────────────────────────────────────

// Sweeper is the background pass. Construct it with New, start it with Start,
// stop it with Close.
type Sweeper struct {
	db     *sql.DB
	core   *core.Implementation
	logger *zap.Logger
	opts   Options

	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
	closed  bool
}

// New builds a sweeper. It does no I/O: nothing touches the database until
// Start or RunOnce.
func New(coreImpl *core.Implementation, logger *zap.Logger, opts Options) *Sweeper {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Sweeper{
		db:     coreImpl.DB(),
		core:   coreImpl,
		logger: logger,
		opts:   opts.withDefaults(),
	}
}

// Options returns the effective configuration, defaults filled in. Useful to
// log at startup so "retention enforcement is on" is a fact in the record
// rather than an assumption.
func (s *Sweeper) Options() Options { return s.opts }

// Start runs the pass every Interval until ctx is done. It returns
// immediately.
//
// The first pass is deliberately delayed by one interval: a pod that has just
// come up is the pod least likely to have anything useful to say, and a
// rolling deploy of three replicas would otherwise have all three sweep at
// once at t=0.
func (s *Sweeper) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.closed {
		return
	}
	// NOT the caller's context alone: Close must be able to stop the loop even
	// when the caller holds a context that never finishes.
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.started = true

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.opts.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				s.safePass(runCtx)
			}
		}
	}()
}

// Close stops the loop and waits for the pass in flight to finish. It is safe
// to call more than once, and safe to call without Start.
func (s *Sweeper) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	cancel := s.cancel
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

// safePass runs one pass and swallows a panic.
//
// An unrecovered panic in a background goroutine takes the whole process down,
// and this process is also serving every agent's MCP calls. A bug in the
// sweeper must degrade the board, not the API.
func (s *Sweeper) safePass(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("sweeper pass panicked", zap.Any("panic", r))
		}
	}()

	rep, err := s.RunOnce(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down; not worth a line
		}
		s.logger.Warn("sweeper pass failed", zap.Error(err))
		return
	}
	if rep.Skipped {
		return
	}
	if rep.isQuiet() {
		s.logger.Debug("sweeper pass", zap.Int("teams", rep.TeamsScanned), zap.Duration("took", rep.Duration))
		return
	}
	s.logger.Info("sweeper pass",
		zap.Int("teams", rep.TeamsScanned),
		zap.Int("claims_expired", rep.ClaimsExpired),
		zap.Int("sessions_stale", rep.SessionsStale),
		zap.Int("sessions_abandoned", rep.SessionsAbandoned),
		zap.Int("unclaimed_raised", rep.UnclaimedRaised),
		zap.Int("events", rep.EventsEmitted),
		zap.Int("retention_events_deleted", rep.Retention.EventsDeleted),
		zap.Int("retention_sessions_deleted", rep.Retention.SessionsDeleted),
		zap.Int("retention_floors_advanced", rep.Retention.FloorsAdvanced),
		zap.Int("login_links_deleted", rep.Logins.LinksDeleted),
		zap.Int("browser_sessions_deleted", rep.Logins.SessionsDeleted),
		zap.Duration("took", rep.Duration),
		zap.Strings("errors", rep.Errors))
}

func (r Report) isQuiet() bool {
	return r.ClaimsExpired == 0 && r.SessionsStale == 0 && r.SessionsAbandoned == 0 &&
		r.UnclaimedRaised == 0 && r.Retention.EventsDeleted == 0 &&
		r.Retention.SessionsDeleted == 0 && r.Logins.LinksDeleted == 0 &&
		r.Logins.SessionsDeleted == 0 && len(r.Errors) == 0
}

// RunOnce does one complete pass: claim expiry, session liveness,
// contract_unclaimed, retention. It is what the loop calls and what an
// operator can call by hand.
//
// It returns an error only when it could not run at all (the database is
// unreachable, the team list would not load). Anything that failed for one
// team lands in Report.Errors and the pass continues, because a single team
// with a poisoned row must not stop every other team's board from updating.
func (s *Sweeper) RunOnce(ctx context.Context) (Report, error) {
	now := s.now()
	rep := Report{StartedAt: now}
	rep.Retention.Enabled = s.opts.RetentionEnabled
	rep.Logins.Enabled = s.opts.loginSweepOn()
	defer func() {
		rep.FinishedAt = s.now()
		rep.Duration = rep.FinishedAt.Sub(rep.StartedAt)
	}()

	if s.opts.Lease {
		release, got, err := s.acquireLease(ctx)
		if err != nil {
			return rep, fmt.Errorf("taking the sweeper lease: %w", err)
		}
		if !got {
			rep.Skipped = true
			return rep, nil
		}
		defer release()
	}

	// Budget for events, shared across the whole pass so one noisy team
	// cannot spend every other team's share.
	budget := &eventBudget{left: s.opts.MaxEventsPerPass}

	// ── 1. claims whose TTL has passed ──────────────────────────────────────
	// Global rather than per-team: held claims across an instance number in
	// the hundreds and the pass is bounded, so one ordered scan beats one
	// query per team.
	if err := s.expireClaims(ctx, now, &rep, budget); err != nil {
		rep.addErr("expiring claims", err)
	}

	teams, err := s.loadTeams(ctx)
	if err != nil {
		return rep, fmt.Errorf("loading teams: %w", err)
	}
	rep.TeamsScanned = len(teams)

	defaultRetentionDays, err := s.instanceDefaultRetentionDays(ctx)
	if err != nil {
		rep.addErr("reading the instance default plan", err)
	}

	for _, t := range teams {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}

		// ── 2. sessions whose heartbeat stopped ─────────────────────────────
		if err := s.sweepSessions(ctx, t, now, &rep, budget); err != nil {
			rep.addErr("sweeping sessions for team "+t.uuid.String(), err)
		}

		// ── 3. consumers with no producer ───────────────────────────────────
		if err := s.detectUnclaimed(ctx, t, now, &rep); err != nil {
			rep.addErr("detecting contract_unclaimed for team "+t.uuid.String(), err)
		}

		// ── 4. the judging backlog and the judge window ─────────────────────
		// docs/DECISIONS.md §4.4. Takes no team lock and writes no event: a
		// pair handed out, re-armed or given up on changes nothing anybody is
		// told about.
		if err := s.sweepJudgements(ctx, t, now, &rep); err != nil {
			rep.addErr("sweeping the pairs to judge for team "+t.uuid.String(), err)
		}

		// ── 5. the duplicate-work pairs, then the incumbents' notices ───────
		// docs/DUPLICATES.md §3.1 and §4.5. The pair pass is the decision
		// pass's twin, under the same per-minute cap and just as silent. The
		// notice pass runs after it and before escalation: an incumbent hears
		// that a duplicate is still standing before anybody's person does.
		if err := s.sweepDuplicatePairs(ctx, t, now, &rep); err != nil {
			rep.addErr("sweeping the duplicate pairs to judge for team "+t.uuid.String(), err)
		}
		if err := s.noticeDuplicateIncumbents(ctx, t, now, &rep); err != nil {
			rep.addErr("telling incumbents about duplicate work for team "+t.uuid.String(), err)
		}

		// ── 6. conflicts the agents did not settle ──────────────────────────
		// Decisions §4.7 and DUPLICATES.md §4.8, deliberately AFTER the
		// backlog steps: a pair handed out a moment ago is a turn the agents
		// have not had yet.
		if err := s.escalateDecisionConflicts(ctx, t, now, &rep); err != nil {
			rep.addErr("escalating conflicts for team "+t.uuid.String(), err)
		}

		// ── 7. retention ────────────────────────────────────────────────────
		if err := s.enforceRetention(ctx, t, now, defaultRetentionDays, &rep); err != nil {
			rep.addErr("enforcing retention for team "+t.uuid.String(), err)
		}
	}

	// ── 8. expired sign-in links and browser sessions ───────────────────────
	// Instance-wide, and NOT gated on RetentionEnabled (logins.go).
	if err := s.sweepLogins(ctx, now, &rep); err != nil {
		rep.addErr("sweeping expired browser logins", err)
	}

	return rep, nil
}

func (s *Sweeper) now() time.Time { return s.opts.Clock().UTC() }

// ─────────────────────────────────────────────
// Lease
// ─────────────────────────────────────────────

// acquireLease takes a MySQL advisory lock on one pinned connection.
//
// GET_LOCK is per-connection, so the connection must be held for as long as
// the lock is wanted and released explicitly — returning it to the pool with
// the lock still held would hand a later query a connection that owns a lock
// nobody remembers taking.
func (s *Sweeper) acquireLease(ctx context.Context) (release func(), acquired bool, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", s.opts.LeaseName).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, false, err
	}
	if !got.Valid || got.Int64 != 1 {
		_ = conn.Close()
		return nil, false, nil
	}
	return func() {
		// A background context: the pass's context may already be cancelled
		// on shutdown, and a lock that outlives the process start-up window
		// would make the next pod skip every pass.
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(rctx, "DO RELEASE_LOCK(?)", s.opts.LeaseName)
		_ = conn.Close()
	}, true, nil
}

// ─────────────────────────────────────────────
// Teams
// ─────────────────────────────────────────────

// teamRow is the little the sweeper needs about a team.
//
// Deliberately not the whole row and never SELECT *: the team row's neighbours
// in this schema carry join codes and webhook URLs, and the habit of selecting
// only what is used is what keeps a secret out of a log line by construction.
type teamRow struct {
	uuid           uuid.UUID
	sequence       int64
	planUUID       *uuid.UUID
	retentionFloor int64
	lastSweepAt    sql.NullTime
	staleAfter     time.Duration
	abandonedAfter time.Duration
	notifyFloor    enums.ConflictSeverity

	// The decisions settings (docs/DECISIONS.md §4.4, §4.7). humanFloor is the
	// severity a decision conflict must reach before a person is ever asked;
	// it defaults to high, which is why most conflicts never interrupt anybody.
	judgeWindow         time.Duration
	maxReviewsPerMinute int
	humanFloor          enums.ConflictSeverity

	// demotedRules is PLAN.md's self-tuning loop arriving here: a rule that
	// crossed 50% false-positive dismissals on this team is demoted to
	// record-only. The sweeper still records a demoted conflict; it just
	// stops interrupting anybody with it.
	demotedRules []string
}

// ruleDemoted reports whether this team has demoted a detector rule to
// record-only.
func (t teamRow) ruleDemoted(rule string) bool {
	for _, r := range t.demotedRules {
		if strings.EqualFold(strings.TrimSpace(r), rule) {
			return true
		}
	}
	return false
}

func (s *Sweeper) loadTeams(ctx context.Context) ([]teamRow, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT `id`, `sequence`, `plan_uuid`, `retention_floor_sequence`, `last_retention_sweep_at`, `settings` "+
			"FROM `team` WHERE `status` = ? ORDER BY `id`",
		int64(enums.RECORD_STATUS_ACTIVE))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []teamRow
	for rows.Next() {
		var (
			id       string
			seq      int64
			planID   sql.NullString
			floor    int64
			sweptAt  sql.NullTime
			settings []byte
		)
		if err := rows.Scan(&id, &seq, &planID, &floor, &sweptAt, &settings); err != nil {
			return nil, err
		}
		tu, err := uuid.FromString(id)
		if err != nil {
			continue
		}
		t := teamRow{
			uuid:           tu,
			sequence:       seq,
			retentionFloor: floor,
			lastSweepAt:    sweptAt,
			staleAfter:     s.opts.SessionStale,
			abandonedAfter: s.opts.SessionAbandoned,
			notifyFloor:    enums.CONFLICT_SEVERITY_MEDIUM,

			judgeWindow:         decisionJudgeWindow,
			maxReviewsPerMinute: defaultMaxReviewsPerMinute,
			humanFloor:          defaultHumanNotifyFloor,
		}
		if planID.Valid && planID.String != "" {
			if pu, err := uuid.FromString(planID.String); err == nil {
				t.planUUID = &pu
			}
		}
		applyTeamSettings(&t, settings, s.opts)
		out = append(out, t)
	}
	return out, rows.Err()
}

// applyTeamSettings lets a team override the liveness thresholds without a
// deploy — team_settings exists for exactly that, and MODEL.md says so.
func applyTeamSettings(t *teamRow, raw []byte, opts Options) {
	if len(raw) == 0 {
		return
	}
	settings := parseTeamSettings(raw)
	if settings.staleSeconds > 0 {
		t.staleAfter = time.Duration(settings.staleSeconds) * time.Second
	}
	if settings.abandonedSeconds > 0 {
		t.abandonedAfter = time.Duration(settings.abandonedSeconds) * time.Second
	}
	if t.abandonedAfter < t.staleAfter {
		t.abandonedAfter = t.staleAfter
	}
	if settings.notifyFloor != enums.CONFLICT_SEVERITY_INVALID {
		t.notifyFloor = settings.notifyFloor
	}
	t.demotedRules = settings.demotedRules
	applyDecisionSettings(t, raw)
	_ = opts
}

// ─────────────────────────────────────────────
// Small shared helpers
// ─────────────────────────────────────────────

// eventBudget bounds how many events one pass appends. The status flips it
// describes have already happened when this runs out, which is the right way
// round: the sweeper's writes are the truth, its events are the telling.
type eventBudget struct{ left int }

func (b *eventBudget) take() bool {
	if b == nil {
		return true
	}
	if b.left <= 0 {
		return false
	}
	b.left--
	return true
}

// placeholders renders "?,?,?" for an IN list.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func anySlice(ids []uuid.UUID) []any {
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// isDuplicateKey reports whether err is MySQL's 1062.
//
// Duplicate-key is not a failure anywhere in this package: it is how two pods
// agree on who wrote the event, and how re-detection of the same conflict
// becomes a counter bump instead of a second row.
func isDuplicateKey(err error) bool {
	var me *mysqldriver.MySQLError
	if errors.As(err, &me) {
		return me.Number == 1062
	}
	return false
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
