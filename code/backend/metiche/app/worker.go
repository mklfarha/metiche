package app

import (
	"go.uber.org/config"
	"go.uber.org/fx"
	"go.uber.org/zap"

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

	// Example — add "context", "sync" and "time" to the imports and uncomment:
	//
	// interval := 5 * time.Minute
	// if d := provider.Get("workers.interval").String(); d != "" {
	// 	if parsed, err := time.ParseDuration(d); err == nil {
	// 		interval = parsed
	// 	}
	// }
	//
	// // NOT the OnStart ctx: that one is cancelled when startup completes.
	// ctx, cancel := context.WithCancel(context.Background())
	// var wg sync.WaitGroup
	//
	// lc.Append(fx.Hook{
	// 	OnStart: func(context.Context) error {
	// 		wg.Add(1)
	// 		go func() {
	// 			defer wg.Done()
	// 			ticker := time.NewTicker(interval)
	// 			defer ticker.Stop()
	// 			for {
	// 				select {
	// 				case <-ctx.Done():
	// 					return
	// 				case <-ticker.C:
	// 					// One recover per tick: a panic here would
	// 					// otherwise take the API down with it.
	// 					func() {
	// 						defer func() {
	// 							if r := recover(); r != nil {
	// 								logger.Error("worker panicked", zap.Any("panic", r))
	// 							}
	// 						}()
	// 						// ... take the lease, then do the work through coreImpl ...
	// 					}()
	// 				}
	// 			}
	// 		}()
	// 		return nil // never block startup
	// 	},
	// 	OnStop: func(stopCtx context.Context) error {
	// 		cancel()
	// 		done := make(chan struct{})
	// 		go func() { wg.Wait(); close(done) }()
	// 		select {
	// 		case <-done:
	// 			return nil
	// 		case <-stopCtx.Done(): // shutdown deadline reached
	// 			return stopCtx.Err()
	// 		}
	// 	},
	// })

	logger.Info("no background workers registered")

	_ = lc
	_ = coreImpl
	_ = provider
}
