package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"server/internal/db"
	"server/internal/db/data"
)

const postgresImage = "postgres:18-alpine"

func TestNew_DefaultsNonPositiveReconciliationGrace(t *testing.T) {
	for _, grace := range []time.Duration{0, -time.Second} {
		t.Run(grace.String(), func(t *testing.T) {
			sessionScheduler := New(
				t.Context(),
				nil,
				slog.New(slog.NewTextHandler(io.Discard, nil)),
				Options{ReconciliationGrace: grace},
			)
			if sessionScheduler.reconciliationGrace != defaultReconciliationGrace {
				t.Fatalf(
					"reconciliation grace = %s, want default %s",
					sessionScheduler.reconciliationGrace,
					defaultReconciliationGrace,
				)
			}
		})
	}
}

func TestScheduler_Claims(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)

	t.Run("spreads claims across available slots", func(t *testing.T) {
		truncate(t, pool)
		workerA := insertWorker(t, pool, queries, workerSpec{maxSlots: 2})
		workerB := insertWorker(t, pool, queries, workerSpec{maxSlots: 2})
		scheduler := newTestScheduler(pool, 0, 50)
		for range 4 {
			if _, err := scheduler.Claim(t.Context(), ClaimRequest{Browser: "chromium"}); err != nil {
				t.Fatalf("Claim() returned an error: %v", err)
			}
		}
		for _, workerID := range []uuid.UUID{workerA, workerB} {
			count, err := queries.CountActiveSessionsByWorker(t.Context(), workerID)
			if err != nil {
				t.Fatalf("CountActiveSessionsByWorker() returned an error: %v", err)
			}
			if count != 2 {
				t.Fatalf("worker %s active count = %d, want 2", workerID, count)
			}
		}
	})

	t.Run("prefers aged worker", func(t *testing.T) {
		truncate(t, pool)
		insertWorker(t, pool, queries, workerSpec{lifetimeSessions: 0, maxSlots: 2})
		aged := insertWorker(t, pool, queries, workerSpec{lifetimeSessions: 10, maxSlots: 2})
		session, err := newTestScheduler(pool, 0, 50).Claim(
			t.Context(),
			ClaimRequest{Browser: "chromium"},
		)
		if err != nil {
			t.Fatalf("Claim() returned an error: %v", err)
		}
		if session.WorkerID != aged {
			t.Fatalf("Claim().WorkerID = %s, want aged worker %s", session.WorkerID, aged)
		}
	})

	t.Run("filters workers by Playwright major and minor version", func(t *testing.T) {
		truncate(t, pool)
		matching := insertWorker(t, pool, queries, workerSpec{
			playwrightVersion: "1.62.1",
			maxSlots:          1,
		})
		insertWorker(t, pool, queries, workerSpec{
			playwrightVersion: "1.63.0",
			lifetimeSessions:  20,
			maxSlots:          1,
		})
		session, err := newTestScheduler(pool, 0, 50).Claim(
			t.Context(),
			ClaimRequest{Browser: "chromium", VersionPrefix: "1.62."},
		)
		if err != nil {
			t.Fatalf("Claim() returned an error: %v", err)
		}
		if session.WorkerID != matching {
			t.Fatalf("Claim().WorkerID = %s, want matching worker %s", session.WorkerID, matching)
		}
	})

	t.Run("version prefix does not match a longer minor version", func(t *testing.T) {
		truncate(t, pool)
		insertWorker(t, pool, queries, workerSpec{playwrightVersion: "1.620.0"})
		_, err := newTestScheduler(pool, 0, 50).Claim(
			t.Context(),
			ClaimRequest{Browser: "chromium", VersionPrefix: "1.62."},
		)
		if !errors.Is(err, ErrNoCapacity) {
			t.Fatalf("Claim() error = %v, want %v", err, ErrNoCapacity)
		}
	})

	tests := []struct {
		name        string
		request     string
		ineligible  workerSpec
		maxLifetime int64
	}{
		{name: "draining", request: "chromium", ineligible: workerSpec{status: data.WorkerStatusDraining}},
		{name: "stalled", request: "chromium", ineligible: workerSpec{status: data.WorkerStatusStalled}},
		{name: "shutting down", request: "chromium", ineligible: workerSpec{status: data.WorkerStatusShuttingDown}},
		{name: "stale heartbeat", request: "chromium", ineligible: workerSpec{heartbeatAge: 2 * time.Hour}},
		{name: "lifetime limit", request: "chromium", ineligible: workerSpec{lifetimeSessions: 5}, maxLifetime: 5},
		{name: "wrong browser", request: "chromium", ineligible: workerSpec{browser: "firefox"}},
	}
	for _, test := range tests {
		t.Run("does not claim "+test.name, func(t *testing.T) {
			truncate(t, pool)
			fallback := insertWorker(t, pool, queries, workerSpec{maxSlots: 1})
			test.ineligible.maxSlots = 4
			test.ineligible.lifetimeSessions += 20
			insertWorker(t, pool, queries, test.ineligible)
			limit := test.maxLifetime
			if limit == 0 {
				limit = 50
			}
			session, err := newTestScheduler(pool, 0, limit).Claim(
				t.Context(),
				ClaimRequest{Browser: test.request},
			)
			if err != nil {
				t.Fatalf("Claim() returned an error: %v", err)
			}
			if session.WorkerID != fallback {
				t.Fatalf("Claim().WorkerID = %s, want eligible worker %s", session.WorkerID, fallback)
			}
		})
	}

	t.Run("separate pools cannot over-admit", func(t *testing.T) {
		truncate(t, pool)
		workerID := insertWorker(t, pool, queries, workerSpec{maxSlots: 1})
		otherPool, err := db.Open(t.Context(), pool.Config().ConnString())
		if err != nil {
			t.Fatalf("opening second Postgres pool: %v", err)
		}
		t.Cleanup(otherPool.Close)

		schedulers := []*Scheduler{
			newTestScheduler(pool, 0, 50),
			newTestScheduler(otherPool, 0, 50),
		}
		start := make(chan struct{})
		results := make(chan error, len(schedulers))
		for _, sessionScheduler := range schedulers {
			go func() {
				<-start
				_, err := sessionScheduler.Claim(t.Context(), ClaimRequest{Browser: "chromium"})
				results <- err
			}()
		}
		close(start)

		succeeded := 0
		full := 0
		for range schedulers {
			err := <-results
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrNoCapacity):
				full++
			default:
				t.Fatalf("concurrent Claim() error = %v, want nil or %v", err, ErrNoCapacity)
			}
		}
		if succeeded != 1 || full != 1 {
			t.Fatalf("concurrent claims = %d succeeded and %d full, want 1 and 1", succeeded, full)
		}
		count, err := queries.CountActiveSessionsByWorker(t.Context(), workerID)
		if err != nil {
			t.Fatalf("CountActiveSessionsByWorker() returned an error: %v", err)
		}
		if count != 1 {
			t.Fatalf("active session count = %d, want 1", count)
		}
	})

	t.Run("read committed recount overrides repeatable read pool default", func(t *testing.T) {
		truncate(t, pool)
		workerA := insertWorker(t, pool, queries, workerSpec{maxSlots: 1, lifetimeSessions: 10})
		workerB := insertWorker(t, pool, queries, workerSpec{maxSlots: 1})
		selected := make(chan struct{})
		resume := make(chan struct{})
		claimDSN := withDSNOption(
			t,
			pool.Config().ConnString(),
			"options",
			`-c default_transaction_isolation=repeatable\ read`,
		)
		config, err := pgxpool.ParseConfig(claimDSN)
		if err != nil {
			t.Fatalf("parsing traced pool config: %v", err)
		}
		config.ConnConfig.Tracer = &pauseAfterWorkerSelectionTracer{
			selected: selected,
			resume:   resume,
		}
		claimPool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatalf("opening traced claim pool: %v", err)
		}
		t.Cleanup(claimPool.Close)
		var isolation string
		if err := claimPool.QueryRow(t.Context(), "SHOW default_transaction_isolation").Scan(&isolation); err != nil {
			t.Fatalf("reading traced pool isolation: %v", err)
		}
		if isolation != "repeatable read" {
			t.Fatalf("traced pool isolation = %q, want repeatable read", isolation)
		}
		sessionScheduler := newTestScheduler(claimPool, 0, 50)

		type claimResult struct {
			session data.Session
			err     error
		}
		result := make(chan claimResult, 1)
		go func() {
			session, err := sessionScheduler.Claim(t.Context(), ClaimRequest{Browser: "chromium"})
			result <- claimResult{session: session, err: err}
		}()
		<-selected
		worker, err := queries.GetWorker(t.Context(), workerA)
		if err != nil {
			t.Fatalf("GetWorker() returned an error: %v", err)
		}
		_, err = pool.Exec(
			t.Context(),
			`INSERT INTO sessions (
    id, worker_id, browser, playwright_version, worker_address,
    mode, status, started_at, last_heartbeat
) VALUES ($1, $2, $3, $4, $5, 'default', 'running', now(), now())`,
			uuid.New(),
			worker.ID,
			worker.Browser,
			worker.PlaywrightVersion,
			worker.Address,
		)
		if err != nil {
			t.Fatalf("filling selected worker from second connection: %v", err)
		}
		close(resume)
		claimed := <-result
		if claimed.err != nil {
			t.Fatalf("Claim() returned an error: %v", claimed.err)
		}
		if claimed.session.WorkerID != workerB {
			t.Fatalf("Claim().WorkerID = %s, want fallback worker %s", claimed.session.WorkerID, workerB)
		}
		count, err := queries.CountActiveSessionsByWorker(t.Context(), workerA)
		if err != nil {
			t.Fatalf("CountActiveSessionsByWorker() returned an error: %v", err)
		}
		if count != 1 {
			t.Fatalf("selected worker active count = %d, want 1", count)
		}
	})
}

func TestScheduler_Queue(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)

	t.Run("notification admits after completion", func(t *testing.T) {
		truncate(t, pool)
		insertWorker(t, pool, queries, workerSpec{maxSlots: 1})
		scheduler := newTestScheduler(pool, 1, 50)
		first, err := scheduler.Claim(t.Context(), ClaimRequest{Browser: "chromium"})
		if err != nil {
			t.Fatalf("first Claim() returned an error: %v", err)
		}
		if _, err := queries.StartSession(t.Context(), first.ID); err != nil {
			t.Fatalf("StartSession() returned an error: %v", err)
		}
		listenerCtx, cancelListener := context.WithCancel(t.Context())
		defer cancelListener()
		go RunListener(listenerCtx, pool, scheduler.Waker(), slog.New(slog.NewTextHandler(io.Discard, nil)))

		result := make(chan error, 1)
		go func() {
			_, err := scheduler.Admit(t.Context(), ClaimRequest{Browser: "chromium"})
			result <- err
		}()
		waitForQueueDepth(t, scheduler, 1)
		if _, err := queries.CompleteSession(t.Context(), first.ID); err != nil {
			t.Fatalf("CompleteSession() returned an error: %v", err)
		}
		if err := <-result; err != nil {
			t.Fatalf("queued Admit() returned an error: %v", err)
		}
	})

	t.Run("full queue rejects immediately", func(t *testing.T) {
		truncate(t, pool)
		insertWorker(t, pool, queries, workerSpec{maxSlots: 1})
		scheduler := newTestScheduler(pool, 1, 50)
		if _, err := scheduler.Claim(t.Context(), ClaimRequest{Browser: "chromium"}); err != nil {
			t.Fatalf("Claim() returned an error: %v", err)
		}
		firstCtx, cancelFirst := context.WithCancel(t.Context())
		defer cancelFirst()
		first := make(chan error, 1)
		go func() {
			_, err := scheduler.Admit(firstCtx, ClaimRequest{Browser: "chromium"})
			first <- err
		}()
		waitForQueueDepth(t, scheduler, 1)
		if _, err := scheduler.Admit(t.Context(), ClaimRequest{Browser: "chromium"}); !errors.Is(err, ErrQueueFull) {
			t.Fatalf("second Admit() error = %v, want %v", err, ErrQueueFull)
		}
		cancelFirst()
		if err := <-first; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Admit() error = %v, want %v", err, context.Canceled)
		}
	})

	t.Run("timeout returns wait timeout", func(t *testing.T) {
		truncate(t, pool)
		insertWorker(t, pool, queries, workerSpec{maxSlots: 1})
		scheduler := newTestScheduler(pool, 1, 50)
		scheduler.queueWaitTimeout = 25 * time.Millisecond
		if _, err := scheduler.Claim(t.Context(), ClaimRequest{Browser: "chromium"}); err != nil {
			t.Fatalf("Claim() returned an error: %v", err)
		}
		if _, err := scheduler.Admit(t.Context(), ClaimRequest{Browser: "chromium"}); !errors.Is(err, ErrWaitTimeout) {
			t.Fatalf("Admit() error = %v, want %v", err, ErrWaitTimeout)
		}
	})

	t.Run("committed queued claim wins over queue deadline", func(t *testing.T) {
		truncate(t, pool)
		insertWorker(t, pool, queries, workerSpec{maxSlots: 1})
		first, err := newTestScheduler(pool, 0, 50).Claim(
			t.Context(),
			ClaimRequest{Browser: "chromium"},
		)
		if err != nil {
			t.Fatalf("first Claim() returned an error: %v", err)
		}

		commitEnded := make(chan struct{})
		resumeCommit := make(chan struct{})
		config, err := pgxpool.ParseConfig(pool.Config().ConnString())
		if err != nil {
			t.Fatalf("parsing commit-traced pool config: %v", err)
		}
		config.ConnConfig.Tracer = &pauseAfterCommitTracer{
			commitEnded: commitEnded,
			resume:      resumeCommit,
		}
		claimPool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatalf("opening commit-traced pool: %v", err)
		}
		t.Cleanup(claimPool.Close)
		sessionScheduler := newTestScheduler(claimPool, 1, 50)
		sessionScheduler.queueWaitTimeout = 50 * time.Millisecond

		type admitResult struct {
			session data.Session
			err     error
		}
		result := make(chan admitResult, 1)
		go func() {
			session, err := sessionScheduler.Admit(t.Context(), ClaimRequest{Browser: "chromium"})
			result <- admitResult{session: session, err: err}
		}()
		waitForQueueDepth(t, sessionScheduler, 1)
		if _, err := queries.FailSession(t.Context(), first.ID); err != nil {
			t.Fatalf("FailSession() returned an error: %v", err)
		}
		sessionScheduler.Waker().Wake()
		select {
		case <-commitEnded:
		case <-time.After(time.Second):
			t.Fatal("queued claim did not reach its commit boundary")
		}
		time.Sleep(2 * sessionScheduler.queueWaitTimeout)
		close(resumeCommit)

		admitted := <-result
		if admitted.err != nil {
			t.Fatalf("Admit() returned an error after commit: %v", admitted.err)
		}
		if admitted.session.ID == uuid.Nil {
			t.Fatal("Admit() returned an empty session")
		}
	})

	t.Run("canceled request frees queue slot", func(t *testing.T) {
		truncate(t, pool)
		insertWorker(t, pool, queries, workerSpec{maxSlots: 1})
		scheduler := newTestScheduler(pool, 1, 50)
		if _, err := scheduler.Claim(t.Context(), ClaimRequest{Browser: "chromium"}); err != nil {
			t.Fatalf("Claim() returned an error: %v", err)
		}
		requestCtx, cancelRequest := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() {
			_, err := scheduler.Admit(requestCtx, ClaimRequest{Browser: "chromium"})
			result <- err
		}()
		waitForQueueDepth(t, scheduler, 1)
		cancelRequest()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Admit() error = %v, want %v", err, context.Canceled)
		}
		waitForQueueDepth(t, scheduler, 0)
		scheduler.queueWaitTimeout = 20 * time.Millisecond
		if _, err := scheduler.Admit(t.Context(), ClaimRequest{Browser: "chromium"}); !errors.Is(err, ErrWaitTimeout) {
			t.Fatalf("later Admit() error = %v, want %v", err, ErrWaitTimeout)
		}
	})

	t.Run("lifecycle cancellation drains queued waiter", func(t *testing.T) {
		truncate(t, pool)
		insertWorker(t, pool, queries, workerSpec{maxSlots: 1})
		lifecycleCtx, cancelLifecycle := context.WithCancel(t.Context())
		sessionScheduler := New(
			lifecycleCtx,
			pool,
			slog.New(slog.NewTextHandler(io.Discard, nil)),
			Options{
				WorkerTTL:           time.Hour,
				PendingSessionTTL:   time.Hour,
				MaxLifetimeSessions: 50,
				MaxQueueSize:        1,
				QueueWaitTimeout:    30 * time.Second,
				PollingInterval:     time.Second,
			},
		)
		if _, err := sessionScheduler.Claim(t.Context(), ClaimRequest{Browser: "chromium"}); err != nil {
			t.Fatalf("Claim() returned an error: %v", err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := sessionScheduler.Admit(t.Context(), ClaimRequest{Browser: "chromium"})
			result <- err
		}()
		waitForQueueDepth(t, sessionScheduler, 1)
		cancelLifecycle()
		select {
		case err := <-result:
			if !errors.Is(err, ErrDraining) {
				t.Fatalf("drained Admit() error = %v, want %v", err, ErrDraining)
			}
		case <-time.After(time.Second):
			t.Fatal("queued Admit() did not return after lifecycle cancellation")
		}
		waitForQueueDepth(t, sessionScheduler, 0)
	})

	t.Run("lifecycle cancellation reaches queued claim blocked on pool", func(t *testing.T) {
		truncate(t, pool)
		insertWorker(t, pool, queries, workerSpec{maxSlots: 1})
		if _, err := newTestScheduler(pool, 0, 50).Claim(
			t.Context(),
			ClaimRequest{Browser: "chromium"},
		); err != nil {
			t.Fatalf("first Claim() returned an error: %v", err)
		}

		rollbackEnded := make(chan struct{})
		resumeRollback := make(chan struct{})
		config, err := pgxpool.ParseConfig(pool.Config().ConnString())
		if err != nil {
			t.Fatalf("parsing rollback-traced pool config: %v", err)
		}
		config.MaxConns = 2
		config.ConnConfig.Tracer = &pauseAfterRollbackTracer{
			rollbackEnded: rollbackEnded,
			resume:        resumeRollback,
			remaining:     maxClaimAttempts,
		}
		claimPool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatalf("opening rollback-traced pool: %v", err)
		}
		t.Cleanup(claimPool.Close)
		firstConnection, err := claimPool.Acquire(t.Context())
		if err != nil {
			t.Fatalf("acquiring first held connection: %v", err)
		}
		defer firstConnection.Release()

		lifecycleCtx, cancelLifecycle := context.WithCancel(t.Context())
		sessionScheduler := New(
			lifecycleCtx,
			claimPool,
			slog.New(slog.NewTextHandler(io.Discard, nil)),
			Options{
				WorkerTTL:           time.Hour,
				PendingSessionTTL:   time.Hour,
				MaxLifetimeSessions: 50,
				MaxQueueSize:        1,
				QueueWaitTimeout:    30 * time.Second,
				PollingInterval:     time.Second,
			},
		)
		result := make(chan error, 1)
		go func() {
			_, err := sessionScheduler.Admit(t.Context(), ClaimRequest{Browser: "chromium"})
			result <- err
		}()
		select {
		case <-rollbackEnded:
		case <-time.After(time.Second):
			t.Fatal("fast-path claim did not reach rollback boundary")
		}

		type acquireResult struct {
			connection *pgxpool.Conn
			err        error
		}
		secondConnectionResult := make(chan acquireResult, 1)
		go func() {
			connection, err := claimPool.Acquire(t.Context())
			secondConnectionResult <- acquireResult{connection: connection, err: err}
		}()
		select {
		case acquired := <-secondConnectionResult:
			if acquired.connection != nil {
				acquired.connection.Release()
			}
			t.Fatalf("second pool acquisition completed before rollback resumed: %v", acquired.err)
		case <-time.After(20 * time.Millisecond):
		}
		close(resumeRollback)
		var acquired acquireResult
		select {
		case acquired = <-secondConnectionResult:
		case <-time.After(time.Second):
			t.Fatal("waiting pool acquisition did not receive the released connection")
		}
		if acquired.err != nil {
			t.Fatalf("acquiring second held connection: %v", acquired.err)
		}
		secondConnection := acquired.connection
		defer secondConnection.Release()
		waitForQueueDepth(t, sessionScheduler, 1)

		cancelLifecycle()
		select {
		case err := <-result:
			if !errors.Is(err, ErrDraining) {
				t.Fatalf("drained blocked Admit() error = %v, want %v", err, ErrDraining)
			}
		case <-time.After(time.Second):
			t.Fatal("queued Admit() stayed blocked in the pool after lifecycle cancellation")
		}
	})

	t.Run("polling works without listener", func(t *testing.T) {
		truncate(t, pool)
		insertWorker(t, pool, queries, workerSpec{maxSlots: 1})
		scheduler := newTestScheduler(pool, 1, 50)
		scheduler.pollingInterval = 20 * time.Millisecond
		first, err := scheduler.Claim(t.Context(), ClaimRequest{Browser: "chromium"})
		if err != nil {
			t.Fatalf("Claim() returned an error: %v", err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := scheduler.Admit(t.Context(), ClaimRequest{Browser: "chromium"})
			result <- err
		}()
		waitForQueueDepth(t, scheduler, 1)
		if _, err := queries.FailSession(t.Context(), first.ID); err != nil {
			t.Fatalf("FailSession() returned an error: %v", err)
		}
		if err := <-result; err != nil {
			t.Fatalf("Admit() through polling fallback returned an error: %v", err)
		}
	})

	t.Run("polling works after listener backend dies", func(t *testing.T) {
		truncate(t, pool)
		insertWorker(t, pool, queries, workerSpec{maxSlots: 1})
		sessionScheduler := newTestScheduler(pool, 1, 50)
		sessionScheduler.pollingInterval = 20 * time.Millisecond
		first, err := sessionScheduler.Claim(t.Context(), ClaimRequest{Browser: "chromium"})
		if err != nil {
			t.Fatalf("Claim() returned an error: %v", err)
		}

		listenerResult := make(chan error, 1)
		go func() {
			_, err := listen(t.Context(), pool, sessionScheduler.Waker())
			listenerResult <- err
		}()
		listenerPID := waitForListenerBackend(t, pool)
		result := make(chan error, 1)
		go func() {
			_, err := sessionScheduler.Admit(t.Context(), ClaimRequest{Browser: "chromium"})
			result <- err
		}()
		waitForQueueDepth(t, sessionScheduler, 1)
		var terminated bool
		if err := pool.QueryRow(
			t.Context(),
			"SELECT pg_terminate_backend($1)",
			listenerPID,
		).Scan(&terminated); err != nil {
			t.Fatalf("terminating listener backend: %v", err)
		}
		if !terminated {
			t.Fatalf("pg_terminate_backend(%d) returned false", listenerPID)
		}
		if err := <-listenerResult; err == nil {
			t.Fatal("listen() returned nil after its backend was terminated")
		}
		if _, err := queries.FailSession(t.Context(), first.ID); err != nil {
			t.Fatalf("FailSession() returned an error: %v", err)
		}
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("Admit() through polling fallback returned an error: %v", err)
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatal("queued Admit() was not admitted within polling fallback interval")
		}
	})
}

func TestScheduler_HeartbeatReconciliationAndRecycling(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)

	t.Run("reconciles sessions in both directions", func(t *testing.T) {
		truncate(t, pool)
		workerID := insertWorker(t, pool, queries, workerSpec{maxSlots: 6})
		otherWorkerID := insertWorker(t, pool, queries, workerSpec{maxSlots: 1})
		oldLost := insertSession(t, pool, queries, workerID, data.SessionStatusRunning)
		young := insertSession(t, pool, queries, workerID, data.SessionStatusRunning)
		youngSession, err := queries.GetSession(t.Context(), young)
		if err != nil {
			t.Fatalf("GetSession(young) returned an error: %v", err)
		}
		if youngSession.StartedAt == nil {
			t.Fatal("young running session has a nil started_at")
		}
		pending := insertSession(t, pool, queries, workerID, data.SessionStatusPending)
		otherSession := insertSession(t, pool, queries, otherWorkerID, data.SessionStatusRunning)
		if _, err := pool.Exec(
			t.Context(),
			"UPDATE sessions SET started_at = now() - interval '20 seconds' WHERE id = $1",
			oldLost,
		); err != nil {
			t.Fatalf("aging lost session: %v", err)
		}
		zombie := uuid.New()
		worker, stale, failed, err := newTestScheduler(pool, 0, 50).Heartbeat(
			t.Context(),
			workerID,
			[]uuid.UUID{young, zombie, otherSession},
		)
		if err != nil {
			t.Fatalf("Heartbeat() returned an error: %v", err)
		}
		if worker.Status != data.WorkerStatusAvailable {
			t.Fatalf("Heartbeat().Status = %q, want available", worker.Status)
		}
		if !slices.Equal(stale, []uuid.UUID{zombie, otherSession}) &&
			!slices.Equal(stale, []uuid.UUID{otherSession, zombie}) {
			t.Fatalf("Heartbeat() stale IDs = %v, want %s and %s", stale, zombie, otherSession)
		}
		if len(failed) != 1 || failed[0] != oldLost {
			t.Fatalf("Heartbeat() failed IDs = %v, want %s", failed, oldLost)
		}
		assertSessionStatus(t, queries, oldLost, data.SessionStatusFailed)
		assertSessionStatus(t, queries, young, data.SessionStatusRunning)
		assertSessionStatus(t, queries, pending, data.SessionStatusPending)
		assertSessionStatus(t, queries, otherSession, data.SessionStatusRunning)
	})

	t.Run("reconciliation grace starts when session starts", func(t *testing.T) {
		truncate(t, pool)
		workerID := insertWorker(t, pool, queries, workerSpec{maxSlots: 1})
		sessionScheduler := New(
			t.Context(),
			pool,
			slog.New(slog.NewTextHandler(io.Discard, nil)),
			Options{
				WorkerTTL:           time.Hour,
				PendingSessionTTL:   time.Hour,
				MaxLifetimeSessions: 50,
				ReconciliationGrace: time.Second,
			},
		)
		claimed, err := sessionScheduler.Claim(t.Context(), ClaimRequest{Browser: "chromium"})
		if err != nil {
			t.Fatalf("Claim() returned an error: %v", err)
		}
		if _, err := pool.Exec(
			t.Context(),
			"UPDATE sessions SET created_at = now() - interval '20 seconds' WHERE id = $1",
			claimed.ID,
		); err != nil {
			t.Fatalf("aging pending session: %v", err)
		}
		started, err := queries.StartSession(t.Context(), claimed.ID)
		if err != nil {
			t.Fatalf("StartSession() returned an error: %v", err)
		}
		if started.StartedAt == nil {
			t.Fatal("StartSession().StartedAt is nil")
		}
		_, _, failed, err := sessionScheduler.Heartbeat(t.Context(), workerID, []uuid.UUID{})
		if err != nil {
			t.Fatalf("first Heartbeat() returned an error: %v", err)
		}
		if len(failed) != 0 {
			t.Fatalf("first Heartbeat() failed IDs = %v, want none", failed)
		}
		assertSessionStatus(t, queries, claimed.ID, data.SessionStatusRunning)

		if _, err := pool.Exec(
			t.Context(),
			"UPDATE sessions SET started_at = now() - interval '2 seconds' WHERE id = $1",
			claimed.ID,
		); err != nil {
			t.Fatalf("aging started session: %v", err)
		}
		_, _, failed, err = sessionScheduler.Heartbeat(t.Context(), workerID, []uuid.UUID{})
		if err != nil {
			t.Fatalf("second Heartbeat() returned an error: %v", err)
		}
		if len(failed) != 1 || failed[0] != claimed.ID {
			t.Fatalf("second Heartbeat() failed IDs = %v, want %s", failed, claimed.ID)
		}
		assertSessionStatus(t, queries, claimed.ID, data.SessionStatusFailed)
	})

	t.Run("drains at lifetime limit and stays draining", func(t *testing.T) {
		truncate(t, pool)
		workerID := insertWorker(t, pool, queries, workerSpec{lifetimeSessions: 5})
		scheduler := newTestScheduler(pool, 0, 5)
		worker, _, _, err := scheduler.Heartbeat(t.Context(), workerID, []uuid.UUID{})
		if err != nil {
			t.Fatalf("Heartbeat() returned an error: %v", err)
		}
		if worker.Status != data.WorkerStatusDraining {
			t.Fatalf("first heartbeat status = %q, want draining", worker.Status)
		}
		worker, _, _, err = scheduler.Heartbeat(t.Context(), workerID, []uuid.UUID{})
		if err != nil {
			t.Fatalf("second Heartbeat() returned an error: %v", err)
		}
		if worker.Status != data.WorkerStatusDraining {
			t.Fatalf("second heartbeat status = %q, want draining", worker.Status)
		}
	})

	t.Run("disabled lifetime limit stays available", func(t *testing.T) {
		truncate(t, pool)
		workerID := insertWorker(t, pool, queries, workerSpec{lifetimeSessions: 100})
		worker, _, _, err := newTestScheduler(pool, 0, 0).Heartbeat(
			t.Context(),
			workerID,
			[]uuid.UUID{},
		)
		if err != nil {
			t.Fatalf("Heartbeat() returned an error: %v", err)
		}
		if worker.Status != data.WorkerStatusAvailable {
			t.Fatalf("heartbeat status = %q, want available", worker.Status)
		}
	})

	t.Run("heartbeat preserves worker intent and revives stalled worker", func(t *testing.T) {
		tests := []struct {
			name     string
			status   data.WorkerStatus
			expected data.WorkerStatus
		}{
			{name: "draining intent", status: data.WorkerStatusDraining, expected: data.WorkerStatusDraining},
			{name: "shutdown intent", status: data.WorkerStatusShuttingDown, expected: data.WorkerStatusShuttingDown},
			{name: "stalled revival", status: data.WorkerStatusStalled, expected: data.WorkerStatusAvailable},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				truncate(t, pool)
				workerID := insertWorker(t, pool, queries, workerSpec{status: test.status})
				worker, _, _, err := newTestScheduler(pool, 0, 50).Heartbeat(
					t.Context(),
					workerID,
					[]uuid.UUID{},
				)
				if err != nil {
					t.Fatalf("Heartbeat() returned an error: %v", err)
				}
				if worker.Status != test.expected {
					t.Fatalf("Heartbeat().Status = %q, want %q", worker.Status, test.expected)
				}
				stored, err := queries.GetWorker(t.Context(), workerID)
				if err != nil {
					t.Fatalf("GetWorker() returned an error: %v", err)
				}
				if stored.Status != test.expected {
					t.Fatalf("stored worker status = %q, want %q", stored.Status, test.expected)
				}
			})
		}
	})
}

type pauseAfterWorkerSelectionKey struct{}
type pauseAfterCommitKey struct{}
type pauseAfterRollbackKey struct{}

type pauseAfterWorkerSelectionTracer struct {
	selected chan<- struct{}
	resume   <-chan struct{}
	once     sync.Once
}

func (t *pauseAfterWorkerSelectionTracer) TraceQueryStart(
	ctx context.Context,
	_ *pgx.Conn,
	data pgx.TraceQueryStartData,
) context.Context {
	if strings.Contains(data.SQL, "-- name: SelectClaimableWorker") {
		return context.WithValue(ctx, pauseAfterWorkerSelectionKey{}, true)
	}
	return ctx
}

func (t *pauseAfterWorkerSelectionTracer) TraceQueryEnd(
	ctx context.Context,
	_ *pgx.Conn,
	_ pgx.TraceQueryEndData,
) {
	isWorkerSelection, _ := ctx.Value(pauseAfterWorkerSelectionKey{}).(bool)
	if !isWorkerSelection {
		return
	}
	t.once.Do(func() {
		close(t.selected)
		<-t.resume
	})
}

type pauseAfterCommitTracer struct {
	commitEnded chan<- struct{}
	resume      <-chan struct{}
	once        sync.Once
}

func (t *pauseAfterCommitTracer) TraceQueryStart(
	ctx context.Context,
	_ *pgx.Conn,
	data pgx.TraceQueryStartData,
) context.Context {
	if strings.EqualFold(strings.TrimSpace(data.SQL), "commit") {
		return context.WithValue(ctx, pauseAfterCommitKey{}, true)
	}
	return ctx
}

func (t *pauseAfterCommitTracer) TraceQueryEnd(
	ctx context.Context,
	_ *pgx.Conn,
	_ pgx.TraceQueryEndData,
) {
	isCommit, _ := ctx.Value(pauseAfterCommitKey{}).(bool)
	if !isCommit {
		return
	}
	t.once.Do(func() {
		close(t.commitEnded)
		<-t.resume
	})
}

type pauseAfterRollbackTracer struct {
	rollbackEnded chan<- struct{}
	resume        <-chan struct{}
	remaining     int
	once          sync.Once
}

func (t *pauseAfterRollbackTracer) TraceQueryStart(
	ctx context.Context,
	_ *pgx.Conn,
	data pgx.TraceQueryStartData,
) context.Context {
	if strings.EqualFold(strings.TrimSpace(data.SQL), "rollback") {
		return context.WithValue(ctx, pauseAfterRollbackKey{}, true)
	}
	return ctx
}

func (t *pauseAfterRollbackTracer) TraceQueryEnd(
	ctx context.Context,
	_ *pgx.Conn,
	_ pgx.TraceQueryEndData,
) {
	isRollback, _ := ctx.Value(pauseAfterRollbackKey{}).(bool)
	if !isRollback {
		return
	}
	t.remaining--
	if t.remaining > 0 {
		return
	}
	t.once.Do(func() {
		close(t.rollbackEnded)
		<-t.resume
	})
}

type workerSpec struct {
	browser           string
	playwrightVersion string
	status            data.WorkerStatus
	maxSlots          int32
	lifetimeSessions  int64
	heartbeatAge      time.Duration
}

func newTestScheduler(pool *pgxpool.Pool, maxQueueSize int, maxLifetime int64) *Scheduler {
	return New(context.Background(), pool, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		WorkerTTL:           time.Hour,
		PendingSessionTTL:   time.Hour,
		MaxLifetimeSessions: maxLifetime,
		MaxQueueSize:        maxQueueSize,
		QueueWaitTimeout:    time.Second,
		PollingInterval:     10 * time.Millisecond,
	})
}

func insertWorker(
	t *testing.T,
	pool *pgxpool.Pool,
	queries *data.Queries,
	spec workerSpec,
) uuid.UUID {
	t.Helper()
	if spec.browser == "" {
		spec.browser = "chromium"
	}
	if spec.status == "" {
		spec.status = data.WorkerStatusAvailable
	}
	if spec.maxSlots == 0 {
		spec.maxSlots = 1
	}
	if spec.playwrightVersion == "" {
		spec.playwrightVersion = "1.62.1"
	}
	id := uuid.New()
	_, err := queries.RegisterWorker(t.Context(), data.RegisterWorkerParams{
		ID:                id,
		Address:           "ws://worker-" + id.String() + ":3000",
		Browser:           spec.browser,
		PlaywrightVersion: spec.playwrightVersion,
		MaxSlots:          spec.maxSlots,
		Status:            spec.status,
	})
	if err != nil {
		t.Fatalf("RegisterWorker() returned an error: %v", err)
	}
	_, err = pool.Exec(
		t.Context(),
		`UPDATE workers
SET lifetime_sessions = $2,
    last_heartbeat = now() - $3::bigint * interval '1 microsecond'
WHERE id = $1`,
		id,
		spec.lifetimeSessions,
		spec.heartbeatAge.Microseconds(),
	)
	if err != nil {
		t.Fatalf("updating worker test state: %v", err)
	}
	return id
}

func insertSession(
	t *testing.T,
	pool *pgxpool.Pool,
	queries *data.Queries,
	workerID uuid.UUID,
	status data.SessionStatus,
) uuid.UUID {
	t.Helper()
	worker, err := queries.GetWorker(t.Context(), workerID)
	if err != nil {
		t.Fatalf("GetWorker() returned an error: %v", err)
	}
	id := uuid.New()
	_, err = pool.Exec(
		t.Context(),
		`INSERT INTO sessions (
    id, worker_id, browser, playwright_version, worker_address,
    mode, status, started_at, last_heartbeat
) VALUES (
    $1, $2, $3, $4, $5, 'default', $6,
    CASE WHEN $6::session_status = 'running' THEN now() END,
    now()
)`,
		id,
		workerID,
		worker.Browser,
		worker.PlaywrightVersion,
		worker.Address,
		status,
	)
	if err != nil {
		t.Fatalf("inserting test session: %v", err)
	}
	return id
}

func assertSessionStatus(t *testing.T, queries *data.Queries, id uuid.UUID, want data.SessionStatus) {
	t.Helper()
	session, err := queries.GetSession(t.Context(), id)
	if err != nil {
		t.Fatalf("GetSession(%s) returned an error: %v", id, err)
	}
	if session.Status != want {
		t.Fatalf("session %s status = %q, want %q", id, session.Status, want)
	}
}

func waitForQueueDepth(t *testing.T, scheduler *Scheduler, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for scheduler.QueueDepth() != want {
		if time.Now().After(deadline) {
			t.Fatalf("queue depth = %d, want %d", scheduler.QueueDepth(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func withDSNOption(t *testing.T, dsn, key, value string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parsing test database DSN: %v", err)
	}
	query := parsed.Query()
	query.Set(key, value)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func waitForListenerBackend(t *testing.T, pool *pgxpool.Pool) int32 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var pid int32
		err := pool.QueryRow(
			t.Context(),
			`SELECT pid
FROM pg_stat_activity
WHERE datname = current_database()
  AND query = 'LISTEN capacity_changed'
  AND pid <> pg_backend_pid()
LIMIT 1`,
		).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("finding listener backend: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("listener backend did not appear")
		}
		time.Sleep(time.Millisecond)
	}
}

func truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), "TRUNCATE sessions, api_keys, workers"); err != nil {
		t.Fatalf("truncating test data: %v", err)
	}
}

func newMigratedTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	container, err := postgres.Run(
		t.Context(),
		postgresImage,
		postgres.WithDatabase("server_test"),
		postgres.WithUsername("server_test"),
		postgres.WithPassword("server_test"),
		postgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, container)
	if err != nil {
		t.Fatalf("starting Postgres container: %v", err)
	}
	dsn, err := container.ConnectionString(t.Context(), "sslmode=disable")
	if err != nil {
		t.Fatalf("getting Postgres connection string: %v", err)
	}
	pool, err := db.Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("opening Postgres pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("migrating test database: %v", err)
	}
	return pool
}
