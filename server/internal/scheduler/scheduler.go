package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"server/internal/db/data"
)

const (
	maxClaimAttempts       = 8
	claimRetryDelay        = 5 * time.Millisecond
	defaultPollingInterval = time.Second
	// DefaultReconciliationGrace must exceed WORKER_DIAL_TIMEOUT. A session has
	// started_at set while the server dials its worker, before worker heartbeats
	// can report the session as active.
	DefaultReconciliationGrace = 15 * time.Second
)

var (
	ErrNoCapacity      = errors.New("scheduler: no capacity")
	ErrQueueFull       = errors.New("scheduler: queue full")
	ErrWaitTimeout     = errors.New("scheduler: queue wait timeout")
	ErrDraining        = errors.New("scheduler: draining")
	errUncertainCommit = errors.New("scheduler: uncertain session claim commit")
)

type Options struct {
	WorkerTTL           time.Duration
	PendingSessionTTL   time.Duration
	MaxLifetimeSessions int64
	MaxQueueSize        int
	QueueWaitTimeout    time.Duration
	PollingInterval     time.Duration
	ReconciliationGrace time.Duration
}

type ClaimRequest struct {
	Browser         string
	VersionPrefix   string
	CreatedByKey    *uuid.UUID
	ConnectMetadata json.RawMessage
}

type Capacity struct {
	Browsers     []data.GetCapacityByBrowserRow
	Queued       int
	MaxQueueSize int
}

type Scheduler struct {
	pool         *pgxpool.Pool
	queries      *data.Queries
	waker        *Waker
	logger       *slog.Logger
	lifecycleCtx context.Context

	workerTTL           time.Duration
	pendingSessionTTL   time.Duration
	maxLifetimeSessions int64
	maxQueueSize        int
	queueWaitTimeout    time.Duration
	pollingInterval     time.Duration
	reconciliationGrace time.Duration

	queueMu sync.Mutex
	queued  int
}

func New(
	ctx context.Context,
	pool *pgxpool.Pool,
	logger *slog.Logger,
	options Options,
) *Scheduler {
	pollingInterval := options.PollingInterval
	if pollingInterval <= 0 {
		pollingInterval = defaultPollingInterval
	}
	reconciliationGrace := options.ReconciliationGrace
	if reconciliationGrace <= 0 {
		reconciliationGrace = DefaultReconciliationGrace
	}
	if logger == nil {
		logger = slog.Default()
	}

	return &Scheduler{
		pool:                pool,
		queries:             data.New(pool),
		waker:               NewWaker(),
		logger:              logger,
		lifecycleCtx:        ctx,
		workerTTL:           options.WorkerTTL,
		pendingSessionTTL:   options.PendingSessionTTL,
		maxLifetimeSessions: options.MaxLifetimeSessions,
		maxQueueSize:        options.MaxQueueSize,
		queueWaitTimeout:    options.QueueWaitTimeout,
		pollingInterval:     pollingInterval,
		reconciliationGrace: reconciliationGrace,
	}
}

func (s *Scheduler) Claim(ctx context.Context, request ClaimRequest) (data.Session, error) {
	excluded := []uuid.UUID{}
	for attempt := range maxClaimAttempts {
		session, fullWorkerID, err := s.claimOnce(ctx, request, excluded)
		if err == nil {
			return session, nil
		}
		if !errors.Is(err, ErrNoCapacity) {
			return data.Session{}, err
		}
		if fullWorkerID != nil {
			excluded = append(excluded, *fullWorkerID)
			continue
		}
		if attempt == maxClaimAttempts-1 {
			break
		}
		if err := waitForRetry(ctx, claimRetryDelay); err != nil {
			return data.Session{}, err
		}
	}

	return data.Session{}, ErrNoCapacity
}

func (s *Scheduler) Admit(ctx context.Context, request ClaimRequest) (data.Session, error) {
	if s.isDraining() {
		return data.Session{}, ErrDraining
	}
	session, err := s.Claim(ctx, request)
	if err == nil {
		return session, nil
	}
	if !errors.Is(err, ErrNoCapacity) {
		return data.Session{}, err
	}
	if s.isDraining() {
		return data.Session{}, ErrDraining
	}
	if !s.enterQueue() {
		return data.Session{}, ErrQueueFull
	}
	defer s.leaveQueue()

	wake, unsubscribe := s.waker.Subscribe()
	defer unsubscribe()
	waitCtx, cancel := context.WithTimeout(ctx, s.queueWaitTimeout)
	defer cancel()
	pollTimer := time.NewTimer(jitteredPollingInterval(s.pollingInterval))
	defer pollTimer.Stop()
	loggedClaimError := false
	excluded := []uuid.UUID{}

	for {
		if waitErr := s.queueWaitError(ctx, waitCtx); waitErr != nil {
			return data.Session{}, waitErr
		}

		claimCtx, cancelClaim := context.WithCancel(waitCtx)
		stopDrainCancel := context.AfterFunc(s.lifecycleCtx, cancelClaim)
		session, fullWorkerID, claimErr := s.claimOnce(claimCtx, request, excluded)
		stopDrainCancel()
		cancelClaim()
		err = claimErr
		if err == nil {
			return session, nil
		}
		if errors.Is(err, ErrNoCapacity) && fullWorkerID != nil {
			excluded = append(excluded, *fullWorkerID)
			continue
		}
		if errors.Is(err, errUncertainCommit) {
			return data.Session{}, err
		}
		excluded = excluded[:0]
		if waitErr := s.queueWaitError(ctx, waitCtx); waitErr != nil {
			return data.Session{}, waitErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return data.Session{}, err
		}
		if !errors.Is(err, ErrNoCapacity) && !loggedClaimError {
			s.logger.Warn("queued session claim failed; waiting to retry", "error", err)
			loggedClaimError = true
		}

		select {
		case <-waitCtx.Done():
			return data.Session{}, s.queueWaitError(ctx, waitCtx)
		case <-s.lifecycleCtx.Done():
			return data.Session{}, ErrDraining
		case <-wake:
		case <-pollTimer.C:
			pollTimer.Reset(jitteredPollingInterval(s.pollingInterval))
		}
	}
}

func (s *Scheduler) Capacity(ctx context.Context) (Capacity, error) {
	rows, err := s.queries.GetCapacityByBrowser(ctx, data.GetCapacityByBrowserParams{
		WorkerTtlMicroseconds: s.workerTTL.Microseconds(),
		MaxLifetimeSessions:   s.maxLifetimeSessions,
	})
	if err != nil {
		return Capacity{}, fmt.Errorf("querying capacity: %w", err)
	}

	return Capacity{
		Browsers:     rows,
		Queued:       s.QueueDepth(),
		MaxQueueSize: s.maxQueueSize,
	}, nil
}

func (s *Scheduler) Heartbeat(
	ctx context.Context,
	workerID uuid.UUID,
	activeSessionIDs []uuid.UUID,
) (_ data.Worker, _ []uuid.UUID, _ []uuid.UUID, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return data.Worker{}, nil, nil, fmt.Errorf("beginning worker heartbeat: %w", err)
	}
	defer rollback(ctx, tx, &err)
	queries := s.queries.WithTx(tx)

	worker, err := queries.UpdateWorkerHeartbeat(ctx, workerID)
	if err != nil {
		return data.Worker{}, nil, nil, fmt.Errorf("updating worker heartbeat: %w", err)
	}

	failedIDs, err := queries.FailUnreportedWorkerSessions(ctx, data.FailUnreportedWorkerSessionsParams{
		WorkerID:          workerID,
		GraceMicroseconds: s.reconciliationGrace.Microseconds(),
		ActiveSessionIds:  activeSessionIDs,
	})
	if err != nil {
		return data.Worker{}, nil, nil, fmt.Errorf("failing unreported worker sessions: %w", err)
	}
	if len(failedIDs) > 0 {
		if err := queries.NotifyCapacityChanged(ctx); err != nil {
			return data.Worker{}, nil, nil, fmt.Errorf("notifying reconciled capacity: %w", err)
		}
	}

	staleIDs, err := queries.ListStaleWorkerSessionIDs(ctx, data.ListStaleWorkerSessionIDsParams{
		ActiveSessionIds: activeSessionIDs,
		WorkerID:         workerID,
	})
	if err != nil {
		return data.Worker{}, nil, nil, fmt.Errorf("listing stale worker sessions: %w", err)
	}
	if staleIDs == nil {
		staleIDs = []uuid.UUID{}
	}

	shouldDrain := s.maxLifetimeSessions > 0 &&
		worker.LifetimeSessions >= s.maxLifetimeSessions &&
		worker.Status == data.WorkerStatusAvailable
	if shouldDrain {
		draining, transitionErr := queries.SetWorkerStatus(ctx, data.SetWorkerStatusParams{
			ID:     workerID,
			Status: data.WorkerStatusDraining,
		})
		switch {
		case transitionErr == nil:
			worker = draining
		case errors.Is(transitionErr, pgx.ErrNoRows):
		default:
			return data.Worker{}, nil, nil, fmt.Errorf("draining lifetime-limited worker: %w", transitionErr)
		}
	}

	// Revival does not notify. The polling fallback discovers its new capacity.
	if err := tx.Commit(ctx); err != nil {
		return data.Worker{}, nil, nil, fmt.Errorf("committing worker heartbeat: %w", err)
	}
	return worker, staleIDs, failedIDs, nil
}

func (s *Scheduler) Waker() *Waker {
	return s.waker
}

func (s *Scheduler) QueueDepth() int {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	return s.queued
}

func (s *Scheduler) claimOnce(
	ctx context.Context,
	request ClaimRequest,
	excluded []uuid.UUID,
) (_ data.Session, fullWorkerID *uuid.UUID, err error) {
	// Capacity invariant: READ COMMITTED gives the recount a fresh snapshot after
	// the worker row lock. Production inserts hold that lock until commit.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return data.Session{}, nil, fmt.Errorf("beginning session claim: %w", err)
	}
	defer rollback(ctx, tx, &err)
	queries := s.queries.WithTx(tx)

	worker, err := queries.SelectClaimableWorker(ctx, data.SelectClaimableWorkerParams{
		Browser:               request.Browser,
		VersionPrefix:         request.VersionPrefix,
		WorkerTtlMicroseconds: s.workerTTL.Microseconds(),
		MaxLifetimeSessions:   s.maxLifetimeSessions,
		ExcludedIds:           excluded,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return data.Session{}, nil, ErrNoCapacity
	}
	if err != nil {
		return data.Session{}, nil, fmt.Errorf("selecting claimable worker: %w", err)
	}
	activeCount, err := queries.CountActiveSessionsByWorker(ctx, worker.ID)
	if err != nil {
		return data.Session{}, nil, fmt.Errorf("recounting worker sessions: %w", err)
	}
	if activeCount >= int64(worker.MaxSlots) {
		workerID := worker.ID
		return data.Session{}, &workerID, ErrNoCapacity
	}

	session, err := queries.InsertClaimedSession(ctx, data.InsertClaimedSessionParams{
		ID:                     uuid.New(),
		WorkerID:               worker.ID,
		Browser:                worker.Browser,
		PlaywrightVersion:      worker.PlaywrightVersion,
		WorkerAddress:          worker.Address,
		CreatedByKey:           request.CreatedByKey,
		PendingTtlMicroseconds: s.pendingSessionTTL.Microseconds(),
		ConnectMetadata:        request.ConnectMetadata,
	})
	if err != nil {
		return data.Session{}, nil, fmt.Errorf("inserting claimed session: %w", err)
	}
	if _, err := queries.IncrementWorkerLifetimeSessions(ctx, worker.ID); err != nil {
		return data.Session{}, nil, fmt.Errorf("incrementing worker lifetime sessions: %w", err)
	}
	// Cancellation or a connection failure during this commit round trip can
	// hide a committed row. The pending-session TTL is the backstop for that
	// maybe-committed row; never auto-retry past an uncertain commit.
	if err := tx.Commit(ctx); err != nil {
		return data.Session{}, nil, fmt.Errorf("%w: %w", errUncertainCommit, err)
	}

	return session, nil, nil
}

func (s *Scheduler) queueWaitError(ctx, waitCtx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.lifecycleCtx.Done():
		return ErrDraining
	default:
	}
	if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
		return ErrWaitTimeout
	}
	return waitCtx.Err()
}

func (s *Scheduler) isDraining() bool {
	return s.lifecycleCtx.Err() != nil
}

func (s *Scheduler) enterQueue() bool {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	if s.queued >= s.maxQueueSize {
		return false
	}
	s.queued++
	return true
}

func (s *Scheduler) leaveQueue() {
	s.queueMu.Lock()
	s.queued--
	s.queueMu.Unlock()
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func jitteredPollingInterval(interval time.Duration) time.Duration {
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
