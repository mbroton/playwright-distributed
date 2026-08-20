package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"server/internal/db"
	"server/internal/db/data"
	"server/internal/db/migrations"
)

const postgresImage = "postgres:18-alpine"

func TestMigrate(t *testing.T) {
	pool := newTestPool(t)

	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() from zero returned an error: %v", err)
	}
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() on a current database returned an error: %v", err)
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.Files)
	if err != nil {
		t.Fatalf("creating migration provider: %v", err)
	}

	sources := provider.ListSources()
	if len(sources) == 0 {
		t.Fatal("embedded migrations are empty")
	}
	var highestVersion int64
	for _, source := range sources {
		if source.Version > highestVersion {
			highestVersion = source.Version
		}
	}

	if _, err := provider.DownTo(t.Context(), 0); err != nil {
		t.Fatalf("DownTo(0) returned an error: %v", err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatalf("Up() after DownTo(0) returned an error: %v", err)
	}

	version, err := provider.GetDBVersion(t.Context())
	if err != nil {
		t.Fatalf("reading migration version: %v", err)
	}
	if version != highestVersion {
		t.Fatalf("migration version = %d, want highest embedded version %d", version, highestVersion)
	}
}

func TestMigrateConcurrent(t *testing.T) {
	const migrationCount = 8

	pool := newTestPool(t)
	ctx := t.Context()
	start := make(chan struct{})
	errs := make(chan error, migrationCount)
	var group sync.WaitGroup
	for range migrationCount {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			errs <- db.Migrate(ctx, pool)
		}()
	}

	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Migrate() returned an error: %v", err)
		}
	}
}

func TestQueries(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	now := time.Now().UTC()
	workerID := testUUID(1)
	keyID := testUUID(2)
	sessionID := testUUID(3)

	key, err := queries.InsertAPIKey(t.Context(), data.InsertAPIKeyParams{
		ID:     keyID,
		Name:   "test key",
		Hash:   "test-key-hash",
		Prefix: "test_",
	})
	if err != nil {
		t.Fatalf("InsertAPIKey() returned an error: %v", err)
	}
	if key.ID != keyID || key.Hash != "test-key-hash" || key.CreatedAt.IsZero() {
		t.Fatalf("InsertAPIKey() = %+v, want inserted key with a creation time", key)
	}

	activeKey, err := queries.GetActiveAPIKeyByHash(t.Context(), key.Hash)
	if err != nil {
		t.Fatalf("GetActiveAPIKeyByHash() returned an error: %v", err)
	}
	if activeKey.ID != keyID {
		t.Fatalf("GetActiveAPIKeyByHash().ID = %v, want %v", activeKey.ID, keyID)
	}

	touchBefore := databaseTime(t, pool)
	if err := queries.TouchAPIKey(t.Context(), keyID); err != nil {
		t.Fatalf("TouchAPIKey() returned an error: %v", err)
	}
	touchAfter := databaseTime(t, pool)
	touchedKey, err := queries.GetActiveAPIKeyByHash(t.Context(), key.Hash)
	if err != nil {
		t.Fatalf("GetActiveAPIKeyByHash() after touch returned an error: %v", err)
	}
	assertOptionalTimeBetween(t, "TouchAPIKey().LastUsedAt", touchedKey.LastUsedAt, touchBefore, touchAfter)
	firstTouch := *touchedKey.LastUsedAt
	if err := queries.TouchAPIKey(t.Context(), keyID); err != nil {
		t.Fatalf("second TouchAPIKey() returned an error: %v", err)
	}
	touchedKey, err = queries.GetActiveAPIKeyByHash(t.Context(), key.Hash)
	if err != nil {
		t.Fatalf("GetActiveAPIKeyByHash() after second touch returned an error: %v", err)
	}
	if !touchedKey.LastUsedAt.Equal(firstTouch) {
		t.Fatalf("second TouchAPIKey() changed last_used_at from %v to %v", firstTouch, touchedKey.LastUsedAt)
	}

	registerBefore := databaseTime(t, pool)
	worker, err := queries.RegisterWorker(t.Context(), data.RegisterWorkerParams{
		ID:                workerID,
		Address:           "ws://worker:3000",
		Browser:           "chromium",
		PlaywrightVersion: "1.58.2",
		MaxSlots:          4,
		Status:            data.WorkerStatusAvailable,
	})
	if err != nil {
		t.Fatalf("RegisterWorker() returned an error: %v", err)
	}
	registerAfter := databaseTime(t, pool)
	assertTimeBetween(t, "RegisterWorker().LastHeartbeat", worker.LastHeartbeat, registerBefore, registerAfter)
	if worker.LifetimeSessions != 0 || worker.CreatedAt.IsZero() {
		t.Fatalf("RegisterWorker() = %+v, want default lifetime and creation time", worker)
	}

	gotWorker, err := queries.GetWorker(t.Context(), workerID)
	if err != nil {
		t.Fatalf("GetWorker() returned an error: %v", err)
	}
	if gotWorker.Address != worker.Address || gotWorker.Browser != worker.Browser {
		t.Fatalf("GetWorker() = %+v, want address %q and browser %q", gotWorker, worker.Address, worker.Browser)
	}

	heartbeatBefore := databaseTime(t, pool)
	gotWorker, err = queries.UpdateWorkerHeartbeat(t.Context(), workerID)
	if err != nil {
		t.Fatalf("UpdateWorkerHeartbeat() returned an error: %v", err)
	}
	heartbeatAfter := databaseTime(t, pool)
	assertTimeBetween(
		t,
		"UpdateWorkerHeartbeat().LastHeartbeat",
		gotWorker.LastHeartbeat,
		heartbeatBefore,
		heartbeatAfter,
	)
	if gotWorker.Status != data.WorkerStatusAvailable {
		t.Fatalf("UpdateWorkerHeartbeat().Status = %q, want unchanged %q", gotWorker.Status, data.WorkerStatusAvailable)
	}
	if _, err := pool.Exec(
		t.Context(),
		"UPDATE workers SET status = 'stalled' WHERE id = $1",
		workerID,
	); err != nil {
		t.Fatalf("setting worker status to stalled: %v", err)
	}
	gotWorker, err = queries.UpdateWorkerHeartbeat(t.Context(), workerID)
	if err != nil {
		t.Fatalf("UpdateWorkerHeartbeat() for stalled worker returned an error: %v", err)
	}
	if gotWorker.Status != data.WorkerStatusAvailable {
		t.Fatalf(
			"UpdateWorkerHeartbeat().Status for stalled worker = %q, want %q",
			gotWorker.Status,
			data.WorkerStatusAvailable,
		)
	}
	if _, err := pool.Exec(
		t.Context(),
		"UPDATE workers SET status = 'stalled' WHERE id = $1",
		workerID,
	); err != nil {
		t.Fatalf("setting worker status to stalled before status transitions: %v", err)
	}
	if _, err := queries.SetWorkerStatus(t.Context(), data.SetWorkerStatusParams{
		ID:     workerID,
		Status: data.WorkerStatusDraining,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("SetWorkerStatus(stalled to draining) error = %v, want %v", err, pgx.ErrNoRows)
	}
	gotWorker, err = queries.SetWorkerStatus(t.Context(), data.SetWorkerStatusParams{
		ID:     workerID,
		Status: data.WorkerStatusShuttingDown,
	})
	if err != nil {
		t.Fatalf("SetWorkerStatus(stalled to shutting_down) returned an error: %v", err)
	}
	if gotWorker.Status != data.WorkerStatusShuttingDown {
		t.Fatalf(
			"SetWorkerStatus(stalled to shutting_down).Status = %q, want %q",
			gotWorker.Status,
			data.WorkerStatusShuttingDown,
		)
	}
	if _, err := pool.Exec(
		t.Context(),
		"UPDATE workers SET status = 'available' WHERE id = $1",
		workerID,
	); err != nil {
		t.Fatalf("resetting worker status to available: %v", err)
	}

	gotWorker, err = queries.SetWorkerStatus(t.Context(), data.SetWorkerStatusParams{
		ID:     workerID,
		Status: data.WorkerStatusDraining,
	})
	if err != nil {
		t.Fatalf("SetWorkerStatus() returned an error: %v", err)
	}
	if gotWorker.Status != data.WorkerStatusDraining {
		t.Fatalf("SetWorkerStatus().Status = %q, want %q", gotWorker.Status, data.WorkerStatusDraining)
	}

	workers, err := queries.ListWorkers(t.Context())
	if err != nil {
		t.Fatalf("ListWorkers() returned an error: %v", err)
	}
	if len(workers) != 1 || workers[0].ID != workerID {
		t.Fatalf("ListWorkers() = %+v, want one registered worker", workers)
	}

	metadata := []byte(`{"browserName":"chromium"}`)
	sessionBefore := databaseTime(t, pool)
	keepAliveMs := int32(30_000)
	mustInsertTestSession(t, pool, testSessionSpec{
		id:                sessionID,
		workerID:          workerID,
		browser:           "chromium",
		playwrightVersion: "1.58.2",
		workerAddress:     "ws://worker:3000",
		mode:              data.SessionModeDedicated,
		status:            data.SessionStatusPending,
		createdByKey:      &keyID,
		expiresAt:         timePointer(now.Add(time.Hour)),
		keepAliveMs:       &keepAliveMs,
		connectMetadata:   metadata,
	})
	sessionAfter := databaseTime(t, pool)
	session, err := queries.GetSession(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("GetSession() after test insert returned an error: %v", err)
	}
	assertTimeBetween(t, "inserted session LastHeartbeat", session.LastHeartbeat, sessionBefore, sessionAfter)
	if session.ID != sessionID || session.Mode != data.SessionModeDedicated || session.CreatedAt.IsZero() {
		t.Fatalf("inserted session = %+v, want dedicated session", session)
	}

	gotSession, err := queries.GetSession(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("GetSession() returned an error: %v", err)
	}
	assertJSONEqual(t, "GetSession().ConnectMetadata", gotSession.ConnectMetadata, metadata)

	gotSession, err = queries.StartSession(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("StartSession() returned an error: %v", err)
	}
	if gotSession.Status != data.SessionStatusRunning {
		t.Fatalf("StartSession().Status = %q, want %q", gotSession.Status, data.SessionStatusRunning)
	}
	if gotSession.StartedAt == nil {
		t.Fatal("StartSession().StartedAt is nil")
	}
	if gotSession.ExpiresAt != nil {
		t.Fatalf("StartSession().ExpiresAt = %v, want nil", gotSession.ExpiresAt)
	}

	_, err = queries.StartSession(t.Context(), uuid.New())
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("StartSession() for missing id error = %v, want %v", err, pgx.ErrNoRows)
	}

	renewBefore := databaseTime(t, pool)
	gotSession, err = queries.RenewSessionHeartbeat(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("RenewSessionHeartbeat() returned an error: %v", err)
	}
	renewAfter := databaseTime(t, pool)
	assertTimeBetween(t, "RenewSessionHeartbeat().LastHeartbeat", gotSession.LastHeartbeat, renewBefore, renewAfter)

	capacityListener, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquiring capacity listener: %v", err)
	}
	defer capacityListener.Release()
	if _, err := capacityListener.Exec(t.Context(), "LISTEN capacity_changed"); err != nil {
		t.Fatalf("listening for capacity changes: %v", err)
	}

	completed, err := queries.CompleteSession(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("CompleteSession() returned an error: %v", err)
	}
	if completed.Status != data.SessionStatusCompleted {
		t.Fatalf("CompleteSession().Status = %q, want %q", completed.Status, data.SessionStatusCompleted)
	}
	notificationCtx, cancelNotification := context.WithTimeout(t.Context(), time.Second)
	defer cancelNotification()
	if _, err := capacityListener.Conn().WaitForNotification(notificationCtx); err != nil {
		t.Fatalf("waiting for CompleteSession() capacity notification: %v", err)
	}
	if _, err := queries.RenewSessionHeartbeat(t.Context(), sessionID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("RenewSessionHeartbeat() for completed session error = %v, want %v", err, pgx.ErrNoRows)
	}

	expiredSessionID := testUUID(4)
	mustInsertTestSession(t, pool, testSessionSpec{
		id:       expiredSessionID,
		workerID: workerID,
		status:   data.SessionStatusExpired,
	})
	if _, err := queries.CompleteSession(t.Context(), expiredSessionID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CompleteSession() for expired session error = %v, want %v", err, pgx.ErrNoRows)
	}
	if _, err := pool.Exec(t.Context(), "DELETE FROM sessions WHERE id = $1", expiredSessionID); err != nil {
		t.Fatalf("deleting expired transition test session: %v", err)
	}

	sessions, err := queries.ListSessionsByWorker(t.Context(), data.ListSessionsByWorkerParams{
		WorkerID:   workerID,
		PageSize:   100,
		PageOffset: 0,
	})
	if err != nil {
		t.Fatalf("ListSessionsByWorker() returned an error: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != sessionID {
		t.Fatalf("ListSessionsByWorker() = %+v, want one inserted session", sessions)
	}

	revokeBefore := databaseTime(t, pool)
	revokedKey, err := queries.RevokeAPIKey(t.Context(), keyID)
	if err != nil {
		t.Fatalf("RevokeAPIKey() returned an error: %v", err)
	}
	revokeAfter := databaseTime(t, pool)
	assertOptionalTimeBetween(t, "RevokeAPIKey().RevokedAt", revokedKey.RevokedAt, revokeBefore, revokeAfter)
	if _, err := queries.GetActiveAPIKeyByHash(t.Context(), key.Hash); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetActiveAPIKeyByHash() after revoke error = %v, want %v", err, pgx.ErrNoRows)
	}

	if _, err := pool.Exec(t.Context(), "DELETE FROM sessions WHERE id = $1", sessionID); err != nil {
		t.Fatalf("deleting test session: %v", err)
	}
	if err := queries.DeleteWorker(t.Context(), workerID); err != nil {
		t.Fatalf("DeleteWorker() returned an error: %v", err)
	}
	if _, err := queries.GetWorker(t.Context(), workerID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetWorker() after delete error = %v, want %v", err, pgx.ErrNoRows)
	}
}

func TestEnumConstraints(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	workerID := testUUID(10)

	_, err := queries.RegisterWorker(t.Context(), data.RegisterWorkerParams{
		ID:                workerID,
		Address:           "ws://worker:3000",
		Browser:           "chromium",
		PlaywrightVersion: "1.58.2",
		MaxSlots:          1,
		Status:            data.WorkerStatus("invalid"),
	})
	assertPGCode(t, "invalid worker status", err, "22P02")

	insertWorker(t, queries, workerID)

	tests := []struct {
		name   string
		mode   data.SessionMode
		status data.SessionStatus
	}{
		{
			name:   "invalid session mode",
			mode:   data.SessionMode("invalid"),
			status: data.SessionStatusPending,
		},
		{
			name:   "invalid session status",
			mode:   data.SessionModeDefault,
			status: data.SessionStatus("invalid"),
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := insertTestSession(t, pool, testSessionSpec{
				id:       testUUID(byte(11 + index)),
				workerID: workerID,
				mode:     test.mode,
				status:   test.status,
			})
			assertPGCode(t, test.name, err, "22P02")
		})
	}
}

func TestForeignKeys(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	workerID := testUUID(20)
	keyID := testUUID(21)
	missingWorkerSessionID := testUUID(22)
	keySessionID := testUUID(24)

	err := insertTestSession(t, pool, testSessionSpec{
		id:                missingWorkerSessionID,
		workerID:          workerID,
		browser:           "firefox",
		playwrightVersion: "1.57.0",
		workerAddress:     "ws://removed-worker:3000",
		status:            data.SessionStatusCompleted,
	})
	if err != nil {
		t.Fatalf("inserting session with missing worker: %v", err)
	}

	insertWorker(t, queries, workerID)
	err = insertTestSession(t, pool, testSessionSpec{
		id:           testUUID(23),
		workerID:     workerID,
		status:       data.SessionStatusPending,
		createdByKey: &keyID,
	})
	assertPGCode(t, "missing api key", err, "23503")

	_, err = queries.InsertAPIKey(t.Context(), data.InsertAPIKeyParams{
		ID:     keyID,
		Name:   "foreign key test",
		Hash:   "foreign-key-test-hash",
		Prefix: "fk_",
	})
	if err != nil {
		t.Fatalf("InsertAPIKey() returned an error: %v", err)
	}
	err = insertTestSession(t, pool, testSessionSpec{
		id:           keySessionID,
		workerID:     workerID,
		status:       data.SessionStatusPending,
		createdByKey: &keyID,
	})
	if err != nil {
		t.Fatalf("inserting session with API key: %v", err)
	}

	if err := queries.DeleteWorker(t.Context(), workerID); err != nil {
		t.Fatalf("DeleteWorker() with sessions returned an error: %v", err)
	}

	tests := []struct {
		name              string
		id                uuid.UUID
		browser           string
		playwrightVersion string
		workerAddress     string
	}{
		{
			name:              "session inserted before worker registration",
			id:                missingWorkerSessionID,
			browser:           "firefox",
			playwrightVersion: "1.57.0",
			workerAddress:     "ws://removed-worker:3000",
		},
		{
			name:              "session created by api key",
			id:                keySessionID,
			browser:           "chromium",
			playwrightVersion: "1.58.2",
			workerAddress:     "ws://worker:3000",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session, err := queries.GetSession(t.Context(), test.id)
			if err != nil {
				t.Fatalf("GetSession() after worker deletion returned an error: %v", err)
			}
			if session.WorkerID != workerID ||
				session.Browser != test.browser ||
				session.PlaywrightVersion != test.playwrightVersion ||
				session.WorkerAddress != test.workerAddress {
				t.Fatalf("GetSession() after worker deletion = %+v, want intact worker history", session)
			}
		})
	}

	_, err = pool.Exec(t.Context(), "DELETE FROM api_keys WHERE id = $1", keyID)
	assertPGCode(t, "api key delete restriction", err, "23001")
}

func TestCheckConstraints(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	workerID := testUUID(30)
	insertWorker(t, queries, workerID)

	_, err := pool.Exec(
		t.Context(),
		"UPDATE workers SET lifetime_sessions = -1 WHERE id = $1",
		workerID,
	)
	assertPGCode(t, "negative worker lifetime sessions", err, "23514")

	negativeKeepAlive := int32(-1)
	err = insertTestSession(t, pool, testSessionSpec{
		id:          testUUID(31),
		workerID:    workerID,
		status:      data.SessionStatusPending,
		keepAliveMs: &negativeKeepAlive,
	})
	assertPGCode(t, "negative session keep alive", err, "23514")

	err = insertTestSession(t, pool, testSessionSpec{
		id:       testUUID(32),
		workerID: workerID,
		browser:  "invalid",
		status:   data.SessionStatusPending,
	})
	assertPGCode(t, "invalid session browser", err, "23514")

	_, err = pool.Exec(
		t.Context(),
		`INSERT INTO sessions (
    id, worker_id, browser, playwright_version, worker_address,
    mode, status, last_heartbeat
) VALUES ($1, $2, 'chromium', '1.58.2', 'ws://worker:3000', 'default', 'running', now())`,
		testUUID(33),
		workerID,
	)
	assertPGCode(t, "running session without started_at", err, "23514")
}

func TestSessionDefaultMetadata(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	workerID := testUUID(40)
	sessionID := testUUID(41)
	insertWorker(t, queries, workerID)

	err := insertTestSession(t, pool, testSessionSpec{
		id:       sessionID,
		workerID: workerID,
		status:   data.SessionStatusPending,
	})
	if err != nil {
		t.Fatalf("inserting session with nil metadata: %v", err)
	}

	session, err := queries.GetSession(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("GetSession() returned an error: %v", err)
	}
	assertJSONEqual(t, "GetSession().ConnectMetadata", session.ConnectMetadata, []byte(`{}`))
}

func TestInsertAPIKeyDuplicateHash(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	const hash = "duplicate-key-hash"

	_, err := queries.InsertAPIKey(t.Context(), data.InsertAPIKeyParams{
		ID:     testUUID(50),
		Name:   "first key",
		Hash:   hash,
		Prefix: "first_",
	})
	if err != nil {
		t.Fatalf("first InsertAPIKey() returned an error: %v", err)
	}

	_, err = queries.InsertAPIKey(t.Context(), data.InsertAPIKeyParams{
		ID:     testUUID(51),
		Name:   "duplicate key",
		Hash:   hash,
		Prefix: "duplicate_",
	})
	assertPGCode(t, "duplicate api key hash", err, "23505")
}

func TestCountRunningSessionsByWorker(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	workerA := testUUID(60)
	workerB := testUUID(61)
	insertWorker(t, queries, workerA)
	insertWorker(t, queries, workerB)

	mustInsertTestSession(t, pool, testSessionSpec{id: testUUID(62), workerID: workerA, status: data.SessionStatusRunning})
	mustInsertTestSession(t, pool, testSessionSpec{id: testUUID(63), workerID: workerA, status: data.SessionStatusCompleted})
	mustInsertTestSession(t, pool, testSessionSpec{id: testUUID(64), workerID: workerB, status: data.SessionStatusRunning})

	countA, err := queries.CountRunningSessionsByWorker(t.Context(), workerA)
	if err != nil {
		t.Fatalf("CountRunningSessionsByWorker(worker A) returned an error: %v", err)
	}
	if countA != 1 {
		t.Fatalf("CountRunningSessionsByWorker(worker A) = %d, want 1", countA)
	}

	countB, err := queries.CountRunningSessionsByWorker(t.Context(), workerB)
	if err != nil {
		t.Fatalf("CountRunningSessionsByWorker(worker B) returned an error: %v", err)
	}
	if countB != 1 {
		t.Fatalf("CountRunningSessionsByWorker(worker B) = %d, want 1", countB)
	}
}

func TestOpen_RequiresOperationalPoolHeadroom(t *testing.T) {
	_, err := db.Open(
		t.Context(),
		"postgres://server_test:server_test@localhost/server_test?pool_max_conns=1",
	)
	if err == nil || !strings.Contains(err.Error(), "max_conns must be at least 2 for operational headroom") {
		t.Fatalf("Open() error = %v, want minimum pool size error", err)
	}
}

func newTestPool(t *testing.T) *pgxpool.Pool {
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

	return pool
}

func newMigratedTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool := newTestPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("migrating test database: %v", err)
	}
	return pool
}

func insertWorker(t *testing.T, queries *data.Queries, id uuid.UUID) {
	t.Helper()

	_, err := queries.RegisterWorker(t.Context(), data.RegisterWorkerParams{
		ID:                id,
		Address:           "ws://worker:3000",
		Browser:           "chromium",
		PlaywrightVersion: "1.58.2",
		MaxSlots:          1,
		Status:            data.WorkerStatusAvailable,
	})
	if err != nil {
		t.Fatalf("RegisterWorker() returned an error: %v", err)
	}
}

type testSessionSpec struct {
	id                uuid.UUID
	workerID          uuid.UUID
	browser           string
	playwrightVersion string
	workerAddress     string
	mode              data.SessionMode
	status            data.SessionStatus
	createdByKey      *uuid.UUID
	expiresAt         *time.Time
	keepAliveMs       *int32
	connectMetadata   []byte
}

func insertTestSession(t *testing.T, pool *pgxpool.Pool, spec testSessionSpec) error {
	t.Helper()
	if spec.browser == "" {
		spec.browser = "chromium"
	}
	if spec.playwrightVersion == "" {
		spec.playwrightVersion = "1.58.2"
	}
	if spec.workerAddress == "" {
		spec.workerAddress = "ws://worker:3000"
	}
	if spec.mode == "" {
		spec.mode = data.SessionModeDefault
	}

	_, err := pool.Exec(
		t.Context(),
		`INSERT INTO sessions (
    id, worker_id, browser, playwright_version, worker_address, mode, status,
    created_by_key, started_at, expires_at, last_heartbeat, keep_alive_ms,
    connect_metadata
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    CASE WHEN $7::session_status = 'running' THEN now() END,
    $9, now(), $10, COALESCE($11::jsonb, '{}'::jsonb)
)`,
		spec.id,
		spec.workerID,
		spec.browser,
		spec.playwrightVersion,
		spec.workerAddress,
		spec.mode,
		spec.status,
		spec.createdByKey,
		spec.expiresAt,
		spec.keepAliveMs,
		spec.connectMetadata,
	)
	return err
}

func mustInsertTestSession(t *testing.T, pool *pgxpool.Pool, spec testSessionSpec) {
	t.Helper()
	if err := insertTestSession(t, pool, spec); err != nil {
		t.Fatalf("inserting test session: %v", err)
	}
}

func testUUID(value byte) uuid.UUID {
	var id uuid.UUID
	id[15] = value
	return id
}

func databaseTime(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()

	var now time.Time
	if err := pool.QueryRow(t.Context(), "SELECT now()").Scan(&now); err != nil {
		t.Fatalf("reading database time: %v", err)
	}
	return now
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func assertTimeBetween(t *testing.T, name string, actual, before, after time.Time) {
	t.Helper()
	if actual.Before(before) || actual.After(after) {
		t.Fatalf("%s = %v, want a time from %v through %v", name, actual, before, after)
	}
}

func assertOptionalTimeBetween(t *testing.T, name string, actual *time.Time, before, after time.Time) {
	t.Helper()
	if actual == nil {
		t.Fatalf("%s is nil, want a time from %v through %v", name, before, after)
	}
	assertTimeBetween(t, name, *actual, before, after)
}

func assertJSONEqual(t *testing.T, name string, actual, expected []byte) {
	t.Helper()

	var actualValue any
	if err := json.Unmarshal(actual, &actualValue); err != nil {
		t.Fatalf("decoding %s: %v", name, err)
	}
	var expectedValue any
	if err := json.Unmarshal(expected, &expectedValue); err != nil {
		t.Fatalf("decoding expected %s: %v", name, err)
	}
	if !reflect.DeepEqual(actualValue, expectedValue) {
		t.Fatalf("%s = %s, want %s", name, actual, expected)
	}
}

func assertPGCode(t *testing.T, name string, err error, expected string) {
	t.Helper()
	postgresError, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		t.Fatalf("%s error = %v, want PostgreSQL code %s", name, err, expected)
	}
	if postgresError.Code != expected {
		t.Fatalf("%s PostgreSQL code = %s, want %s", name, postgresError.Code, expected)
	}
}
