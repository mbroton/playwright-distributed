package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"server/internal/db"
	"server/internal/db/data"
	"server/internal/scheduler"
)

const postgresImage = "postgres:18-alpine"

type staticAuthenticator struct {
	principal Principal
}

type failingExecDB struct {
	data.DBTX
	err error
}

func (db failingExecDB) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, db.err
}

type hijackingResponseWriter struct {
	http.ResponseWriter
	err    error
	called bool
}

func (w *hijackingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.called = true
	return nil, nil, w.err
}

func (a staticAuthenticator) Authenticate(context.Context, string) (Principal, error) {
	return a.principal, nil
}

func TestServer_WorkerAndSessionRoutes(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	logger := testLogger(io.Discard)
	server := New(pool, queries, NewTokenAuthenticator(queries, logger), logger)

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
	if strings.Contains(list.Body.String(), "$schema") || list.Header().Get("Link") != "" {
		t.Fatalf("list response contains a schema link: headers=%v body=%s", list.Header(), list.Body.String())
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
		Status          data.WorkerStatus `json:"status"`
		Commands        []string          `json:"commands"`
		StaleSessionIDs []uuid.UUID       `json:"stale_session_ids"`
	}
	decodeJSON(t, heartbeat.Body.Bytes(), &heartbeatBody)
	if heartbeatBody.Status != data.WorkerStatusAvailable ||
		heartbeatBody.Commands == nil ||
		len(heartbeatBody.Commands) != 0 ||
		heartbeatBody.StaleSessionIDs == nil {
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
	if _, err := pool.Exec(
		t.Context(),
		"UPDATE workers SET status = 'stalled' WHERE id = $1",
		worker.ID,
	); err != nil {
		t.Fatalf("setting worker status to stalled: %v", err)
	}
	recovered := requestJSON(
		t,
		server.Handler,
		http.MethodPost,
		"/internal/workers/"+worker.ID.String()+"/heartbeat",
		map[string]any{"active_session_ids": []string{}},
		"",
	)
	if recovered.Code != http.StatusOK {
		t.Fatalf("stalled worker heartbeat status = %d, want %d: %s", recovered.Code, http.StatusOK, recovered.Body.String())
	}
	decodeJSON(t, recovered.Body.Bytes(), &heartbeatBody)
	if heartbeatBody.Status != data.WorkerStatusAvailable {
		t.Fatalf("stalled worker heartbeat status = %q, want %q", heartbeatBody.Status, data.WorkerStatusAvailable)
	}

	missingHeartbeat := requestJSON(
		t,
		server.Handler,
		http.MethodPost,
		"/internal/workers/"+uuid.NewString()+"/heartbeat",
		map[string]any{"active_session_ids": []string{}},
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
	shuttingDown := requestJSON(
		t,
		server.Handler,
		http.MethodPost,
		"/internal/workers/"+worker.ID.String()+"/status",
		map[string]any{"status": "shutting_down"},
		"",
	)
	if shuttingDown.Code != http.StatusOK {
		t.Fatalf("shutting-down transition status = %d, want %d: %s", shuttingDown.Code, http.StatusOK, shuttingDown.Body.String())
	}
	invalidTransition := requestJSON(
		t,
		server.Handler,
		http.MethodPost,
		"/internal/workers/"+worker.ID.String()+"/status",
		map[string]any{"status": "draining"},
		"",
	)
	if invalidTransition.Code != http.StatusConflict {
		t.Fatalf(
			"invalid status transition status = %d, want %d: %s",
			invalidTransition.Code,
			http.StatusConflict,
			invalidTransition.Body.String(),
		)
	}
	if _, err := pool.Exec(
		t.Context(),
		"UPDATE workers SET status = 'stalled' WHERE id = $1",
		worker.ID,
	); err != nil {
		t.Fatalf("setting worker status to stalled before API transitions: %v", err)
	}
	stalledToDraining := requestJSON(
		t,
		server.Handler,
		http.MethodPost,
		"/internal/workers/"+worker.ID.String()+"/status",
		map[string]any{"status": "draining"},
		"",
	)
	if stalledToDraining.Code != http.StatusConflict {
		t.Fatalf(
			"stalled-to-draining status = %d, want %d: %s",
			stalledToDraining.Code,
			http.StatusConflict,
			stalledToDraining.Body.String(),
		)
	}
	stalledToShuttingDown := requestJSON(
		t,
		server.Handler,
		http.MethodPost,
		"/internal/workers/"+worker.ID.String()+"/status",
		map[string]any{"status": "shutting_down"},
		"",
	)
	if stalledToShuttingDown.Code != http.StatusOK {
		t.Fatalf(
			"stalled-to-shutting-down status = %d, want %d: %s",
			stalledToShuttingDown.Code,
			http.StatusOK,
			stalledToShuttingDown.Body.String(),
		)
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
	var logs bytes.Buffer
	logger := testLogger(&logs)
	authenticator := NewTokenAuthenticator(queries, logger)
	server := New(pool, queries, authenticator, logger)

	open := requestJSON(t, server.Handler, http.MethodGet, "/v1/workers", nil, "")
	if open.Code != http.StatusOK {
		t.Fatalf("request with no active keys status = %d, want %d", open.Code, http.StatusOK)
	}
	secondOpen := requestJSON(t, server.Handler, http.MethodGet, "/v1/workers", nil, "")
	if secondOpen.Code != http.StatusOK {
		t.Fatalf("second request with no active keys status = %d, want %d", secondOpen.Code, http.StatusOK)
	}
	if count := strings.Count(logs.String(), "authentication disabled"); count != 1 {
		t.Fatalf("unauthenticated warning count = %d, want 1: %s", count, logs.String())
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
	principal, err := authenticator.Authenticate(t.Context(), "Bearer "+token)
	if err != nil {
		t.Fatalf("Authenticate() returned an error: %v", err)
	}
	if principal.KeyID == nil || *principal.KeyID != key.ID {
		t.Fatalf("Authenticate() principal = %+v, want key ID %s", principal, key.ID)
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
	secondKey, err := queries.GetActiveAPIKeyByHash(t.Context(), hashToken(secondToken))
	if err != nil {
		t.Fatalf("GetActiveAPIKeyByHash(second key) returned an error: %v", err)
	}
	if _, err := queries.RevokeAPIKey(t.Context(), secondKey.ID); err != nil {
		t.Fatalf("RevokeAPIKey(second key) returned an error: %v", err)
	}
	stillRequired := requestJSON(t, server.Handler, http.MethodGet, "/v1/workers", nil, "")
	if stillRequired.Code != http.StatusUnauthorized {
		t.Fatalf("request after revoking all keys status = %d, want %d", stillRequired.Code, http.StatusUnauthorized)
	}
}

func TestServer_CreateSessionNeverOverAdmits(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	workerID := uuid.New()
	_, err := queries.RegisterWorker(t.Context(), data.RegisterWorkerParams{
		ID:                workerID,
		Address:           "ws://worker:3000",
		Browser:           "chromium",
		PlaywrightVersion: "1.62.1",
		MaxSlots:          4,
		Status:            data.WorkerStatusAvailable,
	})
	if err != nil {
		t.Fatalf("RegisterWorker() returned an error: %v", err)
	}
	sessionScheduler := newTestScheduler(pool, 0, 250*time.Millisecond)
	server := New(
		pool,
		queries,
		NoAuthAuthenticator{},
		testLogger(io.Discard),
		WithScheduler(sessionScheduler),
	)

	const requestCount = 32
	start := make(chan struct{})
	statuses := make(chan int, requestCount)
	var group sync.WaitGroup
	for range requestCount {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			request := httptest.NewRequest(
				http.MethodPost,
				"/v1/sessions",
				strings.NewReader(`{"browser":"chromium"}`),
			)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			server.Handler.ServeHTTP(response, request)
			statuses <- response.Code
		}()
	}
	close(start)
	group.Wait()
	close(statuses)

	created := 0
	rejected := 0
	for status := range statuses {
		switch status {
		case http.StatusCreated:
			created++
		case http.StatusTooManyRequests:
			rejected++
		default:
			t.Fatalf("concurrent create status = %d, want 201 or 429", status)
		}
	}
	if created != 4 || rejected != 28 {
		t.Fatalf("concurrent results = %d created and %d rejected, want 4 and 28", created, rejected)
	}
	active, err := queries.CountActiveSessionsByWorker(t.Context(), workerID)
	if err != nil {
		t.Fatalf("CountActiveSessionsByWorker() returned an error: %v", err)
	}
	if active > 4 {
		t.Fatalf("active session count = %d, want at most 4", active)
	}
}

func TestServer_CreateSessionAdmissionAndAttribution(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)

	t.Run("stamps authenticated api key", func(t *testing.T) {
		truncateTables(t, pool)
		const token = "pwd_session-owner-token"
		key := insertAPIKey(t, queries, "session owner", token)
		insertTestWorker(t, queries, data.WorkerStatusAvailable, 1)
		logger := testLogger(io.Discard)
		server := New(
			pool,
			queries,
			NewTokenAuthenticator(queries, logger),
			logger,
			WithScheduler(newTestScheduler(pool, 0, time.Second)),
		)
		response := requestJSON(t, server.Handler, http.MethodPost, "/v1/sessions", map[string]any{
			"browser": "chromium",
		}, token)
		if response.Code != http.StatusCreated {
			t.Fatalf("create status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
		}
		var session Session
		decodeJSON(t, response.Body.Bytes(), &session)
		if session.CreatedByKey == nil || *session.CreatedByKey != key.ID {
			t.Fatalf("created_by_key = %v, want %s", session.CreatedByKey, key.ID)
		}
		stored, err := queries.GetSession(t.Context(), session.ID)
		if err != nil {
			t.Fatalf("GetSession() returned an error: %v", err)
		}
		if stored.CreatedByKey == nil || *stored.CreatedByKey != key.ID {
			t.Fatalf("stored created_by_key = %v, want %s", stored.CreatedByKey, key.ID)
		}
	})

	t.Run("rejects unavailable request fields", func(t *testing.T) {
		truncateTables(t, pool)
		insertTestWorker(t, queries, data.WorkerStatusAvailable, 1)
		server := New(
			pool,
			queries,
			NoAuthAuthenticator{},
			testLogger(io.Discard),
			WithScheduler(newTestScheduler(pool, 0, time.Second)),
		)
		dedicated := requestJSON(t, server.Handler, http.MethodPost, "/v1/sessions", map[string]any{
			"browser": "chromium",
			"mode":    "dedicated",
		}, "")
		if dedicated.Code != http.StatusUnprocessableEntity ||
			!strings.Contains(dedicated.Body.String(), "dedicated mode is not available yet") {
			t.Fatalf("dedicated response = %d %s, want 422 dedicated rejection", dedicated.Code, dedicated.Body.String())
		}
		tooLarge := requestJSON(t, server.Handler, http.MethodPost, "/v1/sessions", map[string]any{
			"browser":          "chromium",
			"connect_metadata": map[string]any{"value": strings.Repeat("x", maxConnectMetadataSize)},
		}, "")
		if tooLarge.Code != http.StatusUnprocessableEntity {
			t.Fatalf("large metadata status = %d, want %d: %s", tooLarge.Code, http.StatusUnprocessableEntity, tooLarge.Body.String())
		}
		keepAlive := requestJSON(t, server.Handler, http.MethodPost, "/v1/sessions", map[string]any{
			"browser":       "chromium",
			"keep_alive_ms": 30_000,
		}, "")
		if keepAlive.Code != http.StatusUnprocessableEntity {
			t.Fatalf("keep_alive_ms status = %d, want %d: %s", keepAlive.Code, http.StatusUnprocessableEntity, keepAlive.Body.String())
		}
	})

	t.Run("wait timeout includes retry after", func(t *testing.T) {
		truncateTables(t, pool)
		insertTestWorker(t, queries, data.WorkerStatusAvailable, 1)
		sessionScheduler := newTestScheduler(pool, 1, 30*time.Millisecond)
		if _, err := sessionScheduler.Claim(t.Context(), scheduler.ClaimRequest{Browser: "chromium"}); err != nil {
			t.Fatalf("Claim() returned an error: %v", err)
		}
		server := New(
			pool,
			queries,
			NoAuthAuthenticator{},
			testLogger(io.Discard),
			WithScheduler(sessionScheduler),
		)
		response := requestJSON(t, server.Handler, http.MethodPost, "/v1/sessions", map[string]any{
			"browser": "chromium",
		}, "")
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("queue timeout status = %d, want %d: %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
		}
		if response.Header().Get("Retry-After") == "" {
			t.Fatal("queue timeout response does not contain Retry-After")
		}
	})
}

func TestServer_HeartbeatReconcilesAndSignalsDrain(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	workerID := insertTestWorker(t, queries, data.WorkerStatusAvailable, 2)
	if _, err := pool.Exec(
		t.Context(),
		"UPDATE workers SET lifetime_sessions = 5 WHERE id = $1",
		workerID,
	); err != nil {
		t.Fatalf("setting worker lifetime: %v", err)
	}
	lostSession := insertTestSession(t, queries, workerID, data.SessionStatusRunning)
	if _, err := pool.Exec(
		t.Context(),
		"UPDATE sessions SET created_at = now() - interval '20 seconds' WHERE id = $1",
		lostSession,
	); err != nil {
		t.Fatalf("aging lost session: %v", err)
	}
	zombie := uuid.New()
	sessionScheduler := scheduler.New(pool, scheduler.Options{
		WorkerTTL:           time.Hour,
		PendingSessionTTL:   time.Hour,
		MaxLifetimeSessions: 5,
		MaxQueueSize:        0,
		QueueWaitTimeout:    time.Second,
	})
	server := New(
		pool,
		queries,
		NoAuthAuthenticator{},
		testLogger(io.Discard),
		WithScheduler(sessionScheduler),
	)
	response := requestJSON(
		t,
		server.Handler,
		http.MethodPost,
		"/internal/workers/"+workerID.String()+"/heartbeat",
		map[string]any{"active_session_ids": []uuid.UUID{zombie}},
		"",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	var body struct {
		Status          data.WorkerStatus `json:"status"`
		StaleSessionIDs []uuid.UUID       `json:"stale_session_ids"`
	}
	decodeJSON(t, response.Body.Bytes(), &body)
	if body.Status != data.WorkerStatusDraining ||
		len(body.StaleSessionIDs) != 1 ||
		body.StaleSessionIDs[0] != zombie {
		t.Fatalf("heartbeat response = %+v, want draining with zombie %s", body, zombie)
	}
	stored, err := queries.GetSession(t.Context(), lostSession)
	if err != nil {
		t.Fatalf("GetSession() returned an error: %v", err)
	}
	if stored.Status != data.SessionStatusFailed {
		t.Fatalf("lost session status = %q, want failed", stored.Status)
	}
}

func TestServer_Capacity(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	full := insertTestWorker(t, queries, data.WorkerStatusAvailable, 2)
	free := insertTestWorker(t, queries, data.WorkerStatusAvailable, 3)
	draining := insertTestWorker(t, queries, data.WorkerStatusDraining, 10)
	insertTestSession(t, queries, full, data.SessionStatusPending)
	insertTestSession(t, queries, full, data.SessionStatusRunning)
	insertTestSession(t, queries, free, data.SessionStatusRunning)
	insertTestSession(t, queries, draining, data.SessionStatusRunning)
	server := New(
		pool,
		queries,
		NoAuthAuthenticator{},
		testLogger(io.Discard),
		WithScheduler(newTestScheduler(pool, 100, time.Second)),
	)

	response := requestJSON(t, server.Handler, http.MethodGet, "/v1/capacity", nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("capacity status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	var capacity Capacity
	decodeJSON(t, response.Body.Bytes(), &capacity)
	if len(capacity.Browsers) != 1 {
		t.Fatalf("capacity browsers = %+v, want one browser", capacity.Browsers)
	}
	want := BrowserCapacity{
		Browser:        "chromium",
		Workers:        2,
		MaxSlots:       5,
		ActiveSessions: 4,
		AvailableSlots: 1,
	}
	if capacity.Browsers[0] != want {
		t.Fatalf("browser capacity = %+v, want %+v", capacity.Browsers[0], want)
	}
	if capacity.Totals != (CapacityTotals{Workers: 2, MaxSlots: 5, ActiveSessions: 4, AvailableSlots: 1}) ||
		capacity.Queued != 0 || capacity.MaxQueueSize != 100 {
		t.Fatalf("capacity totals and queue = %+v, want fleet totals and replica queue", capacity)
	}
}

func TestServer_TouchFailureDoesNotRejectRequest(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	const token = "pwd_touch-failure-token"
	insertAPIKey(t, queries, "touch failure", token)
	errTouch := errors.New("touch failed")
	var logs bytes.Buffer
	failingTouchQueries := data.New(failingExecDB{DBTX: pool, err: errTouch})
	authenticator := NewTokenAuthenticator(
		failingTouchQueries,
		testLogger(&logs),
	)
	server := New(pool, failingTouchQueries, authenticator, testLogger(io.Discard))

	response := requestJSON(t, server.Handler, http.MethodGet, "/v1/workers", nil, token)
	if response.Code != http.StatusOK {
		t.Fatalf("request after touch failure status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if !strings.Contains(logs.String(), "touch api key") || !strings.Contains(logs.String(), errTouch.Error()) {
		t.Fatalf("touch failure log = %q, want warning with touch error", logs.String())
	}
	untouched, err := queries.GetActiveAPIKeyByHash(t.Context(), hashToken(token))
	if err != nil {
		t.Fatalf("GetActiveAPIKeyByHash() returned an error: %v", err)
	}
	if untouched.LastUsedAt != nil {
		t.Fatalf("last_used_at = %v, want nil after failed touch", untouched.LastUsedAt)
	}
}

func TestServer_RejectsHostlessWorkerAddress(t *testing.T) {
	server := New(nil, nil, NoAuthAuthenticator{}, testLogger(io.Discard))
	for _, address := range []string{"ws://", "ws://:3000", "ws://user@"} {
		response := requestJSON(t, server.Handler, http.MethodPost, "/internal/workers", map[string]any{
			"address":            address,
			"browser":            "chromium",
			"playwright_version": "1.62.1",
			"max_slots":          1,
		}, "")
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("address %q status = %d, want %d: %s", address, response.Code, http.StatusUnprocessableEntity, response.Body.String())
		}
	}
}

func TestServer_SecuredRoutesRequireAuthentication(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	logger := testLogger(io.Discard)
	insertAPIKey(t, queries, "route test", "pwd_route-test-token")
	server := New(pool, queries, NewTokenAuthenticator(queries, logger), logger)

	tests := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{
			name:   "get session",
			method: http.MethodGet,
			path:   "/v1/sessions/" + uuid.NewString(),
		},
		{
			name:   "list workers",
			method: http.MethodGet,
			path:   "/v1/workers",
		},
		{
			name:   "register worker",
			method: http.MethodPost,
			path:   "/internal/workers",
			body: map[string]any{
				"address":            "ws://worker:3000",
				"browser":            "chromium",
				"playwright_version": "1.62.1",
				"max_slots":          1,
			},
		},
		{
			name:   "heartbeat worker",
			method: http.MethodPost,
			path:   "/internal/workers/" + uuid.NewString() + "/heartbeat",
			body:   map[string]any{"active_session_ids": []string{}},
		},
		{
			name:   "set worker status",
			method: http.MethodPost,
			path:   "/internal/workers/" + uuid.NewString() + "/status",
			body:   map[string]any{"status": "draining"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := requestJSON(t, server.Handler, test.method, test.path, test.body, "")
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusUnauthorized, response.Body.String())
			}
			if got := response.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Fatalf("WWW-Authenticate = %q, want %q", got, "Bearer")
			}
		})
	}
}

func TestServer_AuthenticationBackendFailure(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	const token = "pwd_backend-failure-token"
	insertAPIKey(t, queries, "backend failure", token)
	var logs bytes.Buffer
	logger := testLogger(&logs)
	server := New(pool, queries, NewTokenAuthenticator(queries, logger), logger)
	pool.Close()

	response := requestJSON(t, server.Handler, http.MethodGet, "/v1/workers", nil, token)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("503 response does not contain Retry-After")
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("authentication backend log contains token: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "authenticate request") {
		t.Fatalf("authentication backend failure was not logged: %s", logs.String())
	}
}

func TestAuthMiddleware_StoresPrincipal(t *testing.T) {
	keyID := uuid.New()
	mux := http.NewServeMux()
	config := huma.DefaultConfig("principal test", "1.0.0")
	config.CreateHooks = nil
	api := humago.New(mux, config)
	group := huma.NewGroup(api, "")
	group.UseMiddleware(authMiddleware(group, staticAuthenticator{
		principal: Principal{KeyID: &keyID},
	}, testLogger(io.Discard)))
	type principalOutput struct {
		Body Principal
	}
	huma.Register(group, huma.Operation{
		OperationID: "get-principal",
		Method:      http.MethodGet,
		Path:        "/principal",
	}, func(ctx context.Context, _ *struct{}) (*principalOutput, error) {
		return &principalOutput{Body: PrincipalFromContext(ctx)}, nil
	})

	response := requestJSON(t, mux, http.MethodGet, "/principal", nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	var principal Principal
	decodeJSON(t, response.Body.Bytes(), &principal)
	if principal.KeyID == nil || *principal.KeyID != keyID {
		t.Fatalf("principal = %+v, want key ID %s", principal, keyID)
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

func TestServer_DisablesHumaRuntimeRoutes(t *testing.T) {
	server := New(nil, nil, NoAuthAuthenticator{}, testLogger(io.Discard))
	paths := []string{"/docs", "/openapi.json", "/openapi.yaml", "/openapi", "/schemas/Worker.json"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			response := requestJSON(t, server.Handler, http.MethodGet, path, nil, "")
			if response.Code != http.StatusNotFound {
				t.Fatalf("%s status = %d, want %d", path, response.Code, http.StatusNotFound)
			}
		})
	}
}

func TestRequestLogger_OmitsQueryStringAndIncludesRemoteAddress(t *testing.T) {
	var logs bytes.Buffer
	logger := testLogger(&logs)
	handler := requestLogger(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), logger)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/workers?token=pwd_secret", nil)
	request.RemoteAddr = "192.0.2.1:1234"
	handler.ServeHTTP(response, request)
	if strings.Contains(logs.String(), "pwd_secret") || strings.Contains(logs.String(), "?token=") {
		t.Fatalf("request log contains query string: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"path":"/v1/workers"`) {
		t.Fatalf("request log does not contain URL path: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"remote_address":"192.0.2.1:1234"`) {
		t.Fatalf("request log does not contain remote address: %s", logs.String())
	}
}

func TestRequestLogger_SkipsHealthProbes(t *testing.T) {
	var logs bytes.Buffer
	handler := requestLogger(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), testLogger(&logs))

	for _, path := range []string{"/healthz", "/readyz"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	if logs.Len() != 0 {
		t.Fatalf("probe requests produced logs: %s", logs.String())
	}
}

func TestLoggingResponseWriter_Hijack(t *testing.T) {
	errHijack := errors.New("hijack failed")
	underlying := &hijackingResponseWriter{
		ResponseWriter: httptest.NewRecorder(),
		err:            errHijack,
	}
	response := &loggingResponseWriter{ResponseWriter: underlying}
	if _, _, err := response.Hijack(); !errors.Is(err, errHijack) {
		t.Fatalf("Hijack() error = %v, want %v", err, errHijack)
	}
	if !underlying.called {
		t.Fatal("Hijack() did not delegate to the underlying response writer")
	}

	unsupported := &loggingResponseWriter{ResponseWriter: httptest.NewRecorder()}
	if _, _, err := unsupported.Hijack(); err == nil {
		t.Fatal("Hijack() with unsupported response writer returned nil error")
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
	committed, err := os.ReadFile("../../openapi.yaml")
	if err != nil {
		t.Fatalf("reading committed OpenAPI document: %v", err)
	}
	if !bytes.Equal(first, committed) {
		t.Fatal("committed openapi.yaml does not match OpenAPISpec() output; run make openapi")
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

func newTestScheduler(
	pool *pgxpool.Pool,
	maxQueueSize int,
	queueWaitTimeout time.Duration,
) *scheduler.Scheduler {
	return scheduler.New(pool, scheduler.Options{
		WorkerTTL:           time.Hour,
		PendingSessionTTL:   time.Hour,
		MaxLifetimeSessions: 50,
		MaxQueueSize:        maxQueueSize,
		QueueWaitTimeout:    queueWaitTimeout,
		PollingInterval:     10 * time.Millisecond,
	})
}

func insertTestWorker(
	t *testing.T,
	queries *data.Queries,
	status data.WorkerStatus,
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
	return id
}

func insertTestSession(
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

func truncateTables(t *testing.T, pool *pgxpool.Pool) {
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

func databaseTime(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(t.Context(), "SELECT now()").Scan(&now); err != nil {
		t.Fatalf("reading database time: %v", err)
	}
	return now
}
