package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"server/internal/db"
	"server/internal/db/data"
)

const postgresImage = "postgres:18-alpine"

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
}

func TestScheduler_HeartbeatReconciliationAndRecycling(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)

	t.Run("reconciles sessions in both directions", func(t *testing.T) {
		truncate(t, pool)
		workerID := insertWorker(t, pool, queries, workerSpec{maxSlots: 5})
		otherWorkerID := insertWorker(t, pool, queries, workerSpec{maxSlots: 1})
		oldLost := insertSession(t, queries, workerID, data.SessionStatusRunning)
		young := insertSession(t, queries, workerID, data.SessionStatusRunning)
		pending := insertSession(t, queries, workerID, data.SessionStatusPending)
		otherSession := insertSession(t, queries, otherWorkerID, data.SessionStatusRunning)
		if _, err := pool.Exec(
			t.Context(),
			"UPDATE sessions SET created_at = now() - interval '20 seconds' WHERE id = $1",
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
}

type workerSpec struct {
	browser          string
	status           data.WorkerStatus
	maxSlots         int32
	lifetimeSessions int64
	heartbeatAge     time.Duration
}

func newTestScheduler(pool *pgxpool.Pool, maxQueueSize int, maxLifetime int64) *Scheduler {
	return New(pool, Options{
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
	id := uuid.New()
	_, err := queries.RegisterWorker(t.Context(), data.RegisterWorkerParams{
		ID:                id,
		Address:           "ws://worker-" + id.String() + ":3000",
		Browser:           spec.browser,
		PlaywrightVersion: "1.62.1",
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
	_, err = queries.InsertSession(t.Context(), data.InsertSessionParams{
		ID:                id,
		WorkerID:          workerID,
		Browser:           worker.Browser,
		PlaywrightVersion: worker.PlaywrightVersion,
		WorkerAddress:     worker.Address,
		Mode:              data.SessionModeDefault,
		Status:            status,
	})
	if err != nil {
		t.Fatalf("InsertSession() returned an error: %v", err)
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
