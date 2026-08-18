package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestServer_WorkerAndSessionRoutes(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	server := New(pool, queries, NewTokenAuthenticator(queries, 0), testLogger(io.Discard))

	register := requestJSON(t, server.Handler, http.MethodPost, "/internal/workers", map[string]any{
		"address":            "ws://worker:3000",
		"browser":            "chromium",
		"playwright_version": "1.62.1",
		"max_slots":          4,
	}, "")
	if register.Code != http.StatusCreated {
		t.Fatalf("register status = %d, want %d: %s", register.Code, http.StatusCreated, register.Body.String())
	}
	var worker Worker
	decodeJSON(t, register.Body.Bytes(), &worker)
	if worker.ID == uuid.Nil || worker.Status != data.WorkerStatusAvailable {
		t.Fatalf("registered worker = %+v, want server ID and available status", worker)
	}

	list := requestJSON(t, server.Handler, http.MethodGet, "/v1/workers", nil, "")
	if list.Code != http.StatusOK {
		t.Fatalf("list workers status = %d, want %d: %s", list.Code, http.StatusOK, list.Body.String())
	}
	var workers []Worker
	decodeJSON(t, list.Body.Bytes(), &workers)
	if len(workers) != 1 || workers[0].ID != worker.ID {
		t.Fatalf("listed workers = %+v, want registered worker %s", workers, worker.ID)
	}

	heartbeatBefore := databaseTime(t, pool)
	heartbeat := requestJSON(
		t,
		server.Handler,
		http.MethodPost,
		"/internal/workers/"+worker.ID.String()+"/heartbeat",
		map[string]any{"active_session_ids": []string{}},
		"",
	)
	if heartbeat.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d, want %d: %s", heartbeat.Code, http.StatusOK, heartbeat.Body.String())
	}
	var heartbeatBody struct {
		Status   data.WorkerStatus `json:"status"`
		Commands []string          `json:"commands"`
	}
	decodeJSON(t, heartbeat.Body.Bytes(), &heartbeatBody)
	if heartbeatBody.Status != data.WorkerStatusAvailable || heartbeatBody.Commands == nil || len(heartbeatBody.Commands) != 0 {
		t.Fatalf("heartbeat response = %+v, want available and empty commands", heartbeatBody)
	}
	heartbeatAfter := databaseTime(t, pool)
	updatedWorker, err := queries.GetWorker(t.Context(), worker.ID)
	if err != nil {
		t.Fatalf("GetWorker() after heartbeat returned an error: %v", err)
	}
	if updatedWorker.LastHeartbeat.Before(heartbeatBefore) || updatedWorker.LastHeartbeat.After(heartbeatAfter) {
		t.Fatalf(
			"heartbeat time = %v, want a database time from %v through %v",
			updatedWorker.LastHeartbeat,
			heartbeatBefore,
			heartbeatAfter,
		)
	}

	missingHeartbeat := requestJSON(
		t,
		server.Handler,
		http.MethodPost,
		"/internal/workers/"+uuid.NewString()+"/heartbeat",
		map[string]any{},
		"",
	)
	if missingHeartbeat.Code != http.StatusNotFound {
		t.Fatalf("missing heartbeat status = %d, want %d", missingHeartbeat.Code, http.StatusNotFound)
	}

	status := requestJSON(
		t,
		server.Handler,
		http.MethodPost,
		"/internal/workers/"+worker.ID.String()+"/status",
		map[string]any{"status": "draining"},
		"",
	)
	if status.Code != http.StatusOK {
		t.Fatalf("status transition status = %d, want %d: %s", status.Code, http.StatusOK, status.Body.String())
	}
	decodeJSON(t, status.Body.Bytes(), &worker)
	if worker.Status != data.WorkerStatusDraining {
		t.Fatalf("worker status = %q, want %q", worker.Status, data.WorkerStatusDraining)
	}

	sessionID := uuid.New()
	_, err = queries.InsertSession(t.Context(), data.InsertSessionParams{
		ID:                sessionID,
		WorkerID:          worker.ID,
		Browser:           worker.Browser,
		PlaywrightVersion: worker.PlaywrightVersion,
		WorkerAddress:     worker.Address,
		Mode:              data.SessionModeDefault,
		Status:            data.SessionStatusRunning,
		ConnectMetadata:   []byte(`{"source":"test"}`),
	})
	if err != nil {
		t.Fatalf("InsertSession() returned an error: %v", err)
	}
	getSession := requestJSON(t, server.Handler, http.MethodGet, "/v1/sessions/"+sessionID.String(), nil, "")
	if getSession.Code != http.StatusOK {
		t.Fatalf("get session status = %d, want %d: %s", getSession.Code, http.StatusOK, getSession.Body.String())
	}
	var session Session
	decodeJSON(t, getSession.Body.Bytes(), &session)
	if session.ID != sessionID || session.ConnectMetadata["source"] != "test" {
		t.Fatalf("session = %+v, want inserted session %s", session, sessionID)
	}
	missingSession := requestJSON(t, server.Handler, http.MethodGet, "/v1/sessions/"+uuid.NewString(), nil, "")
	if missingSession.Code != http.StatusNotFound {
		t.Fatalf("missing session status = %d, want %d", missingSession.Code, http.StatusNotFound)
	}
}

func TestServer_Authentication(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	authenticator := NewTokenAuthenticator(queries, 0)
	server := New(pool, queries, authenticator, testLogger(io.Discard))

	open := requestJSON(t, server.Handler, http.MethodGet, "/v1/workers", nil, "")
	if open.Code != http.StatusOK {
		t.Fatalf("request with no active keys status = %d, want %d", open.Code, http.StatusOK)
	}

	const token = "pwd_right-token-value"
	key := insertAPIKey(t, queries, "right key", token)

	missing := requestJSON(t, server.Handler, http.MethodGet, "/v1/workers", nil, "")
	wrong := requestJSON(t, server.Handler, http.MethodGet, "/v1/workers", nil, "pwd_wrong-token-value")
	if missing.Code != http.StatusUnauthorized || wrong.Code != http.StatusUnauthorized {
		t.Fatalf("auth failure statuses = %d and %d, want %d", missing.Code, wrong.Code, http.StatusUnauthorized)
	}
	if !bytes.Equal(missing.Body.Bytes(), wrong.Body.Bytes()) {
		t.Fatalf("auth failure bodies differ:\nmissing: %s\nwrong: %s", missing.Body.String(), wrong.Body.String())
	}
	if strings.Contains(missing.Body.String(), token) || strings.Contains(wrong.Body.String(), "pwd_wrong") {
		t.Fatal("auth failure body contains token material")
	}

	authorized := requestJSON(t, server.Handler, http.MethodGet, "/v1/workers", nil, token)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, want %d: %s", authorized.Code, http.StatusOK, authorized.Body.String())
	}
	touched, err := queries.GetActiveAPIKeyByHash(t.Context(), hashToken(token))
	if err != nil {
		t.Fatalf("GetActiveAPIKeyByHash() returned an error: %v", err)
	}
	if touched.LastUsedAt == nil {
		t.Fatal("successful authentication did not touch last_used_at")
	}

	const secondToken = "pwd_second-token-value"
	insertAPIKey(t, queries, "second key", secondToken)
	if _, err := queries.RevokeAPIKey(t.Context(), key.ID); err != nil {
		t.Fatalf("RevokeAPIKey() returned an error: %v", err)
	}
	revoked := requestJSON(t, server.Handler, http.MethodGet, "/v1/workers", nil, token)
	if revoked.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key status = %d, want %d", revoked.Code, http.StatusUnauthorized)
	}
	stillAuthorized := requestJSON(t, server.Handler, http.MethodGet, "/v1/workers", nil, secondToken)
	if stillAuthorized.Code != http.StatusOK {
		t.Fatalf("active key status = %d, want %d", stillAuthorized.Code, http.StatusOK)
	}
}

func TestServer_Readiness(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	server := New(pool, queries, NoAuthAuthenticator{}, testLogger(io.Discard))

	ready := requestJSON(t, server.Handler, http.MethodGet, "/readyz", nil, "")
	if ready.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want %d: %s", ready.Code, http.StatusOK, ready.Body.String())
	}

	pool.Close()
	notReady := requestJSON(t, server.Handler, http.MethodGet, "/readyz", nil, "")
	if notReady.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed database readiness status = %d, want %d", notReady.Code, http.StatusServiceUnavailable)
	}
}

func TestRequestLogger_OmitsQueryString(t *testing.T) {
	var logs bytes.Buffer
	logger := testLogger(&logs)
	handler := requestLogger(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), logger)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz?token=pwd_secret", nil))
	if strings.Contains(logs.String(), "pwd_secret") || strings.Contains(logs.String(), "?token=") {
		t.Fatalf("request log contains query string: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"path":"/healthz"`) {
		t.Fatalf("request log does not contain URL path: %s", logs.String())
	}
}

func TestOpenAPISpec_Deterministic(t *testing.T) {
	first, err := OpenAPISpec()
	if err != nil {
		t.Fatalf("first OpenAPISpec() returned an error: %v", err)
	}
	second, err := OpenAPISpec()
	if err != nil {
		t.Fatalf("second OpenAPISpec() returned an error: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("OpenAPISpec() output changed between calls")
	}
}

func requestJSON(
	t *testing.T,
	handler http.Handler,
	method string,
	path string,
	body any,
	token string,
) *httptest.ResponseRecorder {
	t.Helper()

	var bodyReader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding request body: %v", err)
		}
		bodyReader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeJSON(t *testing.T, encoded []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(encoded, target); err != nil {
		t.Fatalf("decoding JSON response %s: %v", encoded, err)
	}
}

func insertAPIKey(t *testing.T, queries *data.Queries, name, token string) data.APIKey {
	t.Helper()
	key, err := queries.InsertAPIKey(t.Context(), data.InsertAPIKeyParams{
		ID:     uuid.New(),
		Name:   name,
		Hash:   hashToken(token),
		Prefix: token[:8],
	})
	if err != nil {
		t.Fatalf("InsertAPIKey() returned an error: %v", err)
	}
	return key
}

func hashToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func testLogger(output io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(output, nil))
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

func databaseTime(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(t.Context(), "SELECT now()").Scan(&now); err != nil {
		t.Fatalf("reading database time: %v", err)
	}
	return now
}
