package rescuer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
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
	logger  *slog.Logger
	options Options
}

func New(pool *pgxpool.Pool, logger *slog.Logger, options Options) *Rescuer {
	return &Rescuer{
		pool:    pool,
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
			r.logger.Error(
				"rescuer sweep failed",
				"error", err,
				"stalled_workers", summary.StalledWorkers,
				"expired_sessions", summary.ExpiredSessions,
				"removed_workers", summary.RemovedWorkers,
			)
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

func (r *Rescuer) Sweep(ctx context.Context) (Summary, error) {
	summary := Summary{}
	stalledWorkers, err := r.stallSilentWorkers(ctx)
	if err != nil {
		return summary, err
	}
	summary.StalledWorkers = stalledWorkers
	expiredSessions, err := r.expireDeadSessions(ctx)
	if err != nil {
		return summary, err
	}
	summary.ExpiredSessions = len(expiredSessions)
	removedWorkers, err := r.deleteDeadWorkers(ctx)
	if err != nil {
		return summary, err
	}
	summary.RemovedWorkers = len(removedWorkers)

	return summary, nil
}

func (r *Rescuer) stallSilentWorkers(ctx context.Context) (_ int64, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("beginning stall sweep: %w", err)
	}
	defer rollback(ctx, tx, &err)
	queries := data.New(tx)

	stalledWorkers, err := queries.StallSilentWorkers(ctx, r.options.WorkerTTL.Microseconds())
	if err != nil {
		return 0, fmt.Errorf("stalling silent workers: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing stall sweep: %w", err)
	}
	return stalledWorkers, nil
}

func (r *Rescuer) expireDeadSessions(ctx context.Context) (_ []data.ExpireDeadSessionsRow, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning session expiry sweep: %w", err)
	}
	defer rollback(ctx, tx, &err)
	queries := data.New(tx)

	expiredSessions, err := queries.ExpireDeadSessions(ctx, r.options.SessionTTL.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("expiring dead sessions: %w", err)
	}
	if len(expiredSessions) > 0 {
		if err := queries.NotifyCapacityChanged(ctx); err != nil {
			return nil, fmt.Errorf("notifying expired session capacity: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing session expiry sweep: %w", err)
	}
	return expiredSessions, nil
}

func (r *Rescuer) deleteDeadWorkers(ctx context.Context) (_ []uuid.UUID, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning worker deletion sweep: %w", err)
	}
	defer rollback(ctx, tx, &err)
	queries := data.New(tx)

	removedWorkers, err := queries.DeleteDeadWorkers(ctx, data.DeleteDeadWorkersParams{
		WorkerTtlMicroseconds:        r.options.WorkerTTL.Microseconds(),
		StalledWorkerTtlMicroseconds: r.options.StalledWorkerTTL.Microseconds(),
	})
	if err != nil {
		return nil, fmt.Errorf("deleting dead workers: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing worker deletion sweep: %w", err)
	}
	return removedWorkers, nil
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
