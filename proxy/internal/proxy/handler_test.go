package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"

	"proxy/internal/models"
	"proxy/internal/redis"
	"proxy/pkg/config"
	"proxy/pkg/logger"
)

func init() {
	logger.Log = logrus.New()
}

type fakeRedisClient struct {
	selectWorkerFunc              func(ctx context.Context, browserType string) (redis.ServerInfo, error)
	triggerWorkerShutdownFunc     func(ctx context.Context, serverInfo *redis.ServerInfo)
	modifyActiveConnectionsFunc   func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error
	modifyLifetimeConnectionsFunc func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error
}

func (f *fakeRedisClient) SelectWorker(ctx context.Context, browserType string) (redis.ServerInfo, error) {
	if f.selectWorkerFunc != nil {
		return f.selectWorkerFunc(ctx, browserType)
	}
	return redis.ServerInfo{}, nil
}

func (f *fakeRedisClient) TriggerWorkerShutdownIfNeeded(ctx context.Context, serverInfo *redis.ServerInfo) {
	if f.triggerWorkerShutdownFunc != nil {
		f.triggerWorkerShutdownFunc(ctx, serverInfo)
	}
}

func (f *fakeRedisClient) ModifyActiveConnections(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
	if f.modifyActiveConnectionsFunc != nil {
		return f.modifyActiveConnectionsFunc(ctx, serverInfo, delta)
	}
	return nil
}

func (f *fakeRedisClient) ModifyLifetimeConnections(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
	if f.modifyLifetimeConnectionsFunc != nil {
		return f.modifyLifetimeConnectionsFunc(ctx, serverInfo, delta)
	}
	return nil
}

func newTestConfig() *config.Config {
	return &config.Config{
		DefaultBrowserType:  "chromium",
		WorkerSelectTimeout: 0,
	}
}

func TestProxyHandler_InvalidBrowserType(t *testing.T) {
	handler := proxyHandler(&fakeRedisClient{}, newTestConfig())

	req := httptest.NewRequest("GET", "/?browser=opera", nil)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, resp.Code)
	}

	var body models.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if body.Error.Message == "" {
		t.Fatalf("expected error message, got empty string")
	}
}

func TestProxyHandler_NonRootPathReturnsNotFound(t *testing.T) {
	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			t.Fatalf("selectWorker should not be called, got browserType %s", browserType)
			return redis.ServerInfo{}, nil
		},
	}

	handler := proxyHandler(fake, newTestConfig())

	req := httptest.NewRequest("GET", "/healthz", nil)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, resp.Code)
	}
}

func TestProxyHandler_DefaultBrowserTypeIsUsedWhenMissing(t *testing.T) {
	cfg := newTestConfig()
	cfg.DefaultBrowserType = "webkit"

	browserCh := make(chan string, 1)
	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			browserCh <- browserType
			return redis.ServerInfo{}, errors.New("boom")
		},
	}

	handler := proxyHandler(fake, cfg)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, resp.Code)
	}

	select {
	case browser := <-browserCh:
		if browser != "webkit" {
			t.Fatalf("expected browser type 'webkit', got %s", browser)
		}
	default:
		t.Fatal("expected SelectWorker to be called")
	}
}

func TestProxyHandler_AllowsKnownBrowserQueryValues(t *testing.T) {
	tests := []struct {
		name   string
		query  string
		expect string
	}{
		{name: "chromium", query: "chromium", expect: "chromium"},
		{name: "firefox", query: "firefox", expect: "firefox"},
		{name: "webkit", query: "webkit", expect: "webkit"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			browserCh := make(chan string, 1)
			fake := &fakeRedisClient{
				selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
					browserCh <- browserType
					return redis.ServerInfo{}, errors.New("boom")
				},
			}

			handler := proxyHandler(fake, newTestConfig())

			req := httptest.NewRequest("GET", "/?browser="+tc.query, nil)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			resp := httptest.NewRecorder()

			handler.ServeHTTP(resp, req)

			if resp.Code != http.StatusInternalServerError {
				t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, resp.Code)
			}

			select {
			case browser := <-browserCh:
				if browser != tc.expect {
					t.Fatalf("expected browser type %q, got %q", tc.expect, browser)
				}
			default:
				t.Fatal("expected SelectWorker to be called")
			}
		})
	}
}

func TestProxyHandler_NonWebSocketRequest(t *testing.T) {
	handler := proxyHandler(&fakeRedisClient{}, newTestConfig())

	req := httptest.NewRequest("GET", "/", nil)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusUpgradeRequired {
		t.Fatalf("expected status %d, got %d", http.StatusUpgradeRequired, resp.Code)
	}

	var body models.MessageResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if body.Message == "" {
		t.Fatalf("expected message, got empty string")
	}
}

func TestProxyHandler_NoAvailableWorkers(t *testing.T) {
	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			return redis.ServerInfo{}, redis.ErrNoAvailableWorkers
		},
	}
	handler := proxyHandler(fake, newTestConfig())

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d, got %d", http.StatusServiceUnavailable, resp.Code)
	}
}

func TestProxyHandler_SelectWorkerUnexpectedError(t *testing.T) {
	expectedErr := errors.New("boom")
	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			return redis.ServerInfo{}, expectedErr
		},
	}
	handler := proxyHandler(fake, newTestConfig())

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "test")
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, resp.Code)
	}
}

func TestSelectWorkerWithRetryRetriesUntilSuccess(t *testing.T) {
	attempts := 0
	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			attempts++
			if attempts < 2 {
				return redis.ServerInfo{}, redis.ErrNoAvailableWorkers
			}
			return redis.ServerInfo{ID: "worker-1"}, nil
		},
	}

	timeout := time.Second
	server, err := selectWorkerWithRetry(context.Background(), fake, timeout, "chromium")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}

	if server.ID != "worker-1" {
		t.Fatalf("expected worker ID 'worker-1', got %s", server.ID)
	}
}

func TestSelectWorkerWithRetryTimeout(t *testing.T) {
	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			return redis.ServerInfo{}, redis.ErrNoAvailableWorkers
		},
	}

	timeout := 200 * time.Millisecond
	start := time.Now()
	_, err := selectWorkerWithRetry(context.Background(), fake, timeout, "chromium")
	if !errors.Is(err, redis.ErrNoAvailableWorkers) {
		t.Fatalf("expected ErrNoAvailableWorkers, got %v", err)
	}

	if elapsed := time.Since(start); elapsed < timeout {
		t.Fatalf("expected retry loop to last at least %v, got %v", timeout, elapsed)
	}
}

func TestSelectWorkerWithRetryCanceledContext(t *testing.T) {
	calls := int32(0)
	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			atomic.AddInt32(&calls, 1)
			if ctx.Err() == nil {
				t.Fatalf("expected context to be cancelled")
			}
			return redis.ServerInfo{}, redis.ErrNoAvailableWorkers
		},
	}

	parent, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := selectWorkerWithRetry(parent, fake, time.Second, "chromium")
	if !errors.Is(err, redis.ErrNoAvailableWorkers) {
		t.Fatalf("expected ErrNoAvailableWorkers, got %v", err)
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected SelectWorker to be called once, got %d", got)
	}
}

func TestProxyHandler_SuccessfulConnectionLifecycle(t *testing.T) {
	atomic.StoreInt64(&activeConnections, 0)
	t.Cleanup(func() {
		atomic.StoreInt64(&activeConnections, 0)
	})

	backendUpgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := backendUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("backend upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		for {
			msgType, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(msgType, payload); err != nil {
				return
			}
		}
	}))
	defer backend.Close()

	workerEndpoint := "ws" + strings.TrimPrefix(backend.URL, "http")

	shutdownCalled := make(chan struct{}, 1)
	modifyCalls := make(chan int64, 1)

	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			return redis.ServerInfo{ID: "worker-1", Endpoint: workerEndpoint}, nil
		},
		triggerWorkerShutdownFunc: func(ctx context.Context, serverInfo *redis.ServerInfo) {
			shutdownCalled <- struct{}{}
		},
		modifyActiveConnectionsFunc: func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
			modifyCalls <- delta
			return nil
		},
	}

	cfg := newTestConfig()
	cfg.WorkerSelectTimeout = 1

	proxyServer := httptest.NewServer(http.HandlerFunc(proxyHandler(fake, cfg)))
	defer proxyServer.Close()

	proxyURL := "ws" + strings.TrimPrefix(proxyServer.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(proxyURL, nil)
	if err != nil {
		t.Fatalf("failed to dial proxy: %v", err)
	}

	if err := clientConn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("failed to send message through proxy: %v", err)
	}

	if err := clientConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("failed to set read deadline: %v", err)
	}
	msgType, echo, err := clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read echo message: %v", err)
	}
	if msgType != websocket.TextMessage || string(echo) != "ping" {
		t.Fatalf("unexpected echo response: type=%d payload=%q", msgType, string(echo))
	}

	if err := clientConn.Close(); err != nil {
		t.Fatalf("failed to close client connection: %v", err)
	}

	select {
	case <-shutdownCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected TriggerWorkerShutdownIfNeeded to be called")
	}

	select {
	case delta := <-modifyCalls:
		if delta != -1 {
			t.Fatalf("expected ModifyActiveConnections delta -1, got %d", delta)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected ModifyActiveConnections to be called")
	}
}

func TestProxyHandler_BackendDialFailure(t *testing.T) {
	shutdownCalled := make(chan struct{}, 1)
	modifyActiveCalls := make(chan int64, 1)
	modifyLifetimeCalls := make(chan int64, 1)

	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			return redis.ServerInfo{ID: "worker-1", Endpoint: "ws://127.0.0.1:1"}, nil
		},
		triggerWorkerShutdownFunc: func(ctx context.Context, serverInfo *redis.ServerInfo) {
			shutdownCalled <- struct{}{}
		},
		modifyActiveConnectionsFunc: func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
			modifyActiveCalls <- delta
			return nil
		},
		modifyLifetimeConnectionsFunc: func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
			modifyLifetimeCalls <- delta
			return nil
		},
	}

	handler := proxyHandler(fake, newTestConfig())

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "test")

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, resp.Code)
	}

	select {
	case <-shutdownCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected TriggerWorkerShutdownIfNeeded to be called")
	}

	select {
	case delta := <-modifyActiveCalls:
		if delta != -1 {
			t.Fatalf("expected ModifyActiveConnections delta -1, got %d", delta)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected ModifyActiveConnections rollback")
	}

	select {
	case delta := <-modifyLifetimeCalls:
		if delta != -1 {
			t.Fatalf("expected ModifyLifetimeConnections delta -1, got %d", delta)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected ModifyLifetimeConnections rollback")
	}
}

func TestProxyHandler_WebSocketUpgradeFailureRollsBackCounters(t *testing.T) {
	atomic.StoreInt64(&activeConnections, 0)
	t.Cleanup(func() {
		atomic.StoreInt64(&activeConnections, 0)
	})

	backendUpgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := backendUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("backend upgrade failed: %v", err)
		}
		conn.Close()
	}))
	defer backend.Close()

	workerEndpoint := "ws" + strings.TrimPrefix(backend.URL, "http")

	activeRollbacks := make(chan int64, 1)
	lifetimeRollbacks := make(chan int64, 1)
	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			return redis.ServerInfo{ID: "worker-1", Endpoint: workerEndpoint}, nil
		},
		modifyActiveConnectionsFunc: func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
			activeRollbacks <- delta
			return nil
		},
		modifyLifetimeConnectionsFunc: func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
			lifetimeRollbacks <- delta
			return nil
		},
	}

	handler := proxyHandler(fake, newTestConfig())

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if got := atomic.LoadInt64(&activeConnections); got != 0 {
		t.Fatalf("expected activeConnections to remain 0, got %d", got)
	}

	select {
	case delta := <-activeRollbacks:
		if delta != -1 {
			t.Fatalf("expected ModifyActiveConnections rollback delta -1, got %d", delta)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected ModifyActiveConnections rollback")
	}

	select {
	case delta := <-lifetimeRollbacks:
		if delta != -1 {
			t.Fatalf("expected ModifyLifetimeConnections rollback delta -1, got %d", delta)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected ModifyLifetimeConnections rollback")
	}
}

func TestProxyHandler_ModifyActiveConnectionsErrorIsIgnored(t *testing.T) {
	atomic.StoreInt64(&activeConnections, 0)
	t.Cleanup(func() {
		atomic.StoreInt64(&activeConnections, 0)
	})

	backendUpgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := backendUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("backend upgrade failed: %v", err)
		}
		defer conn.Close()

		for {
			msgType, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(msgType, payload); err != nil {
				return
			}
		}
	}))
	defer backend.Close()

	workerEndpoint := "ws" + strings.TrimPrefix(backend.URL, "http")

	modifyCalls := make(chan int64, 1)
	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			return redis.ServerInfo{ID: "worker-1", Endpoint: workerEndpoint}, nil
		},
		modifyActiveConnectionsFunc: func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
			modifyCalls <- delta
			return errors.New("redis failure")
		},
	}

	cfg := newTestConfig()
	cfg.WorkerSelectTimeout = 1

	proxyServer := httptest.NewServer(http.HandlerFunc(proxyHandler(fake, cfg)))
	defer proxyServer.Close()

	proxyURL := "ws" + strings.TrimPrefix(proxyServer.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(proxyURL, nil)
	if err != nil {
		t.Fatalf("failed to dial proxy: %v", err)
	}

	if err := clientConn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatalf("failed to write message: %v", err)
	}

	if err := clientConn.Close(); err != nil {
		t.Fatalf("failed to close client connection: %v", err)
	}

	select {
	case delta := <-modifyCalls:
		if delta != -1 {
			t.Fatalf("expected ModifyActiveConnections delta -1, got %d", delta)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected ModifyActiveConnections to be called")
	}
}

func TestMetricsHandlerReportsActiveConnections(t *testing.T) {
	atomic.StoreInt64(&activeConnections, 37)
	t.Cleanup(func() {
		atomic.StoreInt64(&activeConnections, 0)
	})

	req := httptest.NewRequest("GET", "/metrics", nil)
	resp := httptest.NewRecorder()

	metricsHandler(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, resp.Code)
	}

	var metrics models.MetricsResponse
	if err := json.NewDecoder(resp.Body).Decode(&metrics); err != nil {
		t.Fatalf("failed to decode metrics response: %v", err)
	}

	if metrics.ActiveConnections != 37 {
		t.Fatalf("expected active connections 37, got %d", metrics.ActiveConnections)
	}
}

func TestRunReaperLoopInvokesRedis(t *testing.T) {
	cfg := newTestConfig()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ticks := make(chan time.Time)
	stub := &stubReaperClient{
		returnCounts: []int{0, 2},
		returnErrs:   []error{errors.New("boom"), nil},
		callCh:       make(chan struct{}, 2),
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runReaperLoop(ctx, cfg, stub, ticks)
	}()

	ticks <- time.Now()
	waitForCall(t, stub.callCh)
	ticks <- time.Now()
	waitForCall(t, stub.callCh)

	cancel()
	wg.Wait()

	if got := stub.callCount(); got != 2 {
		t.Fatalf("expected 2 reaper calls, got %d", got)
	}
}

func TestFaviconHandlerReturnsNoContent(t *testing.T) {
	mux := newProxyMux(newTestConfig(), &fakeRedisClient{})

	req := httptest.NewRequest("GET", "/favicon.ico", nil)
	resp := httptest.NewRecorder()

	mux.ServeHTTP(resp, req)

	if resp.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, resp.Code)
	}
	if resp.Body.Len() != 0 {
		t.Fatalf("expected empty body, got %q", resp.Body.String())
	}
}

func TestRelayStopsOnNormalClosure(t *testing.T) {
	src := &stubWSConn{
		addr: fakeAddr("src"),
		reads: []readResult{{
			err: &websocket.CloseError{Code: websocket.CloseNormalClosure},
		}},
	}
	dst := &stubWSConn{addr: fakeAddr("dst")}

	relay(src, dst, "client->server")

	if len(dst.writes) != 0 {
		t.Fatalf("expected no writes, got %d", len(dst.writes))
	}
}

func TestRelayStopsOnAbnormalClosure(t *testing.T) {
	src := &stubWSConn{
		addr: fakeAddr("src"),
		reads: []readResult{{
			err: &websocket.CloseError{Code: websocket.CloseAbnormalClosure},
		}},
	}
	dst := &stubWSConn{addr: fakeAddr("dst")}

	relay(src, dst, "client->server")

	if len(dst.writes) != 0 {
		t.Fatalf("expected no writes, got %d", len(dst.writes))
	}
}

func TestRelayStopsOnNetErrClosed(t *testing.T) {
	src := &stubWSConn{
		addr:  fakeAddr("src"),
		reads: []readResult{{err: net.ErrClosed}},
	}
	dst := &stubWSConn{addr: fakeAddr("dst")}

	relay(src, dst, "client->server")

	if len(dst.writes) != 0 {
		t.Fatalf("expected no writes, got %d", len(dst.writes))
	}
}

func TestRelayStopsOnWriteFailure(t *testing.T) {
	src := &stubWSConn{
		addr: fakeAddr("src"),
		reads: []readResult{{
			messageType: websocket.TextMessage,
			payload:     []byte("hello"),
		}},
	}
	dst := &stubWSConn{
		addr:        fakeAddr("dst"),
		writeErrors: []error{errors.New("boom")},
	}

	relay(src, dst, "client->server")

	if len(dst.writes) != 1 {
		t.Fatalf("expected 1 write, got %d", len(dst.writes))
	}
	if dst.writes[0].messageType != websocket.TextMessage || string(dst.writes[0].payload) != "hello" {
		t.Fatalf("unexpected write payload: %#v", dst.writes[0])
	}
}

type readResult struct {
	messageType int
	payload     []byte
	err         error
}

type writeCall struct {
	messageType int
	payload     []byte
}

type stubWSConn struct {
	mu          sync.Mutex
	reads       []readResult
	writeErrors []error
	writes      []writeCall
	addr        net.Addr
}

func (s *stubWSConn) ReadMessage() (int, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.reads) == 0 {
		return 0, nil, io.EOF
	}

	next := s.reads[0]
	s.reads = s.reads[1:]
	if next.err != nil {
		return 0, nil, next.err
	}
	return next.messageType, append([]byte(nil), next.payload...), nil
}

func (s *stubWSConn) WriteMessage(messageType int, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.writes = append(s.writes, writeCall{messageType: messageType, payload: append([]byte(nil), data...)})

	if len(s.writeErrors) == 0 {
		return nil
	}

	err := s.writeErrors[0]
	s.writeErrors = s.writeErrors[1:]
	return err
}

func (s *stubWSConn) RemoteAddr() net.Addr {
	return s.addr
}

type fakeNetAddr string

func (f fakeNetAddr) Network() string { return "tcp" }

func (f fakeNetAddr) String() string { return string(f) }

func fakeAddr(label string) net.Addr {
	return fakeNetAddr(label)
}

func waitForCall(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for call")
	}
}

type stubReaperClient struct {
	mu           sync.Mutex
	calls        int
	returnCounts []int
	returnErrs   []error
	callCh       chan struct{}
}

func (s *stubReaperClient) ReapStaleWorkers(ctx context.Context) (int, error) {
	s.mu.Lock()
	idx := s.calls
	s.calls++
	s.mu.Unlock()

	if s.callCh != nil {
		s.callCh <- struct{}{}
	}

	var count int
	if idx < len(s.returnCounts) {
		count = s.returnCounts[idx]
	}

	var err error
	if idx < len(s.returnErrs) {
		err = s.returnErrs[idx]
	}

	return count, err
}

func (s *stubReaperClient) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}
