package rescuer

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"server/internal/db"
	"server/internal/db/data"
	"server/internal/scheduler"
)

const postgresImage = "postgres:18-alpine"

func TestRescuer_Sweep(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	rescuer := newTestRescuer(pool)

	staleAvailable := insertWorker(t, pool, queries, data.WorkerStatusAvailable, time.Minute, 1)
	staleDraining := insertWorker(t, pool, queries, data.WorkerStatusDraining, 20*time.Minute, 1)
	shuttingDown := insertWorker(t, pool, queries, data.WorkerStatusShuttingDown, time.Minute, 1)
	deadStalled := insertWorker(t, pool, queries, data.WorkerStatusStalled, 20*time.Minute, 1)
	busyStalled := insertWorker(t, pool, queries, data.WorkerStatusStalled, 20*time.Minute, 1)
	busySession := insertSession(t, queries, busyStalled, data.SessionStatusRunning)

	activeWorker := insertWorker(t, pool, queries, data.WorkerStatusAvailable, 0, 1)
	staleRunning := insertSession(t, queries, activeWorker, data.SessionStatusRunning)
	if _, err := pool.Exec(
		t.Context(),
		"UPDATE sessions SET last_heartbeat = now() - interval '2 hours' WHERE id = $1",
		staleRunning,
	); err != nil {
		t.Fatalf("aging running session: %v", err)
	}
	pendingWorker := insertWorker(t, pool, queries, data.WorkerStatusAvailable, 0, 1)
	expiredPending := insertSession(t, queries, pendingWorker, data.SessionStatusPending)
	if _, err := pool.Exec(
		t.Context(),
		"UPDATE sessions SET expires_at = now() - interval '1 second' WHERE id = $1",
		expiredPending,
	); err != nil {
		t.Fatalf("expiring pending session: %v", err)
	}

	summary, err := rescuer.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep() returned an error: %v", err)
	}
	if summary.StalledWorkers != 1 || summary.ExpiredSessions != 2 || summary.RemovedWorkers != 2 {
		t.Fatalf("Sweep() = %+v, want 1 stalled, 2 expired, and 2 removed", summary)
	}
	assertWorkerStatus(t, queries, staleAvailable, data.WorkerStatusStalled)
	assertWorkerStatus(t, queries, staleDraining, data.WorkerStatusDraining)
	assertWorkerMissing(t, queries, shuttingDown)
	assertWorkerMissing(t, queries, deadStalled)
	assertWorkerStatus(t, queries, busyStalled, data.WorkerStatusStalled)
	assertSessionStatus(t, queries, busySession, data.SessionStatusRunning)
	assertSessionStatus(t, queries, staleRunning, data.SessionStatusExpired)
	assertSessionStatus(t, queries, expiredPending, data.SessionStatusExpired)

	claimScheduler := scheduler.New(pool, scheduler.Options{
		WorkerTTL:           time.Hour,
		PendingSessionTTL:   time.Hour,
		MaxLifetimeSessions: 50,
		MaxQueueSize:        0,
		QueueWaitTimeout:    time.Second,
	})
	session, err := claimScheduler.Claim(t.Context(), scheduler.ClaimRequest{Browser: "chromium"})
	if err != nil {
		t.Fatalf("Claim() after rescue returned an error: %v", err)
	}
	if session.WorkerID != activeWorker && session.WorkerID != pendingWorker {
		t.Fatalf("Claim().WorkerID = %s, want a worker whose expired slot was freed", session.WorkerID)
	}
}

func TestRescuer_FullCrashRecoveryLoop(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	insertWorker(t, pool, queries, data.WorkerStatusAvailable, 0, 1)
	claimScheduler := scheduler.New(pool, scheduler.Options{
		WorkerTTL:           time.Hour,
		PendingSessionTTL:   time.Hour,
		MaxLifetimeSessions: 50,
		MaxQueueSize:        1,
		QueueWaitTimeout:    2 * time.Second,
		PollingInterval:     10 * time.Millisecond,
	})
	first, err := claimScheduler.Claim(t.Context(), scheduler.ClaimRequest{Browser: "chromium"})
	if err != nil {
		t.Fatalf("first Claim() returned an error: %v", err)
	}
	if _, err := pool.Exec(
		t.Context(),
		"UPDATE sessions SET last_heartbeat = now() - interval '2 hours' WHERE id = $1",
		first.ID,
	); err != nil {
		t.Fatalf("aging claimed session: %v", err)
	}

	result := make(chan error, 1)
	go func() {
		_, err := claimScheduler.Admit(t.Context(), scheduler.ClaimRequest{Browser: "chromium"})
		result <- err
	}()
	waitForQueueDepth(t, claimScheduler, 1)
	if _, err := newTestRescuer(pool).Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep() returned an error: %v", err)
	}
	if err := <-result; err != nil {
		t.Fatalf("blocked Admit() after rescue returned an error: %v", err)
	}
	assertSessionStatus(t, queries, first.ID, data.SessionStatusExpired)
}

func newTestRescuer(pool *pgxpool.Pool) *Rescuer {
	return New(
		pool,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Options{
			WorkerTTL:        30 * time.Second,
			SessionTTL:       30 * time.Second,
			StalledWorkerTTL: 10 * time.Minute,
			Interval:         time.Second,
		},
	)
}

func insertWorker(
	t *testing.T,
	pool *pgxpool.Pool,
	queries *data.Queries,
	status data.WorkerStatus,
	heartbeatAge time.Duration,
	maxSlots int32,
) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := queries.RegisterWorker(t.Context(), data.RegisterWorkerParams{
		ID:                id,
		Address:           "ws://worker-" + id.String() + ":3000",
		Browser:           "chromium",
		PlaywrightVersion: "1.62.1",
		MaxSlots:          maxSlots,
		Status:            status,
	})
	if err != nil {
		t.Fatalf("RegisterWorker() returned an error: %v", err)
	}
	if _, err := pool.Exec(
		t.Context(),
		"UPDATE workers SET last_heartbeat = now() - $2::bigint * interval '1 microsecond' WHERE id = $1",
		id,
		heartbeatAge.Microseconds(),
	); err != nil {
		t.Fatalf("aging worker heartbeat: %v", err)
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

func assertWorkerStatus(t *testing.T, queries *data.Queries, id uuid.UUID, want data.WorkerStatus) {
	t.Helper()
	worker, err := queries.GetWorker(t.Context(), id)
	if err != nil {
		t.Fatalf("GetWorker(%s) returned an error: %v", id, err)
	}
	if worker.Status != want {
		t.Fatalf("worker %s status = %q, want %q", id, worker.Status, want)
	}
}

func assertWorkerMissing(t *testing.T, queries *data.Queries, id uuid.UUID) {
	t.Helper()
	if _, err := queries.GetWorker(t.Context(), id); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetWorker(%s) error = %v, want %v", id, err, pgx.ErrNoRows)
	}
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

func waitForQueueDepth(t *testing.T, claimScheduler *scheduler.Scheduler, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for claimScheduler.QueueDepth() != want {
		if time.Now().After(deadline) {
			t.Fatalf("queue depth = %d, want %d", claimScheduler.QueueDepth(), want)
		}
		time.Sleep(time.Millisecond)
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
