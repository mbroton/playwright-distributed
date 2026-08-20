package rescuer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"server/internal/db/data"
)

type Options struct {
	WorkerTTL        time.Duration
	SessionTTL       time.Duration
	StalledWorkerTTL time.Duration
	Interval         time.Duration
}

type Summary struct {
	StalledWorkers  int64
	ExpiredSessions int
	RemovedWorkers  int
}

type Rescuer struct {
	pool    *pgxpool.Pool
	queries *data.Queries
	logger  *slog.Logger
	options Options
}

func New(pool *pgxpool.Pool, logger *slog.Logger, options Options) *Rescuer {
	return &Rescuer{
		pool:    pool,
		queries: data.New(pool),
		logger:  logger,
		options: options,
	}
}

// Run executes on every replica. Guarded, idempotent writes make concurrent
// sweeps harmless, so the server does not need leader election.
func (r *Rescuer) Run(ctx context.Context) {
	for {
		timer := time.NewTimer(jitter(r.options.Interval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		tickCtx, cancel := context.WithTimeout(ctx, r.options.Interval)
		summary, err := r.Sweep(tickCtx)
		cancel()
		if err != nil {
			r.logger.Error("rescuer sweep failed", "error", err)
			continue
		}
		if summary.StalledWorkers == 0 && summary.ExpiredSessions == 0 && summary.RemovedWorkers == 0 {
			continue
		}
		r.logger.Info(
			"rescuer sweep",
			"stalled_workers", summary.StalledWorkers,
			"expired_sessions", summary.ExpiredSessions,
			"removed_workers", summary.RemovedWorkers,
		)
	}
}

func (r *Rescuer) Sweep(ctx context.Context) (_ Summary, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Summary{}, fmt.Errorf("beginning rescuer sweep: %w", err)
	}
	defer rollback(ctx, tx, &err)
	queries := r.queries.WithTx(tx)

	stalledWorkers, err := queries.StallSilentWorkers(ctx, r.options.WorkerTTL.Microseconds())
	if err != nil {
		return Summary{}, fmt.Errorf("stalling silent workers: %w", err)
	}
	expiredSessions, err := queries.ExpireDeadSessions(ctx, r.options.SessionTTL.Microseconds())
	if err != nil {
		return Summary{}, fmt.Errorf("expiring dead sessions: %w", err)
	}
	if len(expiredSessions) > 0 {
		if err := queries.NotifyCapacityChanged(ctx); err != nil {
			return Summary{}, fmt.Errorf("notifying expired session capacity: %w", err)
		}
	}
	removedWorkers, err := queries.DeleteDeadWorkers(ctx, data.DeleteDeadWorkersParams{
		WorkerTtlMicroseconds:        r.options.WorkerTTL.Microseconds(),
		StalledWorkerTtlMicroseconds: r.options.StalledWorkerTTL.Microseconds(),
	})
	if err != nil {
		return Summary{}, fmt.Errorf("deleting dead workers: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Summary{}, fmt.Errorf("committing rescuer sweep: %w", err)
	}

	return Summary{
		StalledWorkers:  stalledWorkers,
		ExpiredSessions: len(expiredSessions),
		RemovedWorkers:  len(removedWorkers),
	}, nil
}

func jitter(interval time.Duration) time.Duration {
	span := interval * 2 / 5
	return interval*4/5 + time.Duration(rand.Int64N(int64(span)+1))
}

func rollback(ctx context.Context, tx pgx.Tx, err *error) {
	rollbackErr := tx.Rollback(ctx)
	if rollbackErr == nil || errors.Is(rollbackErr, pgx.ErrTxClosed) {
		return
	}
	*err = errors.Join(*err, fmt.Errorf("rolling back transaction: %w", rollbackErr))
}
