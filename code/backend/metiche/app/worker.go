package app

import (
	"context"

	"go.uber.org/config"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/app/sweeper"
	"github.com/mklfarha/metiche/backend/core"
)

// RegisterWorkers is wired in main.go via fx.Invoke; it is where background work
// lives — schedulers, pollers, ingesters, queue consumers, cache warmers. It runs
// IN THE SAME PROCESS as the API, so it shares the data layer (coreImpl), the
// configuration and the logger with every request handler.
//
// Add your workers below — this file is generated ONCE and will not be
// overwritten. No code generation step is required; rebuild and run.
//
// THE CONTRACT — six things will bite you, five of them only at runtime:
//
// 1. OnStart MUST NOT BLOCK. fx runs the lifecycle hooks sequentially and does
// not finish startup — nor serve a single request — until yours returns. Spawn a
// goroutine and return nil.
//
// 2. THE OnStart CONTEXT IS CANCELLED the moment startup completes: it is the
// startup deadline, not the app's lifetime. A goroutine that inherits it dies
// immediately, usually silently. Derive your long-lived context from
// context.Background() and cancel it in OnStop.
//
// 3. OnStop MUST CANCEL AND WAIT. Cancel that context, then wait for the
// goroutines to return (a sync.WaitGroup, with a select on the stop context so a
// stuck worker cannot hang the shutdown), so in-flight work is not torn down
// mid-write on a rolling deploy.
//
// 4. EVERY REPLICA RUNS THIS. This is the API process; scale it to 3 pods and
// your "every 5 minutes" job runs three times every 5 minutes, concurrently.
// Nothing here coordinates for you. Either pin the deployment to a single
// replica, or take a lease before doing the work — a row in a table with an
// owner and an expiry, or a MySQL advisory lock on a dedicated connection:
//
//	conn, err := coreImpl.DB().Conn(ctx) // GET_LOCK is per-connection
//	defer conn.Close()                   // releasing the connection releases the lock
//	var got int
//	err = conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", "myapp:mywork").Scan(&got)
//	if got != 1 {
//		return // another replica holds it; skip this tick
//	}
//
// 5. A PANIC KILLS THE WHOLE PROCESS. An unrecovered panic in a goroutine is not
// caught by the generated HTTP/gRPC recoverer middleware — it takes the API down
// with it. Recover per unit of work (per tick, per item), log it, and keep the
// loop alive.
//
// 6. THE CORE CACHES ARE SHARED WITH THE API. coreImpl's per-entity caches are
// the ones serving your API reads, and every write flushes that entity's cache.
// A worker writing one row at a time in a tight loop keeps the API reading cold.
// Batch instead: one transaction per logical unit of work.
//
//	tx, err := coreImpl.DB().Begin() // there is no transaction helper on coreImpl
//	defer tx.Rollback()              // no-op once Commit has run
//	// ... coreImpl.Foo().Insert(ctx, v, foo.WithSQLTransaction(tx)) ...
//	return tx.Commit()
//
// WithSQLTransaction is a per-module option — every entity module exports its
// own — so pass the same tx to each call that must share the transaction.
//
// Read intervals, batch sizes, endpoints and feature switches from `provider`
// (for example provider.Get("workers").Populate(&cfg)) instead of hard-coding
// them: they are deploy-time settings, and config/base.yaml is regenerated on
// every run, so real values belong in the deploy-time configuration.
func RegisterWorkers(lc fx.Lifecycle, coreImpl *core.Implementation, provider config.Provider, logger *zap.Logger) {
	if logger == nil {
		logger = zap.NewNop()
	}

	// ── the sweeper ─────────────────────────────────────────────────────────
	//
	// metiche's one background worker. It expires lapsed claims and stale
	// sessions, raises contract_unclaimed conflicts ("somebody is coding
	// against an endpoint nobody is building"), and enforces retention.
	//
	// IT IS NOT THE MECHANISM FOR ANY OF THAT except retention. Expiry is
	// authoritative at read time -- every detection query filters on
	// expires_at, so a lapsed claim stops colliding the instant it lapses,
	// whether or not a pass has run. The sweeper exists so the BOARD agrees
	// with the detector: without it a released claim sits on screen looking
	// held. Correctness does not depend on this loop; only visibility does,
	// which is why a missed pass is not an incident.
	//
	// Retention is the exception, and it is off unless an operator turns it
	// on -- see Options.RetentionEnabled. Nothing is deleted by default.
	opts := sweeper.Options{}
	if err := provider.Get("sweeper").Populate(&opts); err != nil {
		// Not fatal: every field has a safe zero value and withDefaults
		// fills in PLAN.md's numbers, so a malformed block degrades to the
		// documented defaults rather than taking the API down. Retention
		// stays off, because its zero value is off.
		logger.Warn("sweeper config could not be read; using defaults with retention off",
			zap.Error(err))
		opts = sweeper.Options{}
	}
	sw := sweeper.New(coreImpl, logger, opts)

	// NOT the OnStart ctx -- that one is cancelled the moment startup
	// completes, which would kill the loop silently.
	ctx, cancel := context.WithCancel(context.Background())

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			// Start returns immediately and recovers per pass, so a panic in
			// one sweep cannot take the API process down with it.
			sw.Start(ctx)
			eff := sw.Options()
			logger.Info("sweeper started",
				zap.Duration("interval", eff.Interval),
				zap.Bool("retention_enabled", eff.RetentionEnabled),
				zap.Bool("lease", eff.Lease))
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			cancel()
			done := make(chan struct{})
			go func() { sw.Close(); close(done) }()
			select {
			case <-done:
				return nil
			case <-stopCtx.Done():
				// The pass in flight outlived the shutdown deadline. Every
				// write underneath is idempotent, so the next pod redoes it
				// safely; say so rather than failing the shutdown silently.
				logger.Warn("sweeper did not finish its pass before the shutdown deadline")
				return stopCtx.Err()
			}
		},
	})

}
