package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"server/internal/db/data"
	"server/internal/relay"
	"server/internal/rescuer"
	"server/internal/scheduler"
)

const (
	testHeartbeatInterval = 10 * time.Millisecond
	testWriteTimeout      = 5 * time.Second
	testPingInterval      = 200 * time.Millisecond
	testPongTimeout       = 3 * time.Second
)

type echoWorker struct {
	server   *httptest.Server
	headers  chan http.Header
	closes   chan workerClose
	upgrades atomic.Int32
}

type workerClose struct {
	sessionID string
	err       *websocket.CloseError
}

type messageObservingWorker struct {
	server     *httptest.Server
	messages   chan []byte
	readErrors chan error
}

type failingRenewDB struct {
	data.DBTX
	err error
}

func (db failingRenewDB) QueryRow(ctx context.Context, query string, args ...interface{}) pgx.Row {
	if strings.Contains(query, "-- name: RenewSessionHeartbeat :one") {
		return errorRow{err: db.err}
	}
	return db.DBTX.QueryRow(ctx, query, args...)
}

type errorRow struct {
	err error
}

func (row errorRow) Scan(...interface{}) error {
	return row.err
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func TestRelay_ImplicitEndToEnd(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	truncateTables(t, pool)
	worker := newEchoWorker(t)
	workerID := insertRelayWorker(t, queries, worker.wsURL(), "chromium", "1.62.1", 1)
	const token = "pwd_relay-token-secret"
	key := insertAPIKey(t, queries, "relay test", token)
	var logs lockedBuffer
	logger := testLogger(&logs)
	manager := newTestRelayManager(queries, logger, relay.Options{})
	httpServer := newRelayHTTPServer(
		t,
		pool,
		NewTokenAuthenticator(queries, logger),
		manager,
		logger,
		0,
	)

	headers := http.Header{
		"User-Agent":           []string{"Playwright/1.62.1 (x64; linux)"},
		"Cookie":               []string{"secret=cookie"},
		"X-Playwright-Test":    []string{"forward-me"},
		"X-Playwright-Another": []string{"one", "two"},
	}
	client, response, err := websocket.DefaultDialer.Dial(
		wsURL(httpServer.URL)+"/?token="+token+"&browser=chromium",
		headers,
	)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("implicit WebSocket dial returned status %d and error: %v", status, err)
	}
	defer client.Close()

	assertMessage(t, client, []byte("worker-ready"))
	payload := []byte("client-to-worker")
	if err := client.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatalf("writing relay payload: %v", err)
	}
	assertMessage(t, client, payload)

	large := bytes.Repeat([]byte("streamed-message-"), (16*1024*1024)/len("streamed-message-")+1)
	large = large[:16*1024*1024]
	largeResult := make(chan []byte, 1)
	largeError := make(chan error, 1)
	go func() {
		_, message, err := client.ReadMessage()
		if err != nil {
			largeError <- err
			return
		}
		largeResult <- message
	}()
	if err := client.WriteMessage(websocket.BinaryMessage, large); err != nil {
		t.Fatalf("writing large relay payload: %v", err)
	}
	select {
	case got := <-largeResult:
		if !bytes.Equal(got, large) {
			t.Fatalf("large relayed message length = %d, want %d", len(got), len(large))
		}
	case err := <-largeError:
		t.Fatalf("reading large relayed payload: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("large relayed payload did not arrive before timeout")
	}

	session := latestSession(t, pool, queries)
	if session.Status != data.SessionStatusRunning {
		t.Fatalf("live session status = %q, want %q", session.Status, data.SessionStatusRunning)
	}
	firstHeartbeat := session.LastHeartbeat
	waitForSession(t, queries, session.ID, func(current data.Session) bool {
		return current.LastHeartbeat.After(firstHeartbeat)
	})

	if err := client.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"),
		time.Now().Add(time.Second),
	); err != nil {
		t.Fatalf("closing client WebSocket: %v", err)
	}
	workerClose := receiveClose(t, worker, session.ID)
	if workerClose.Code != websocket.CloseNormalClosure || workerClose.Text != "done" {
		t.Fatalf("worker close = %d %q, want 1000 %q", workerClose.Code, workerClose.Text, "done")
	}
	waitForSession(t, queries, session.ID, func(current data.Session) bool {
		return current.Status == data.SessionStatusCompleted
	})
	completed, err := queries.GetSession(t.Context(), session.ID)
	if err != nil {
		t.Fatalf("GetSession() after relay close returned an error: %v", err)
	}
	if completed.WorkerID != workerID || completed.Browser != "chromium" ||
		completed.PlaywrightVersion != "1.62.1" || completed.WorkerAddress != worker.wsURL() {
		t.Fatalf("session denormalized fields = %+v, want worker registration values", completed)
	}
	if completed.CreatedByKey == nil || *completed.CreatedByKey != key.ID {
		t.Fatalf("session created_by_key = %v, want %s", completed.CreatedByKey, key.ID)
	}

	forwarded := receiveHeaders(t, worker.headers)
	if forwarded.Get("User-Agent") != headers.Get("User-Agent") ||
		forwarded.Get("X-Playwright-Test") != "forward-me" ||
		len(forwarded.Values("X-Playwright-Another")) != 2 {
		t.Fatalf("worker headers = %v, want forwarded Playwright headers", forwarded)
	}
	if forwarded.Get("Authorization") != "" || forwarded.Get("Cookie") != "" {
		t.Fatalf("worker received secret headers: %v", forwarded)
	}
	if forwarded.Get("x-pwd-session-id") != session.ID.String() {
		t.Fatalf("x-pwd-session-id = %q, want %s", forwarded.Get("x-pwd-session-id"), session.ID)
	}
	waitForCondition(t, time.Second, func() bool { return strings.Contains(logs.String(), "http request") })
	if !strings.Contains(logs.String(), `"status":101`) {
		t.Fatalf("relay request log does not contain status 101: %s", logs.String())
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), "?token=") {
		t.Fatalf("relay logs contain token material: %s", logs.String())
	}
}

func TestRelay_HTTPPreflightAndAuth(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)

	t.Run("root non-upgrade and unmatched path", func(t *testing.T) {
		truncateTables(t, pool)
		manager := newTestRelayManager(queries, testLogger(io.Discard), relay.Options{})
		server := newRelayHTTPServer(
			t,
			pool,
			NoAuthAuthenticator{},
			manager,
			testLogger(io.Discard),
			0,
		)
		response, err := http.Get(server.URL + "/")
		if err != nil {
			t.Fatalf("GET / returned an error: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusUpgradeRequired {
			t.Fatalf("GET / status = %d, want %d", response.StatusCode, http.StatusUpgradeRequired)
		}
		missing, err := http.Get(server.URL + "/not-a-route")
		if err != nil {
			t.Fatalf("GET unmatched path returned an error: %v", err)
		}
		defer missing.Body.Close()
		if missing.StatusCode != http.StatusNotFound {
			t.Fatalf("unmatched status = %d, want %d", missing.StatusCode, http.StatusNotFound)
		}
		invalid, err := http.Get(server.URL + "/?browser=chrome")
		if err != nil {
			t.Fatalf("GET invalid browser returned an error: %v", err)
		}
		defer invalid.Body.Close()
		if invalid.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid browser status = %d, want %d", invalid.StatusCode, http.StatusBadRequest)
		}
	})

	t.Run("authentication carriers and origin", func(t *testing.T) {
		truncateTables(t, pool)
		worker := newEchoWorker(t)
		insertRelayWorker(t, queries, worker.wsURL(), "chromium", "1.62.1", 4)
		const token = "pwd_auth-relay-token"
		insertAPIKey(t, queries, "relay auth", token)
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{})
		server := newRelayHTTPServer(
			t,
			pool,
			NewTokenAuthenticator(queries, logger),
			manager,
			logger,
			0,
		)

		_, response, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/?token=pwd_bad", nil)
		if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("bad token dial response = %v, error = %v; want 401", response, err)
		}
		foreign := http.Header{"Origin": []string{"https://foreign.example"}}
		_, response, err = websocket.DefaultDialer.Dial(wsURL(server.URL)+"/?token="+token, foreign)
		if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
			t.Fatalf("foreign Origin response = %v, error = %v; want 403", response, err)
		}
		bearer := http.Header{"Authorization": []string{"Bearer " + token}}
		client, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/", bearer)
		if err != nil {
			t.Fatalf("Bearer relay dial returned an error: %v", err)
		}
		assertMessage(t, client, []byte("worker-ready"))
		session := latestSession(t, pool, queries)
		closeWebSocket(t, client, websocket.CloseNormalClosure, "bearer done")
		_ = receiveClose(t, worker, session.ID)
	})

	t.Run("no-auth mode", func(t *testing.T) {
		truncateTables(t, pool)
		worker := newEchoWorker(t)
		insertRelayWorker(t, queries, worker.wsURL(), "chromium", "1.62.1", 1)
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)
		client, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/", nil)
		if err != nil {
			t.Fatalf("no-auth relay dial returned an error: %v", err)
		}
		assertMessage(t, client, []byte("worker-ready"))
		session := latestSession(t, pool, queries)
		closeWebSocket(t, client, websocket.CloseNormalClosure, "done")
		_ = receiveClose(t, worker, session.ID)
	})
}

func TestRelay_DrainGates(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)

	t.Run("pre-upgrade request is rejected", func(t *testing.T) {
		truncateTables(t, pool)
		worker := newEchoWorker(t)
		insertRelayWorker(t, queries, worker.wsURL(), "chromium", "1.62.1", 1)
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)
		if err := manager.Shutdown(t.Context()); err != nil {
			t.Fatalf("Manager.Shutdown() returned an error: %v", err)
		}

		_, response, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/", nil)
		if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("draining relay response = %v, error = %v; want 503", response, err)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			t.Fatalf("reading drain response: %v", readErr)
		}
		if !strings.Contains(string(body), "server is shutting down") {
			t.Fatalf("drain response body = %s, want shutdown reason", body)
		}
		if got := worker.upgrades.Load(); got != 0 {
			t.Fatalf("worker upgrades = %d, want 0", got)
		}
	})

	t.Run("drain between preflight and register closes upgraded client", func(t *testing.T) {
		truncateTables(t, pool)
		worker, dialStarted, releaseDial := newGatedEchoWorker(t)
		insertRelayWorker(t, queries, worker.wsURL(), "chromium", "1.62.1", 1)
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)

		type dialResult struct {
			connection *websocket.Conn
			response   *http.Response
			err        error
		}
		dialDone := make(chan dialResult, 1)
		go func() {
			connection, response, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/", nil)
			dialDone <- dialResult{connection: connection, response: response, err: err}
		}()
		select {
		case <-dialStarted:
		case <-time.After(time.Second):
			t.Fatal("worker dial did not start")
		}
		shutdownCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		shutdownDone := make(chan error, 1)
		go func() { shutdownDone <- manager.Shutdown(shutdownCtx) }()
		for {
			probe, beginErr := manager.BeginAttach(uuid.New())
			if errors.Is(beginErr, relay.ErrDraining) {
				break
			}
			if beginErr != nil {
				t.Fatalf("BeginAttach() during shutdown returned an error: %v", beginErr)
			}
			probe.Release()
			time.Sleep(time.Millisecond)
		}
		select {
		case err := <-shutdownDone:
			t.Fatalf("Manager.Shutdown() returned during worker dial: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		running := latestSession(t, pool, queries)
		if running.Status != data.SessionStatusRunning {
			t.Fatalf("gated-dial session status = %q, want running", running.Status)
		}
		releaseDial()

		result := <-dialDone
		if result.err != nil || result.response == nil || result.response.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("racing drain dial response = %v, error = %v; want 101", result.response, result.err)
		}
		defer result.connection.Close()
		closeErr := readClose(t, result.connection)
		if closeErr.Code != websocket.CloseGoingAway || closeErr.Text != "server shutting down" {
			t.Fatalf("racing drain close = %d %q, want 1001 server shutting down", closeErr.Code, closeErr.Text)
		}
		session := latestSession(t, pool, queries)
		waitForSession(t, queries, session.ID, func(current data.Session) bool {
			return current.Status == data.SessionStatusCompleted
		})
		workerClose := receiveClose(t, worker, session.ID)
		if workerClose.Code != websocket.CloseGoingAway || workerClose.Text != "server shutting down" {
			t.Fatalf("racing drain worker close = %d %q, want 1001 server shutting down", workerClose.Code, workerClose.Text)
		}
		if err := <-shutdownDone; err != nil {
			t.Fatalf("Manager.Shutdown() returned an error: %v", err)
		}
		var runningCount int
		if err := pool.QueryRow(
			t.Context(),
			"SELECT count(*) FROM sessions WHERE status = 'running'",
		).Scan(&runningCount); err != nil {
			t.Fatalf("counting running sessions after shutdown: %v", err)
		}
		if runningCount != 0 {
			t.Fatalf("running sessions after shutdown = %d, want 0", runningCount)
		}
	})
}

func TestRelay_ExplicitAttachAndTermination(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)
	truncateTables(t, pool)
	worker := newEchoWorker(t)
	insertRelayWorker(t, queries, worker.wsURL(), "chromium", "1.62.1", 4)
	logger := testLogger(io.Discard)
	manager := newTestRelayManager(queries, logger, relay.Options{})
	server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)

	session := createSession(t, server.URL, "1.62")
	mismatchHeaders := http.Header{"User-Agent": []string{"Playwright/1.63.0 (linux)"}}
	_, response, err := websocket.DefaultDialer.Dial(
		wsURL(server.URL)+"/sessions/"+session.ID.String(),
		mismatchHeaders,
	)
	if err == nil || response == nil || response.StatusCode != http.StatusConflict {
		t.Fatalf("version mismatch response = %v, error = %v; want 409", response, err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !strings.Contains(string(body), "1.63") || !strings.Contains(string(body), "1.62.1") {
		t.Fatalf("version mismatch body = %s, want both versions", body)
	}

	type attachResult struct {
		connection *websocket.Conn
		response   *http.Response
		err        error
	}
	start := make(chan struct{})
	const attachCount = 8
	results := make(chan attachResult, attachCount)
	for range attachCount {
		go func() {
			<-start
			connection, response, err := websocket.DefaultDialer.Dial(
				wsURL(server.URL)+"/sessions/"+session.ID.String(),
				http.Header{"User-Agent": []string{"Playwright/1.62.1 (linux)"}},
			)
			results <- attachResult{connection: connection, response: response, err: err}
		}()
	}
	close(start)
	var winner *websocket.Conn
	conflicts := 0
	for range attachCount {
		result := <-results
		if result.err == nil {
			if result.response == nil || result.response.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("successful attach response = %v, want 101", result.response)
			}
			winner = result.connection
			continue
		}
		if result.response != nil &&
			(result.response.StatusCode == http.StatusConflict || result.response.StatusCode == http.StatusGone) {
			conflicts++
			_ = result.response.Body.Close()
			continue
		}
		t.Fatalf("concurrent attach result = %+v, want upgrade, 409, or 410", result)
	}
	if winner == nil || conflicts != attachCount-1 {
		t.Fatalf(
			"concurrent attaches: winner = %v, rejections = %d; want one winner and %d rejections",
			winner != nil,
			conflicts,
			attachCount-1,
		)
	}
	if got := worker.upgrades.Load(); got != 1 {
		t.Fatalf("worker upgrades = %d, want 1", got)
	}
	defer winner.Close()
	assertMessage(t, winner, []byte("worker-ready"))
	_, runningResponse, runningErr := websocket.DefaultDialer.Dial(
		wsURL(server.URL)+"/sessions/"+session.ID.String(),
		nil,
	)
	if runningErr == nil || runningResponse == nil || runningResponse.StatusCode != http.StatusConflict {
		t.Fatalf("running attach response = %v, error = %v; want 409", runningResponse, runningErr)
	}
	_ = runningResponse.Body.Close()

	deleteResponse := requestJSON(
		t,
		server.Config.Handler,
		http.MethodDelete,
		"/v1/sessions/"+session.ID.String(),
		nil,
		"",
	)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("DELETE live session status = %d, want %d", deleteResponse.Code, http.StatusNoContent)
	}
	closeErr := readClose(t, winner)
	if closeErr.Code != websocket.CloseGoingAway || closeErr.Text != "session terminated" {
		t.Fatalf("client close = %d %q, want 1001 %q", closeErr.Code, closeErr.Text, "session terminated")
	}
	workerClose := receiveClose(t, worker, session.ID)
	if workerClose.Code != websocket.CloseGoingAway || workerClose.Text != "session terminated" {
		t.Fatalf("worker close = %d %q, want 1001 session terminated", workerClose.Code, workerClose.Text)
	}
	waitForSession(t, queries, session.ID, func(current data.Session) bool {
		return current.Status == data.SessionStatusCompleted
	})
	deleteAgain := requestJSON(
		t,
		server.Config.Handler,
		http.MethodDelete,
		"/v1/sessions/"+session.ID.String(),
		nil,
		"",
	)
	if deleteAgain.Code != http.StatusNoContent {
		t.Fatalf("second DELETE status = %d, want %d", deleteAgain.Code, http.StatusNoContent)
	}

	normalSession := createSession(t, server.URL, "")
	normalClient, _, err := websocket.DefaultDialer.Dial(
		wsURL(server.URL)+"/sessions/"+normalSession.ID.String(),
		nil,
	)
	if err != nil {
		t.Fatalf("normal explicit attach returned an error: %v", err)
	}
	assertMessage(t, normalClient, []byte("worker-ready"))
	closeWebSocket(t, normalClient, websocket.CloseNormalClosure, "explicit done")
	normalWorkerClose := receiveClose(t, worker, normalSession.ID)
	if normalWorkerClose.Code != websocket.CloseNormalClosure {
		t.Fatalf("normal explicit worker close = %d, want 1000", normalWorkerClose.Code)
	}
	waitForSession(t, queries, normalSession.ID, func(current data.Session) bool {
		return current.Status == data.SessionStatusCompleted
	})

	expiredID := insertTestSession(t, pool, queries, session.WorkerID, data.SessionStatusExpired)
	_, expiredResponse, expiredErr := websocket.DefaultDialer.Dial(
		wsURL(server.URL)+"/sessions/"+expiredID.String(),
		nil,
	)
	if expiredErr == nil || expiredResponse == nil || expiredResponse.StatusCode != http.StatusGone {
		t.Fatalf("expired attach response = %v, error = %v; want 410", expiredResponse, expiredErr)
	}
	_ = expiredResponse.Body.Close()
	_, missingResponse, missingErr := websocket.DefaultDialer.Dial(
		wsURL(server.URL)+"/sessions/"+uuid.NewString(),
		nil,
	)
	if missingErr == nil || missingResponse == nil || missingResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("missing attach response = %v, error = %v; want 404", missingResponse, missingErr)
	}
	_ = missingResponse.Body.Close()
}

func TestRelay_VersionRoutingAndDialFailure(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)

	t.Run("implicit and explicit version routing", func(t *testing.T) {
		truncateTables(t, pool)
		worker62 := newEchoWorker(t)
		worker63 := newEchoWorker(t)
		matchingID := insertRelayWorker(t, queries, worker62.wsURL(), "chromium", "1.62.1", 2)
		incompatibleID := insertRelayWorker(t, queries, worker63.wsURL(), "chromium", "1.63.0", 2)
		if _, err := pool.Exec(
			t.Context(),
			"UPDATE workers SET lifetime_sessions = 20 WHERE id = $1",
			incompatibleID,
		); err != nil {
			t.Fatalf("aging incompatible worker: %v", err)
		}
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)
		client, _, err := websocket.DefaultDialer.Dial(
			wsURL(server.URL)+"/",
			http.Header{"User-Agent": []string{"Playwright/1.62.1 (linux)"}},
		)
		if err != nil {
			t.Fatalf("version-aware implicit dial returned an error: %v", err)
		}
		assertMessage(t, client, []byte("worker-ready"))
		claimed := latestSession(t, pool, queries)
		if claimed.WorkerID != matchingID {
			t.Fatalf("implicit session worker = %s, want 1.62 worker %s", claimed.WorkerID, matchingID)
		}
		closeWebSocket(t, client, websocket.CloseNormalClosure, "done")
		_ = receiveClose(t, worker62, claimed.ID)
		withoutVersion, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/", nil)
		if err != nil {
			t.Fatalf("versionless implicit dial returned an error: %v", err)
		}
		assertMessage(t, withoutVersion, []byte("worker-ready"))
		versionlessSession := latestSession(t, pool, queries)
		closeWebSocket(t, withoutVersion, websocket.CloseNormalClosure, "done")
		switch versionlessSession.WorkerID {
		case matchingID:
			_ = receiveClose(t, worker62, versionlessSession.ID)
		case incompatibleID:
			_ = receiveClose(t, worker63, versionlessSession.ID)
		default:
			t.Fatalf("versionless session used unknown worker %s", versionlessSession.WorkerID)
		}

		explicit := createSession(t, server.URL, "1.62")
		if explicit.WorkerID != matchingID {
			t.Fatalf("explicit session worker = %s, want 1.62 worker %s", explicit.WorkerID, matchingID)
		}
	})

	t.Run("version miss follows admission failure", func(t *testing.T) {
		truncateTables(t, pool)
		worker := newEchoWorker(t)
		insertRelayWorker(t, queries, worker.wsURL(), "chromium", "1.620.0", 1)
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)
		_, response, err := websocket.DefaultDialer.Dial(
			wsURL(server.URL)+"/",
			http.Header{"User-Agent": []string{"Playwright/1.62.1 (linux)"}},
		)
		if err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("version miss response = %v, error = %v; want 429", response, err)
		}
		_ = response.Body.Close()
		postResponse := requestJSON(
			t,
			server.Config.Handler,
			http.MethodPost,
			"/v1/sessions",
			map[string]any{"browser": "chromium", "playwright_version": "1.62"},
			"",
		)
		if postResponse.Code != http.StatusTooManyRequests {
			t.Fatalf("version-miss POST status = %d, want %d", postResponse.Code, http.StatusTooManyRequests)
		}
	})

	t.Run("dial failure retries on a compatible worker", func(t *testing.T) {
		truncateTables(t, pool)
		var failedDials atomic.Int32
		failedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			failedDials.Add(1)
			http.Error(w, "not a WebSocket worker", http.StatusServiceUnavailable)
		}))
		t.Cleanup(failedServer.Close)
		failedWorkerID := insertRelayWorker(
			t,
			queries,
			wsURL(failedServer.URL),
			"chromium",
			"1.62.1",
			1,
		)
		if _, err := pool.Exec(
			t.Context(),
			"UPDATE workers SET lifetime_sessions = 20 WHERE id = $1",
			failedWorkerID,
		); err != nil {
			t.Fatalf("aging failed worker: %v", err)
		}
		replacement := newEchoWorker(t)
		replacementWorkerID := insertRelayWorker(
			t,
			queries,
			replacement.wsURL(),
			"chromium",
			"1.62.2",
			1,
		)
		incompatible := newEchoWorker(t)
		incompatibleWorkerID := insertRelayWorker(
			t,
			queries,
			incompatible.wsURL(),
			"chromium",
			"1.63.0",
			1,
		)
		if _, err := pool.Exec(
			t.Context(),
			"UPDATE workers SET lifetime_sessions = 50 WHERE id = $1",
			incompatibleWorkerID,
		); err != nil {
			t.Fatalf("aging incompatible worker: %v", err)
		}
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)

		client, response, err := websocket.DefaultDialer.Dial(
			wsURL(server.URL)+"/",
			http.Header{"User-Agent": []string{"Playwright/1.62.1 (linux)"}},
		)
		if err != nil || response == nil || response.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("retried relay response = %v, error = %v; want 101", response, err)
		}
		defer client.Close()
		assertMessage(t, client, []byte("worker-ready"))

		session := latestSession(t, pool, queries)
		if session.WorkerID != replacementWorkerID || session.WorkerAddress != replacement.wsURL() ||
			session.PlaywrightVersion != "1.62.2" || session.Status != data.SessionStatusRunning {
			t.Fatalf("retried session = %+v, want running session on compatible replacement", session)
		}
		failedWorker, err := queries.GetWorker(t.Context(), failedWorkerID)
		if err != nil {
			t.Fatalf("GetWorker() for failed worker returned an error: %v", err)
		}
		if failedWorker.Status != data.WorkerStatusStalled {
			t.Fatalf("failed worker status = %q, want %q", failedWorker.Status, data.WorkerStatusStalled)
		}
		replacementWorker, err := queries.GetWorker(t.Context(), replacementWorkerID)
		if err != nil {
			t.Fatalf("GetWorker() for replacement returned an error: %v", err)
		}
		if replacementWorker.LifetimeSessions != 1 {
			t.Fatalf("replacement lifetime sessions = %d, want 1", replacementWorker.LifetimeSessions)
		}
		if failedDials.Load() != 1 || replacement.upgrades.Load() != 1 || incompatible.upgrades.Load() != 0 {
			t.Fatalf(
				"dial attempts = failed:%d replacement:%d incompatible:%d, want 1/1/0",
				failedDials.Load(),
				replacement.upgrades.Load(),
				incompatible.upgrades.Load(),
			)
		}
		closeWebSocket(t, client, websocket.CloseNormalClosure, "done")
		_ = receiveClose(t, replacement, session.ID)
	})

	t.Run("dial failure with no replacement returns bad gateway", func(t *testing.T) {
		truncateTables(t, pool)
		workerURL := newSlowHandshakeWorker(t)
		workerID := insertRelayWorker(t, queries, workerURL, "chromium", "1.62.1", 1)
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)
		type dialResult struct {
			response *http.Response
			err      error
		}
		dialDone := make(chan dialResult, 1)
		go func() {
			_, response, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/", nil)
			dialDone <- dialResult{response: response, err: err}
		}()
		waitForCondition(t, time.Second, func() bool {
			var count int
			err := pool.QueryRow(
				t.Context(),
				"SELECT count(*) FROM sessions WHERE status = 'running'",
			).Scan(&count)
			return err == nil && count == 1
		})
		var failedSessionID uuid.UUID
		if err := pool.QueryRow(
			t.Context(),
			"SELECT id FROM sessions WHERE status = 'running' LIMIT 1",
		).Scan(&failedSessionID); err != nil {
			t.Fatalf("selecting dial-running session: %v", err)
		}
		result := <-dialDone
		if result.err == nil || result.response == nil || result.response.StatusCode != http.StatusBadGateway {
			t.Fatalf("dead worker response = %v, error = %v; want 502", result.response, result.err)
		}
		_ = result.response.Body.Close()
		failed, err := queries.GetSession(t.Context(), failedSessionID)
		if err != nil {
			t.Fatalf("GetSession() after dial failure returned an error: %v", err)
		}
		if failed.Status != data.SessionStatusFailed {
			t.Fatalf("dial-failed session status = %q, want %q", failed.Status, data.SessionStatusFailed)
		}
		failedWorker, err := queries.GetWorker(t.Context(), workerID)
		if err != nil {
			t.Fatalf("GetWorker() after dial failure returned an error: %v", err)
		}
		if failedWorker.Status != data.WorkerStatusStalled {
			t.Fatalf("dial-failed worker status = %q, want %q", failedWorker.Status, data.WorkerStatusStalled)
		}
	})

	t.Run("client upgrade failure closes worker cleanly", func(t *testing.T) {
		truncateTables(t, pool)
		worker := newEchoWorker(t)
		insertRelayWorker(t, queries, worker.wsURL(), "chromium", "1.62.1", 1)
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)
		request, err := http.NewRequest(http.MethodGet, server.URL+"/", nil)
		if err != nil {
			t.Fatalf("creating invalid upgrade request: %v", err)
		}
		request.Header.Set("Connection", "Upgrade")
		request.Header.Set("Upgrade", "websocket")
		request.Header.Set("Sec-WebSocket-Version", "13")
		request.Header.Set("Sec-WebSocket-Key", "invalid")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("invalid upgrade request returned an error: %v", err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid upgrade status = %d, want %d", response.StatusCode, http.StatusBadRequest)
		}
		session := latestSession(t, pool, queries)
		workerClose := receiveClose(t, worker, session.ID)
		if workerClose.Code != websocket.CloseInternalServerErr || workerClose.Text != "client upgrade failed" {
			t.Fatalf("upgrade-failure worker close = %d %q, want 1011 client upgrade failed", workerClose.Code, workerClose.Text)
		}
		waitForSession(t, queries, session.ID, func(current data.Session) bool {
			return current.Status == data.SessionStatusFailed
		})
	})
}

func TestRelay_LivenessBlockedPeerAndShutdown(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)

	t.Run("idle relay stays live and rescuer does not expire it", func(t *testing.T) {
		truncateTables(t, pool)
		worker := newEchoWorker(t)
		insertRelayWorker(t, queries, worker.wsURL(), "chromium", "1.62.1", 1)
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{
			WriteTimeout: 250 * time.Millisecond,
			PingInterval: 20 * time.Millisecond,
			PongTimeout:  80 * time.Millisecond,
		})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)
		rescuerCtx, stopRescuer := context.WithCancel(t.Context())
		defer stopRescuer()
		go rescuer.New(pool, logger, rescuer.Options{
			WorkerTTL:        time.Hour,
			SessionTTL:       60 * time.Millisecond,
			StalledWorkerTTL: time.Hour,
			Interval:         10 * time.Millisecond,
		}).Run(rescuerCtx)
		client, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/", nil)
		if err != nil {
			t.Fatalf("idle relay dial returned an error: %v", err)
		}
		assertMessage(t, client, []byte("worker-ready"))
		messages := make(chan []byte, 1)
		readErrors := make(chan error, 1)
		go func() {
			for {
				_, message, err := client.ReadMessage()
				if err != nil {
					readErrors <- err
					return
				}
				messages <- message
			}
		}()
		time.Sleep(180 * time.Millisecond)
		if err := client.WriteMessage(websocket.TextMessage, []byte("still-live")); err != nil {
			t.Fatalf("writing after idle period: %v", err)
		}
		select {
		case message := <-messages:
			if !bytes.Equal(message, []byte("still-live")) {
				t.Fatalf("idle relay message = %q, want still-live", message)
			}
		case err := <-readErrors:
			t.Fatalf("idle relay reader stopped: %v", err)
		case <-time.After(time.Second):
			t.Fatal("idle relay did not echo after pong-timeout windows")
		}
		session := latestSession(t, pool, queries)
		if session.Status != data.SessionStatusRunning {
			t.Fatalf("idle session status = %q, want running", session.Status)
		}
		closeWebSocket(t, client, websocket.CloseNormalClosure, "done")
		_ = receiveClose(t, worker, session.ID)
	})

	t.Run("heartbeat database failure does not stop relay", func(t *testing.T) {
		truncateTables(t, pool)
		worker := newEchoWorker(t)
		insertRelayWorker(t, queries, worker.wsURL(), "chromium", "1.62.1", 1)
		errRenew := errors.New("database unavailable")
		var logs lockedBuffer
		logger := testLogger(&logs)
		managerQueries := data.New(failingRenewDB{DBTX: pool, err: errRenew})
		manager := newTestRelayManager(managerQueries, logger, relay.Options{
			HeartbeatInterval: 10 * time.Millisecond,
		})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)
		client, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/", nil)
		if err != nil {
			t.Fatalf("renewal-failure relay dial returned an error: %v", err)
		}
		defer client.Close()
		assertMessage(t, client, []byte("worker-ready"))
		waitForCondition(t, time.Second, func() bool {
			return strings.Contains(logs.String(), "renew session heartbeat") &&
				strings.Contains(logs.String(), errRenew.Error())
		})
		if err := client.WriteMessage(websocket.TextMessage, []byte("still-live")); err != nil {
			t.Fatalf("writing after renewal failure: %v", err)
		}
		assertMessage(t, client, []byte("still-live"))
		session := latestSession(t, pool, queries)
		closeWebSocket(t, client, websocket.CloseNormalClosure, "done")
		_ = receiveClose(t, worker, session.ID)
		waitForSession(t, queries, session.ID, func(current data.Session) bool {
			return current.Status == data.SessionStatusCompleted
		})
	})

	t.Run("peer that does not process pings is failed", func(t *testing.T) {
		truncateTables(t, pool)
		blocked, release := newBlockedWorker(t)
		defer release()
		insertRelayWorker(t, queries, blocked, "chromium", "1.62.1", 1)
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{
			WriteTimeout: 250 * time.Millisecond,
			PingInterval: 20 * time.Millisecond,
			PongTimeout:  80 * time.Millisecond,
		})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)
		client, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/", nil)
		if err != nil {
			t.Fatalf("blocked-worker relay dial returned an error: %v", err)
		}
		closeErr := readClose(t, client)
		if closeErr.Code != websocket.CloseInternalServerErr {
			t.Fatalf("unresponsive peer close code = %d, want %d", closeErr.Code, websocket.CloseInternalServerErr)
		}
		failed := latestSession(t, pool, queries)
		waitForSession(t, queries, failed.ID, func(current data.Session) bool {
			return current.Status == data.SessionStatusFailed
		})
	})

	t.Run("shutdown closes active relay after grace", func(t *testing.T) {
		truncateTables(t, pool)
		worker := newEchoWorker(t)
		insertRelayWorker(t, queries, worker.wsURL(), "chromium", "1.62.1", 1)
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{ShutdownGrace: 20 * time.Millisecond})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)
		client, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/", nil)
		if err != nil {
			t.Fatalf("shutdown relay dial returned an error: %v", err)
		}
		assertMessage(t, client, []byte("worker-ready"))
		shutdownCtx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		shutdownDone := make(chan error, 1)
		go func() { shutdownDone <- manager.Shutdown(shutdownCtx) }()
		closeErr := readClose(t, client)
		if closeErr.Code != websocket.CloseGoingAway || closeErr.Text != "server shutting down" {
			t.Fatalf("shutdown close = %d %q, want 1001 server shutting down", closeErr.Code, closeErr.Text)
		}
		if err := <-shutdownDone; err != nil {
			t.Fatalf("Manager.Shutdown() returned an error: %v", err)
		}
		session := latestSession(t, pool, queries)
		if session.Status != data.SessionStatusCompleted {
			t.Fatalf("shutdown session status = %q, want completed", session.Status)
		}
	})

	t.Run("shutdown interrupts a blocked data writer", func(t *testing.T) {
		truncateTables(t, pool)
		workerURL, releaseWorker, workerClosed := newBlockedWorkerWithCloseSignal(t)
		insertRelayWorker(t, queries, workerURL, "chromium", "1.62.1", 1)
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{
			WriteTimeout:  30 * time.Second,
			PingInterval:  time.Hour,
			PongTimeout:   time.Hour,
			ShutdownGrace: 20 * time.Millisecond,
		})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)
		client, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/", nil)
		if err != nil {
			t.Fatalf("blocked-shutdown relay dial returned an error: %v", err)
		}
		defer client.Close()
		writerStarted := make(chan struct{})
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			payload := bytes.Repeat([]byte("blocked-shutdown"), 256*1024)
			close(writerStarted)
			for {
				if err := client.WriteMessage(websocket.BinaryMessage, payload); err != nil {
					return
				}
			}
		}()
		<-writerStarted
		time.Sleep(100 * time.Millisecond)

		shutdownCtx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		defer cancel()
		started := time.Now()
		if err := manager.Shutdown(shutdownCtx); err != nil {
			t.Fatalf("Manager.Shutdown() with blocked writer returned an error: %v", err)
		}
		if elapsed := time.Since(started); elapsed >= 6*time.Second {
			t.Fatalf("Manager.Shutdown() duration = %v, want less than 6s", elapsed)
		}
		closeErr := readClose(t, client)
		if closeErr.Code != websocket.CloseGoingAway || closeErr.Text != "server shutting down" {
			t.Fatalf("blocked shutdown close = %d %q, want 1001 server shutting down", closeErr.Code, closeErr.Text)
		}
		releaseWorker()
		select {
		case <-workerClosed:
		case <-time.After(time.Second):
			t.Fatal("worker connection stayed open after shutdown")
		}
		select {
		case <-writerDone:
		case <-time.After(time.Second):
			t.Fatal("client writer stayed blocked after shutdown")
		}
		session := latestSession(t, pool, queries)
		if session.Status != data.SessionStatusCompleted {
			t.Fatalf("blocked-shutdown session status = %q, want completed", session.Status)
		}
	})
}

func TestRelay_CloseCodeForwardingAndBlockedWrite(t *testing.T) {
	pool := newMigratedTestPool(t)
	queries := data.New(pool)

	t.Run("worker close code and reason reach client", func(t *testing.T) {
		truncateTables(t, pool)
		workerURL := newClosingWorker(t, 4008, "worker restart")
		insertRelayWorker(t, queries, workerURL, "chromium", "1.62.1", 1)
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)
		client, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/", nil)
		if err != nil {
			t.Fatalf("worker-close relay dial returned an error: %v", err)
		}
		closeErr := readClose(t, client)
		if closeErr.Code != 4008 || closeErr.Text != "worker restart" {
			t.Fatalf("client close = %d %q, want 4008 worker restart", closeErr.Code, closeErr.Text)
		}
		session := latestSession(t, pool, queries)
		waitForSession(t, queries, session.ID, func(current data.Session) bool {
			return current.Status == data.SessionStatusFailed
		})
	})

	t.Run("client close code and reason reach worker", func(t *testing.T) {
		truncateTables(t, pool)
		worker := newEchoWorker(t)
		insertRelayWorker(t, queries, worker.wsURL(), "chromium", "1.62.1", 1)
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)
		client, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/", nil)
		if err != nil {
			t.Fatalf("client-close relay dial returned an error: %v", err)
		}
		assertMessage(t, client, []byte("worker-ready"))
		session := latestSession(t, pool, queries)
		closeWebSocket(t, client, 4008, "client restart")
		closeErr := receiveClose(t, worker, session.ID)
		if closeErr.Code != 4008 || closeErr.Text != "client restart" {
			t.Fatalf("worker close = %d %q, want 4008 client restart", closeErr.Code, closeErr.Text)
		}
		waitForSession(t, queries, session.ID, func(current data.Session) bool {
			return current.Status == data.SessionStatusFailed
		})
	})

	t.Run("worker transport loss becomes valid client close", func(t *testing.T) {
		truncateTables(t, pool)
		workerURL := newAbortingWorker(t)
		insertRelayWorker(t, queries, workerURL, "chromium", "1.62.1", 1)
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)
		client, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/", nil)
		if err != nil {
			t.Fatalf("aborting-worker relay dial returned an error: %v", err)
		}
		defer client.Close()
		closeErr := readClose(t, client)
		if closeErr.Code != websocket.CloseInternalServerErr || closeErr.Text != "peer connection lost" {
			t.Fatalf("transport-loss close = %d %q, want 1011 peer connection lost", closeErr.Code, closeErr.Text)
		}
		session := latestSession(t, pool, queries)
		waitForSession(t, queries, session.ID, func(current data.Session) bool {
			return current.Status == data.SessionStatusFailed
		})
	})

	t.Run("client transport loss becomes valid worker close", func(t *testing.T) {
		truncateTables(t, pool)
		worker := newEchoWorker(t)
		insertRelayWorker(t, queries, worker.wsURL(), "chromium", "1.62.1", 1)
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)
		client, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/", nil)
		if err != nil {
			t.Fatalf("client-loss relay dial returned an error: %v", err)
		}
		assertMessage(t, client, []byte("worker-ready"))
		session := latestSession(t, pool, queries)
		if err := client.UnderlyingConn().Close(); err != nil {
			t.Fatalf("closing client TCP connection: %v", err)
		}
		closeErr := receiveClose(t, worker, session.ID)
		if closeErr.Code != websocket.CloseInternalServerErr || closeErr.Text != "peer connection lost" {
			t.Fatalf("client-loss worker close = %d %q, want 1011 peer connection lost", closeErr.Code, closeErr.Text)
		}
		waitForSession(t, queries, session.ID, func(current data.Session) bool {
			return current.Status == data.SessionStatusFailed
		})
	})

	t.Run("incomplete source message is not completed at worker", func(t *testing.T) {
		truncateTables(t, pool)
		worker := newMessageObservingWorker(t)
		insertRelayWorker(t, queries, worker.wsURL(), "chromium", "1.62.1", 1)
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)
		client, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/", nil)
		if err != nil {
			t.Fatalf("partial-message relay dial returned an error: %v", err)
		}
		assertMessage(t, client, []byte("worker-ready"))
		session := latestSession(t, pool, queries)
		writeNonFinalClientFrame(t, client.UnderlyingConn(), []byte("partial protocol message"))
		if err := client.UnderlyingConn().Close(); err != nil {
			t.Fatalf("closing partial-message TCP connection: %v", err)
		}
		select {
		case message := <-worker.messages:
			t.Fatalf("worker received completed truncated message %q", message)
		case <-worker.readErrors:
		case <-time.After(2 * time.Second):
			t.Fatal("worker did not observe relay teardown")
		}
		waitForSession(t, queries, session.ID, func(current data.Session) bool {
			return current.Status == data.SessionStatusFailed
		})
	})

	t.Run("worker that stops reading hits write deadline", func(t *testing.T) {
		truncateTables(t, pool)
		blocked, release := newBlockedWorker(t)
		defer release()
		insertRelayWorker(t, queries, blocked, "chromium", "1.62.1", 1)
		logger := testLogger(io.Discard)
		manager := newTestRelayManager(queries, logger, relay.Options{
			WriteTimeout: 30 * time.Millisecond,
			PingInterval: time.Second,
			PongTimeout:  2 * time.Second,
		})
		server := newRelayHTTPServer(t, pool, NoAuthAuthenticator{}, manager, logger, 0)
		client, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL)+"/", nil)
		if err != nil {
			t.Fatalf("blocked-write relay dial returned an error: %v", err)
		}
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			payload := bytes.Repeat([]byte("blocked-write"), 128*1024)
			for range 64 {
				if err := client.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
					return
				}
				if err := client.WriteMessage(websocket.BinaryMessage, payload); err != nil {
					return
				}
			}
		}()
		closeErr := readClose(t, client)
		if closeErr.Code != websocket.CloseInternalServerErr {
			t.Fatalf("blocked-write close code = %d, want %d", closeErr.Code, websocket.CloseInternalServerErr)
		}
		select {
		case <-writerDone:
		case <-time.After(3 * time.Second):
			t.Fatal("client writer did not stop after relay teardown")
		}
		session := latestSession(t, pool, queries)
		waitForSession(t, queries, session.ID, func(current data.Session) bool {
			return current.Status == data.SessionStatusFailed
		})
	})
}

func newEchoWorker(t *testing.T) *echoWorker {
	t.Helper()
	return newEchoWorkerWithGate(t, nil, nil)
}

func newGatedEchoWorker(t *testing.T) (*echoWorker, <-chan struct{}, func()) {
	t.Helper()
	dialStarted := make(chan struct{})
	releaseDial := make(chan struct{})
	worker := newEchoWorkerWithGate(t, dialStarted, releaseDial)
	var once sync.Once
	release := func() {
		once.Do(func() { close(releaseDial) })
	}
	t.Cleanup(release)
	return worker, dialStarted, release
}

func newEchoWorkerWithGate(
	t *testing.T,
	dialStarted chan<- struct{},
	releaseDial <-chan struct{},
) *echoWorker {
	t.Helper()
	worker := &echoWorker{
		headers: make(chan http.Header, 1),
		closes:  make(chan workerClose, 8),
	}
	upgrader := websocket.Upgrader{}
	worker.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if dialStarted != nil {
			close(dialStarted)
			<-releaseDial
		}
		connection, err := upgrader.Upgrade(w, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		worker.upgrades.Add(1)
		sessionID := request.Header.Get("x-pwd-session-id")
		select {
		case worker.headers <- request.Header.Clone():
		default:
		}
		if err := connection.WriteMessage(websocket.TextMessage, []byte("worker-ready")); err != nil {
			return
		}
		buffer := make([]byte, 32*1024)
		for {
			messageType, reader, err := connection.NextReader()
			if err != nil {
				var closeErr *websocket.CloseError
				if errors.As(err, &closeErr) && closeErr.Code != websocket.CloseAbnormalClosure {
					select {
					case worker.closes <- workerClose{sessionID: sessionID, err: closeErr}:
					default:
					}
				}
				return
			}
			writer, err := connection.NextWriter(messageType)
			if err != nil {
				return
			}
			if _, err := io.CopyBuffer(writer, reader, buffer); err != nil {
				_ = writer.Close()
				return
			}
			if err := writer.Close(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(func() {
		worker.server.CloseClientConnections()
		worker.server.Close()
	})
	return worker
}

func (w *echoWorker) wsURL() string {
	return wsURL(w.server.URL)
}

func newBlockedWorker(t *testing.T) (string, func()) {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(w, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		<-release
	}))
	stop := func() {
		once.Do(func() { close(release) })
		server.CloseClientConnections()
		server.Close()
	}
	t.Cleanup(stop)
	return wsURL(server.URL), stop
}

func newBlockedWorkerWithCloseSignal(t *testing.T) (string, func(), <-chan struct{}) {
	t.Helper()
	release := make(chan struct{})
	closed := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(w, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		<-release
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				close(closed)
				return
			}
		}
	}))
	stop := func() {
		releaseOnce.Do(func() { close(release) })
	}
	t.Cleanup(func() {
		stop()
		server.CloseClientConnections()
		server.Close()
	})
	return wsURL(server.URL), stop, closed
}

func newClosingWorker(t *testing.T, code int, reason string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(w, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		_ = connection.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(code, reason),
			time.Now().Add(time.Second),
		)
	}))
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	return wsURL(server.URL)
}

func newAbortingWorker(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(w, request, nil)
		if err != nil {
			return
		}
		_ = connection.UnderlyingConn().Close()
	}))
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	return wsURL(server.URL)
}

func newMessageObservingWorker(t *testing.T) *messageObservingWorker {
	t.Helper()
	worker := &messageObservingWorker{
		messages:   make(chan []byte, 1),
		readErrors: make(chan error, 1),
	}
	worker.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(w, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		if err := connection.WriteMessage(websocket.TextMessage, []byte("worker-ready")); err != nil {
			return
		}
		_, message, err := connection.ReadMessage()
		if err != nil {
			worker.readErrors <- err
			return
		}
		worker.messages <- message
	}))
	t.Cleanup(func() {
		worker.server.CloseClientConnections()
		worker.server.Close()
	})
	return worker
}

func (w *messageObservingWorker) wsURL() string {
	return wsURL(w.server.URL)
}

func newSlowHandshakeWorker(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(time.Second)
	}))
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	return wsURL(server.URL)
}

func newTestRelayManager(
	queries *data.Queries,
	logger *slog.Logger,
	overrides relay.Options,
) *relay.Manager {
	options := relay.Options{
		WriteTimeout:      testWriteTimeout,
		PingInterval:      testPingInterval,
		PongTimeout:       testPongTimeout,
		HeartbeatInterval: testHeartbeatInterval,
		ShutdownGrace:     50 * time.Millisecond,
	}
	if overrides.WriteTimeout > 0 {
		options.WriteTimeout = overrides.WriteTimeout
	}
	if overrides.PingInterval > 0 {
		options.PingInterval = overrides.PingInterval
	}
	if overrides.PongTimeout > 0 {
		options.PongTimeout = overrides.PongTimeout
	}
	if overrides.HeartbeatInterval > 0 {
		options.HeartbeatInterval = overrides.HeartbeatInterval
	}
	if overrides.ShutdownGrace > 0 {
		options.ShutdownGrace = overrides.ShutdownGrace
	}
	return relay.NewManager(queries, logger, options)
}

func newRelayHTTPServer(
	t *testing.T,
	pool *pgxpool.Pool,
	authenticator Authenticator,
	manager *relay.Manager,
	logger *slog.Logger,
	maxQueueSize int,
) *httptest.Server {
	t.Helper()
	queries := data.New(pool)
	server := New(
		pool,
		queries,
		authenticator,
		logger,
		WithScheduler(newTestSchedulerForRelay(pool, maxQueueSize)),
		WithRelayManager(manager, "chromium", 250*time.Millisecond),
	)
	httpServer := httptest.NewServer(server.Handler)
	t.Cleanup(httpServer.Close)
	return httpServer
}

func newTestSchedulerForRelay(pool *pgxpool.Pool, maxQueueSize int) *scheduler.Scheduler {
	return scheduler.New(
		context.Background(),
		pool,
		testLogger(io.Discard),
		scheduler.Options{
			WorkerTTL:           time.Hour,
			PendingSessionTTL:   time.Hour,
			MaxLifetimeSessions: 100,
			MaxQueueSize:        maxQueueSize,
			QueueWaitTimeout:    100 * time.Millisecond,
			PollingInterval:     10 * time.Millisecond,
		},
	)
}

func insertRelayWorker(
	t *testing.T,
	queries *data.Queries,
	address, browser, version string,
	maxSlots int32,
) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := queries.RegisterWorker(t.Context(), data.RegisterWorkerParams{
		ID:                id,
		Address:           address,
		Browser:           browser,
		PlaywrightVersion: version,
		MaxSlots:          maxSlots,
		Status:            data.WorkerStatusAvailable,
	}); err != nil {
		t.Fatalf("RegisterWorker() returned an error: %v", err)
	}
	return id
}

func createSession(t *testing.T, serverURL, version string) Session {
	t.Helper()
	body := map[string]any{"browser": "chromium"}
	if version != "" {
		body["playwright_version"] = version
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding create session body: %v", err)
	}
	response, err := http.Post(serverURL+"/v1/sessions", "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("POST /v1/sessions returned an error: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("POST /v1/sessions status = %d, want %d: %s", response.StatusCode, http.StatusCreated, body)
	}
	var session Session
	if err := json.NewDecoder(response.Body).Decode(&session); err != nil {
		t.Fatalf("decoding created session: %v", err)
	}
	return session
}

func latestSession(t *testing.T, pool *pgxpool.Pool, queries *data.Queries) data.Session {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(t.Context(), "SELECT id FROM sessions ORDER BY created_at DESC, id DESC LIMIT 1").Scan(&id); err != nil {
		t.Fatalf("selecting latest session: %v", err)
	}
	session, err := queries.GetSession(t.Context(), id)
	if err != nil {
		t.Fatalf("GetSession(%s) returned an error: %v", id, err)
	}
	return session
}

func waitForSession(
	t *testing.T,
	queries *data.Queries,
	sessionID uuid.UUID,
	condition func(data.Session) bool,
) {
	t.Helper()
	waitForCondition(t, 2*time.Second, func() bool {
		session, err := queries.GetSession(t.Context(), sessionID)
		return err == nil && condition(session)
	})
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true before timeout")
		}
		time.Sleep(time.Millisecond)
	}
}

func assertMessage(t *testing.T, connection *websocket.Conn, want []byte) {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("setting client read deadline: %v", err)
	}
	_, got, err := connection.ReadMessage()
	if err != nil {
		t.Fatalf("reading relayed message: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("relayed message length = %d, want %d", len(got), len(want))
	}
}

func readClose(t *testing.T, connection *websocket.Conn) *websocket.CloseError {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("setting close read deadline: %v", err)
	}
	_, _, err := connection.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("ReadMessage() error = %v, want WebSocket close error", err)
	}
	return closeErr
}

func closeWebSocket(t *testing.T, connection *websocket.Conn, code int, reason string) {
	t.Helper()
	if err := connection.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason),
		time.Now().Add(time.Second),
	); err != nil {
		t.Fatalf("writing WebSocket close: %v", err)
	}
}

func writeNonFinalClientFrame(t *testing.T, connection net.Conn, payload []byte) {
	t.Helper()
	if len(payload) > 125 {
		t.Fatalf("test payload length = %d, want at most 125", len(payload))
	}
	mask := [4]byte{0x12, 0x34, 0x56, 0x78}
	frame := make([]byte, 0, 2+len(mask)+len(payload))
	frame = append(frame, websocket.BinaryMessage, 0x80|byte(len(payload)))
	frame = append(frame, mask[:]...)
	for index, value := range payload {
		frame = append(frame, value^mask[index%len(mask)])
	}
	written, err := connection.Write(frame)
	if err != nil {
		t.Fatalf("writing non-final client frame: %v", err)
	}
	if written != len(frame) {
		t.Fatalf("non-final frame bytes written = %d, want %d", written, len(frame))
	}
}

func receiveClose(t *testing.T, worker *echoWorker, sessionID uuid.UUID) *websocket.CloseError {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case closeEvent := <-worker.closes:
			if closeEvent.sessionID == sessionID.String() {
				return closeEvent.err
			}
		case <-timer.C:
			t.Fatalf("worker did not receive a close frame for session %s", sessionID)
			return nil
		}
	}
}

func receiveHeaders(t *testing.T, headers <-chan http.Header) http.Header {
	t.Helper()
	select {
	case value := <-headers:
		return value
	case <-time.After(time.Second):
		t.Fatal("worker did not receive request headers")
		return nil
	}
}

func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}
