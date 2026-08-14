package db_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"server/internal/db"
	"server/internal/db/data"
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

	var version int64
	err := pool.QueryRow(
		t.Context(),
		"SELECT version_id FROM goose_db_version WHERE is_applied ORDER BY id DESC LIMIT 1",
	).Scan(&version)
	if err != nil {
		t.Fatalf("reading migration version: %v", err)
	}
	if version != 1 {
		t.Fatalf("migration version = %d, want 1", version)
	}
}

func TestQueries(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
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
	if key.ID != keyID || key.Hash != "test-key-hash" || !key.CreatedAt.Valid {
		t.Fatalf("InsertAPIKey() = %+v, want inserted key with a creation time", key)
	}

	activeKey, err := queries.GetActiveAPIKeyByHash(t.Context(), key.Hash)
	if err != nil {
		t.Fatalf("GetActiveAPIKeyByHash() returned an error: %v", err)
	}
	if activeKey.ID != keyID {
		t.Fatalf("GetActiveAPIKeyByHash().ID = %v, want %v", activeKey.ID, keyID)
	}

	touchedAt := now.Add(time.Minute)
	touchedKey, err := queries.TouchAPIKey(t.Context(), data.TouchAPIKeyParams{
		ID:         keyID,
		LastUsedAt: timestamp(touchedAt),
	})
	if err != nil {
		t.Fatalf("TouchAPIKey() returned an error: %v", err)
	}
	assertTime(t, "TouchAPIKey().LastUsedAt", touchedKey.LastUsedAt, touchedAt)

	worker, err := queries.RegisterWorker(t.Context(), data.RegisterWorkerParams{
		ID:                workerID,
		Address:           "ws://worker:3000",
		Browser:           "chromium",
		PlaywrightVersion: "1.58.2",
		MaxSlots:          4,
		Status:            data.WorkerStatusAvailable,
		LastHeartbeat:     timestamp(now),
	})
	if err != nil {
		t.Fatalf("RegisterWorker() returned an error: %v", err)
	}
	if worker.LifetimeSessions != 0 || !worker.CreatedAt.Valid {
		t.Fatalf("RegisterWorker() = %+v, want default lifetime and creation time", worker)
	}

	gotWorker, err := queries.GetWorker(t.Context(), workerID)
	if err != nil {
		t.Fatalf("GetWorker() returned an error: %v", err)
	}
	if gotWorker.Address != worker.Address || gotWorker.Browser != worker.Browser {
		t.Fatalf("GetWorker() = %+v, want address %q and browser %q", gotWorker, worker.Address, worker.Browser)
	}

	heartbeatAt := now.Add(2 * time.Minute)
	gotWorker, err = queries.UpdateWorkerHeartbeat(t.Context(), data.UpdateWorkerHeartbeatParams{
		ID:            workerID,
		LastHeartbeat: timestamp(heartbeatAt),
		Status:        data.WorkerStatusDraining,
	})
	if err != nil {
		t.Fatalf("UpdateWorkerHeartbeat() returned an error: %v", err)
	}
	assertTime(t, "UpdateWorkerHeartbeat().LastHeartbeat", gotWorker.LastHeartbeat, heartbeatAt)
	if gotWorker.Status != data.WorkerStatusDraining {
		t.Fatalf("UpdateWorkerHeartbeat().Status = %q, want %q", gotWorker.Status, data.WorkerStatusDraining)
	}

	gotWorker, err = queries.SetWorkerStatus(t.Context(), data.SetWorkerStatusParams{
		ID:     workerID,
		Status: data.WorkerStatusAvailable,
	})
	if err != nil {
		t.Fatalf("SetWorkerStatus() returned an error: %v", err)
	}
	if gotWorker.Status != data.WorkerStatusAvailable {
		t.Fatalf("SetWorkerStatus().Status = %q, want %q", gotWorker.Status, data.WorkerStatusAvailable)
	}

	workers, err := queries.ListWorkers(t.Context())
	if err != nil {
		t.Fatalf("ListWorkers() returned an error: %v", err)
	}
	if len(workers) != 1 || workers[0].ID != workerID {
		t.Fatalf("ListWorkers() = %+v, want one registered worker", workers)
	}

	metadata := []byte(`{"browserName":"chromium"}`)
	session, err := queries.InsertSession(t.Context(), data.InsertSessionParams{
		ID:              sessionID,
		WorkerID:        workerID,
		Mode:            data.SessionModeDedicated,
		Status:          data.SessionStatusPending,
		CreatedByKey:    keyID,
		ExpiresAt:       timestamp(now.Add(time.Hour)),
		LastHeartbeat:   timestamp(now),
		KeepAliveMs:     pgtype.Int4{Int32: 30_000, Valid: true},
		ConnectMetadata: metadata,
	})
	if err != nil {
		t.Fatalf("InsertSession() returned an error: %v", err)
	}
	if session.ID != sessionID || session.Mode != data.SessionModeDedicated || !session.CreatedAt.Valid {
		t.Fatalf("InsertSession() = %+v, want inserted dedicated session", session)
	}

	gotSession, err := queries.GetSession(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("GetSession() returned an error: %v", err)
	}
	assertJSONEqual(t, "GetSession().ConnectMetadata", gotSession.ConnectMetadata, metadata)

	gotSession, err = queries.SetSessionStatus(t.Context(), data.SetSessionStatusParams{
		ID:     sessionID,
		Status: data.SessionStatusRunning,
	})
	if err != nil {
		t.Fatalf("SetSessionStatus() returned an error: %v", err)
	}
	if gotSession.Status != data.SessionStatusRunning {
		t.Fatalf("SetSessionStatus().Status = %q, want %q", gotSession.Status, data.SessionStatusRunning)
	}

	running, err := queries.CountRunningSessionsByWorker(t.Context(), workerID)
	if err != nil {
		t.Fatalf("CountRunningSessionsByWorker() returned an error: %v", err)
	}
	if running != 1 {
		t.Fatalf("CountRunningSessionsByWorker() = %d, want 1", running)
	}

	renewedAt := now.Add(3 * time.Minute)
	gotSession, err = queries.RenewSessionHeartbeat(t.Context(), data.RenewSessionHeartbeatParams{
		ID:            sessionID,
		LastHeartbeat: timestamp(renewedAt),
	})
	if err != nil {
		t.Fatalf("RenewSessionHeartbeat() returned an error: %v", err)
	}
	assertTime(t, "RenewSessionHeartbeat().LastHeartbeat", gotSession.LastHeartbeat, renewedAt)

	sessions, err := queries.ListSessionsByWorker(t.Context(), workerID)
	if err != nil {
		t.Fatalf("ListSessionsByWorker() returned an error: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != sessionID {
		t.Fatalf("ListSessionsByWorker() = %+v, want one inserted session", sessions)
	}

	revokedAt := now.Add(4 * time.Minute)
	revokedKey, err := queries.RevokeAPIKey(t.Context(), data.RevokeAPIKeyParams{
		ID:        keyID,
		RevokedAt: timestamp(revokedAt),
	})
	if err != nil {
		t.Fatalf("RevokeAPIKey() returned an error: %v", err)
	}
	assertTime(t, "RevokeAPIKey().RevokedAt", revokedKey.RevokedAt, revokedAt)
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
	now := time.Now().UTC().Truncate(time.Microsecond)
	workerID := testUUID(10)

	_, err := queries.RegisterWorker(t.Context(), data.RegisterWorkerParams{
		ID:                workerID,
		Address:           "ws://worker:3000",
		Browser:           "chromium",
		PlaywrightVersion: "1.58.2",
		MaxSlots:          1,
		Status:            data.WorkerStatus("invalid"),
		LastHeartbeat:     timestamp(now),
	})
	assertPGCode(t, "invalid worker status", err, "22P02")

	insertWorker(t, queries, workerID, now)

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
			_, err := queries.InsertSession(t.Context(), data.InsertSessionParams{
				ID:              testUUID(byte(11 + index)),
				WorkerID:        workerID,
				Mode:            test.mode,
				Status:          test.status,
				LastHeartbeat:   timestamp(now),
				ConnectMetadata: []byte(`{}`),
			})
			assertPGCode(t, test.name, err, "22P02")
		})
	}
}

func TestForeignKeys(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	workerID := testUUID(20)
	keyID := testUUID(21)

	_, err := queries.InsertSession(t.Context(), data.InsertSessionParams{
		ID:              testUUID(22),
		WorkerID:        workerID,
		Mode:            data.SessionModeDefault,
		Status:          data.SessionStatusPending,
		LastHeartbeat:   timestamp(now),
		ConnectMetadata: []byte(`{}`),
	})
	assertPGCode(t, "missing worker", err, "23503")

	insertWorker(t, queries, workerID, now)
	_, err = queries.InsertSession(t.Context(), data.InsertSessionParams{
		ID:              testUUID(23),
		WorkerID:        workerID,
		Mode:            data.SessionModeDefault,
		Status:          data.SessionStatusPending,
		CreatedByKey:    keyID,
		LastHeartbeat:   timestamp(now),
		ConnectMetadata: []byte(`{}`),
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
	_, err = queries.InsertSession(t.Context(), data.InsertSessionParams{
		ID:              testUUID(24),
		WorkerID:        workerID,
		Mode:            data.SessionModeDefault,
		Status:          data.SessionStatusPending,
		CreatedByKey:    keyID,
		LastHeartbeat:   timestamp(now),
		ConnectMetadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertSession() returned an error: %v", err)
	}

	assertPGCode(t, "worker delete restriction", queries.DeleteWorker(t.Context(), workerID), "23001")
	_, err = pool.Exec(t.Context(), "DELETE FROM api_keys WHERE id = $1", keyID)
	assertPGCode(t, "api key delete restriction", err, "23503")
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

func insertWorker(t *testing.T, queries *data.Queries, id pgtype.UUID, heartbeat time.Time) {
	t.Helper()

	_, err := queries.RegisterWorker(t.Context(), data.RegisterWorkerParams{
		ID:                id,
		Address:           "ws://worker:3000",
		Browser:           "chromium",
		PlaywrightVersion: "1.58.2",
		MaxSlots:          1,
		Status:            data.WorkerStatusAvailable,
		LastHeartbeat:     timestamp(heartbeat),
	})
	if err != nil {
		t.Fatalf("RegisterWorker() returned an error: %v", err)
	}
}

func testUUID(value byte) pgtype.UUID {
	var bytes [16]byte
	bytes[15] = value
	return pgtype.UUID{Bytes: bytes, Valid: true}
}

func timestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

func assertTime(t *testing.T, name string, actual pgtype.Timestamptz, expected time.Time) {
	t.Helper()
	if !actual.Valid || !actual.Time.Equal(expected) {
		t.Fatalf("%s = %v, want %v", name, actual, expected)
	}
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
