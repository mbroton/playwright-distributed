package proxy

import (
	"bufio"
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

const validWebSocketKey = "dGhlIHNhbXBsZSBub25jZQ=="

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
		DefaultBrowserType:          "chromium",
		ProxyReadHeaderTimeout:      1,
		ProxyWorkerSelectionTimeout: 1,
		ProxyConnectTimeout:         1,
	}
}

func TestProxyHandler_InvalidBrowserType(t *testing.T) {
	handler := proxyHandler(&fakeRedisClient{}, newTestConfig())

	req := httptest.NewRequest("GET", "/?browser=opera", nil)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	assertJSONErrorResponse(t, resp, http.StatusBadRequest, unsupportedBrowserMessage)
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

	assertJSONErrorResponse(t, resp, http.StatusNotFound, notFoundMessage)
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
	req.Header.Set("Sec-WebSocket-Key", validWebSocketKey)
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
			req.Header.Set("Sec-WebSocket-Key", validWebSocketKey)
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

func TestProxyHandler_RejectsDuplicateBrowserQueryValues(t *testing.T) {
	handler := proxyHandler(&fakeRedisClient{}, newTestConfig())

	req := httptest.NewRequest("GET", "/?browser=chromium&browser=firefox", nil)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	assertJSONErrorResponse(t, resp, http.StatusBadRequest, unsupportedBrowserMessage)
}

func TestProxyHandler_RejectsUnsupportedQueryParameters(t *testing.T) {
	handler := proxyHandler(&fakeRedisClient{}, newTestConfig())

	req := httptest.NewRequest("GET", "/?browser=chromium&foo=bar", nil)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	assertJSONErrorResponse(t, resp, http.StatusBadRequest, unsupportedQueryParametersMessage)
}

func TestProxyHandler_NonWebSocketRequest(t *testing.T) {
	handler := proxyHandler(&fakeRedisClient{}, newTestConfig())

	req := httptest.NewRequest("GET", "/", nil)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	assertJSONErrorResponse(t, resp, http.StatusUpgradeRequired, websocketUpgradeRequiredMessage)
}

func TestProxyHandler_HandshakePreflightRejectsNonGETMethod(t *testing.T) {
	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			t.Fatal("SelectWorker should not be called for invalid method")
			return redis.ServerInfo{}, nil
		},
	}

	handler := proxyHandler(fake, newTestConfig())

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", supportedWebSocketVersion)
	req.Header.Set("Sec-WebSocket-Key", validWebSocketKey)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	assertPlainTextStatusResponse(t, resp, http.StatusMethodNotAllowed)
	if got := resp.Header().Get(secWebSocketVersionHeader); got != supportedWebSocketVersion {
		t.Fatalf("expected %s header %q, got %q", secWebSocketVersionHeader, supportedWebSocketVersion, got)
	}
}

func TestProxyHandler_HandshakePreflightRejectsUnsupportedVersion(t *testing.T) {
	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			t.Fatal("SelectWorker should not be called for unsupported version")
			return redis.ServerInfo{}, nil
		},
	}

	handler := proxyHandler(fake, newTestConfig())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "12")
	req.Header.Set("Sec-WebSocket-Key", validWebSocketKey)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	assertPlainTextStatusResponse(t, resp, http.StatusBadRequest)
	if got := resp.Header().Get(secWebSocketVersionHeader); got != supportedWebSocketVersion {
		t.Fatalf("expected %s header %q, got %q", secWebSocketVersionHeader, supportedWebSocketVersion, got)
	}
}

func TestProxyHandler_HandshakePreflightRejectsInvalidKey(t *testing.T) {
	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			t.Fatal("SelectWorker should not be called for invalid key")
			return redis.ServerInfo{}, nil
		},
	}

	handler := proxyHandler(fake, newTestConfig())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", supportedWebSocketVersion)
	req.Header.Set("Sec-WebSocket-Key", "invalid")
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	assertPlainTextStatusResponse(t, resp, http.StatusBadRequest)
	if got := resp.Header().Get(secWebSocketVersionHeader); got != supportedWebSocketVersion {
		t.Fatalf("expected %s header %q, got %q", secWebSocketVersionHeader, supportedWebSocketVersion, got)
	}
}

func TestValidateClientHandshake_ParallelsGorillaForMalformedRequests(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		version string
		key     string
	}{
		{
			name:    "wrong method",
			method:  http.MethodPost,
			version: supportedWebSocketVersion,
			key:     validWebSocketKey,
		},
		{
			name:    "unsupported version",
			method:  http.MethodGet,
			version: "12",
			key:     validWebSocketKey,
		},
		{
			name:    "invalid key",
			method:  http.MethodGet,
			version: supportedWebSocketVersion,
			key:     "invalid",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			expectedReq := newWebSocketUpgradeRequest(tc.method)
			expectedReq.Header.Set("Sec-WebSocket-Version", tc.version)
			expectedReq.Header.Set("Sec-WebSocket-Key", tc.key)

			actualReq := newWebSocketUpgradeRequest(tc.method)
			actualReq.Header.Set("Sec-WebSocket-Version", tc.version)
			actualReq.Header.Set("Sec-WebSocket-Key", tc.key)

			expectedResp := httptest.NewRecorder()
			upgrader := websocket.Upgrader{
				CheckOrigin: func(r *http.Request) bool {
					return true
				},
			}
			_, _ = upgrader.Upgrade(expectedResp, expectedReq, nil)

			actualResp := httptest.NewRecorder()
			if validateClientHandshake(actualResp, actualReq) {
				t.Fatal("expected handshake validation to fail")
			}

			if actualResp.Code != expectedResp.Code {
				t.Fatalf("expected status %d, got %d", expectedResp.Code, actualResp.Code)
			}
			if actualResp.Body.String() != expectedResp.Body.String() {
				t.Fatalf("expected body %q, got %q", expectedResp.Body.String(), actualResp.Body.String())
			}
			if actualResp.Header().Get("Content-Type") != expectedResp.Header().Get("Content-Type") {
				t.Fatalf("expected content type %q, got %q", expectedResp.Header().Get("Content-Type"), actualResp.Header().Get("Content-Type"))
			}
			if actualResp.Header().Get(secWebSocketVersionHeader) != expectedResp.Header().Get(secWebSocketVersionHeader) {
				t.Fatalf(
					"expected %s header %q, got %q",
					secWebSocketVersionHeader,
					expectedResp.Header().Get(secWebSocketVersionHeader),
					actualResp.Header().Get(secWebSocketVersionHeader),
				)
			}
		})
	}
}

func TestProxyHandler_WorkerSelectionTimeoutReturnsStructuredError(t *testing.T) {
	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			return redis.ServerInfo{}, redis.ErrNoAvailableWorkers
		},
	}

	handler := proxyHandler(fake, newTestConfig())

	req := newWebSocketUpgradeRequest(http.MethodGet)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	assertJSONErrorResponse(t, resp, http.StatusServiceUnavailable, workerSelectionTimedOutMessage)
}

func TestProxyHandler_CanceledSelectionDoesNotWriteStructuredResponse(t *testing.T) {
	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			t.Fatal("SelectWorker should not be called for an already canceled request")
			return redis.ServerInfo{}, nil
		},
	}

	handler := proxyHandler(fake, newTestConfig())

	req := newWebSocketUpgradeRequest(http.MethodGet)
	canceledCtx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(canceledCtx)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Body.Len() != 0 {
		t.Fatalf("expected no response body for canceled selection, got %q", resp.Body.String())
	}
	if contentType := resp.Header().Get("Content-Type"); contentType != "" {
		t.Fatalf("expected no response content type for canceled selection, got %q", contentType)
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
	req.Header.Set("Sec-WebSocket-Key", validWebSocketKey)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	assertJSONErrorResponse(t, resp, http.StatusInternalServerError, internalServerErrorMessage)
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

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	server, err := selectWorkerWithRetry(ctx, fake, "chromium")
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
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	_, err := selectWorkerWithRetry(ctx, fake, "chromium")
	if !errors.Is(err, errWorkerSelectionDeadlineExceeded) {
		t.Fatalf("expected worker selection deadline error, got %v", err)
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
			return redis.ServerInfo{}, redis.ErrNoAvailableWorkers
		},
	}

	parent, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := selectWorkerWithRetry(parent, fake, "chromium")
	if !errors.Is(err, errWorkerSelectionCanceled) {
		t.Fatalf("expected worker selection canceled error, got %v", err)
	}

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("expected SelectWorker not to be called, got %d", got)
	}
}

func TestSelectWorkerWithRetryUnexpectedError(t *testing.T) {
	expectedErr := errors.New("boom")
	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			return redis.ServerInfo{}, expectedErr
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := selectWorkerWithRetry(ctx, fake, "chromium")
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected unexpected selector error %v, got %v", expectedErr, err)
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
	cfg.ProxyConnectTimeout = 1

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

func TestProxyHandler_SuccessfulConnectionCleanupUsesDetachedBookkeepingContext(t *testing.T) {
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
	modifyCalls := make(chan redisModifyCall, 1)

	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			return redis.ServerInfo{ID: "worker-1", Endpoint: workerEndpoint}, nil
		},
		modifyActiveConnectionsFunc: func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
			modifyCalls <- redisModifyCall{delta: delta, ctxErr: ctx.Err()}
			return nil
		},
	}

	cfg := newTestConfig()
	cfg.ProxyConnectTimeout = 1

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
	if _, _, err := clientConn.ReadMessage(); err != nil {
		t.Fatalf("failed to read echo message: %v", err)
	}

	time.Sleep(time.Duration(cfg.ProxyConnectTimeout)*time.Second + 200*time.Millisecond)

	if err := clientConn.Close(); err != nil {
		t.Fatalf("failed to close client connection: %v", err)
	}

	select {
	case call := <-modifyCalls:
		if call.delta != -1 {
			t.Fatalf("expected ModifyActiveConnections delta -1, got %d", call.delta)
		}
		if call.ctxErr != nil {
			t.Fatalf("expected detached bookkeeping context, got %v", call.ctxErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected ModifyActiveConnections to be called")
	}
}

func TestProxyHandler_TriggerWorkerShutdownUsesDetachedBookkeepingContext(t *testing.T) {
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
	triggerCtxErr := make(chan error, 1)

	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			return redis.ServerInfo{ID: "worker-1", Endpoint: workerEndpoint}, nil
		},
		triggerWorkerShutdownFunc: func(ctx context.Context, serverInfo *redis.ServerInfo) {
			time.Sleep(1200 * time.Millisecond)
			triggerCtxErr <- ctx.Err()
		},
		modifyActiveConnectionsFunc: func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
			return nil
		},
	}

	cfg := newTestConfig()
	cfg.ProxyConnectTimeout = 1

	proxyServer := httptest.NewServer(http.HandlerFunc(proxyHandler(fake, cfg)))
	defer proxyServer.Close()

	proxyURL := "ws" + strings.TrimPrefix(proxyServer.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(proxyURL, nil)
	if err != nil {
		t.Fatalf("failed to dial proxy: %v", err)
	}
	defer clientConn.Close()

	if err := clientConn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("failed to send message through proxy: %v", err)
	}

	if err := clientConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("failed to set read deadline: %v", err)
	}
	if _, _, err := clientConn.ReadMessage(); err != nil {
		t.Fatalf("failed to read echo message: %v", err)
	}

	select {
	case err := <-triggerCtxErr:
		if err != nil {
			t.Fatalf("expected detached bookkeeping context, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("expected TriggerWorkerShutdownIfNeeded to be called")
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
	req.Header.Set("Sec-WebSocket-Key", validWebSocketKey)

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	assertJSONErrorResponse(t, resp, http.StatusServiceUnavailable, selectedWorkerUnavailableMessage)

	// TriggerWorkerShutdownIfNeeded should NOT be called on backend dial failure
	// because the connection never succeeded and counters will be rolled back
	select {
	case <-shutdownCalled:
		t.Fatal("TriggerWorkerShutdownIfNeeded should not be called when backend dial fails")
	case <-time.After(100 * time.Millisecond):
		// Expected: shutdown should not be triggered
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

func TestProxyHandler_BackendDialFailureRollbackUsesDetachedBookkeepingContext(t *testing.T) {
	activeRollback := make(chan redisModifyCall, 1)
	lifetimeRollback := make(chan redisModifyCall, 1)

	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			return redis.ServerInfo{
				ID:          "worker-1",
				BrowserType: "chromium",
				Endpoint:    "ws://worker.invalid/playwright",
			}, nil
		},
		modifyActiveConnectionsFunc: func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
			activeRollback <- redisModifyCall{delta: delta, ctxErr: ctx.Err()}
			return nil
		},
		modifyLifetimeConnectionsFunc: func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
			lifetimeRollback <- redisModifyCall{delta: delta, ctxErr: ctx.Err()}
			return nil
		},
	}

	handler := proxyHandlerWithBackendDialer(fake, newTestConfig(), func(timeout time.Duration) websocketBackendDialer {
		return &stubBackendDialer{
			dialContextFunc: func(ctx context.Context, urlStr string, requestHeader http.Header) (*websocket.Conn, *http.Response, error) {
				<-ctx.Done()
				return nil, nil, ctx.Err()
			},
		}
	})

	parentCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/", nil).WithContext(parentCtx)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", validWebSocketKey)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	assertJSONErrorResponse(t, resp, http.StatusServiceUnavailable, connectTimedOutAfterSelectingWorkerMessage)

	select {
	case call := <-activeRollback:
		if call.delta != -1 {
			t.Fatalf("expected ModifyActiveConnections delta -1, got %d", call.delta)
		}
		if call.ctxErr != nil {
			t.Fatalf("expected detached bookkeeping context for active rollback, got %v", call.ctxErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected ModifyActiveConnections rollback")
	}

	select {
	case call := <-lifetimeRollback:
		if call.delta != -1 {
			t.Fatalf("expected ModifyLifetimeConnections delta -1, got %d", call.delta)
		}
		if call.ctxErr != nil {
			t.Fatalf("expected detached bookkeeping context for lifetime rollback, got %v", call.ctxErr)
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
	req.Header.Set("Sec-WebSocket-Key", validWebSocketKey)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	assertPlainTextStatusResponse(t, resp, http.StatusInternalServerError)

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
	cfg.ProxyConnectTimeout = 1

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

func TestProxyHandler_BackendDialHonorsRequestDeadline(t *testing.T) {
	dialStarted := make(chan struct{}, 1)
	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			return redis.ServerInfo{
				ID:          "worker-1",
				BrowserType: "chromium",
				Endpoint:    "ws://worker.invalid/playwright",
			}, nil
		},
		modifyActiveConnectionsFunc: func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
			return nil
		},
		modifyLifetimeConnectionsFunc: func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
			return nil
		},
	}

	handler := proxyHandlerWithBackendDialer(fake, newTestConfig(), func(timeout time.Duration) websocketBackendDialer {
		return &stubBackendDialer{
			dialContextFunc: func(ctx context.Context, urlStr string, requestHeader http.Header) (*websocket.Conn, *http.Response, error) {
				dialStarted <- struct{}{}
				<-ctx.Done()
				return nil, nil, ctx.Err()
			},
		}
	})

	parentCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/", nil).WithContext(parentCtx)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", validWebSocketKey)
	resp := httptest.NewRecorder()

	start := time.Now()
	handler.ServeHTTP(resp, req)
	elapsed := time.Since(start)

	select {
	case <-dialStarted:
	default:
		t.Fatal("expected backend dial to start")
	}

	assertJSONErrorResponse(t, resp, http.StatusServiceUnavailable, connectTimedOutAfterSelectingWorkerMessage)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("expected backend dial to respect request deadline, got %v", elapsed)
	}
}

func TestProxyHandler_BackendDialHandshakeTimeoutReturnsConnectTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer listener.Close()

	acceptedConn := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		acceptedConn <- conn
	}()

	var stalledConn net.Conn
	defer func() {
		if stalledConn != nil {
			_ = stalledConn.Close()
		}
	}()

	activeRollback := make(chan redisModifyCall, 1)
	lifetimeRollback := make(chan redisModifyCall, 1)
	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			return redis.ServerInfo{
				ID:          "worker-1",
				BrowserType: "chromium",
				Endpoint:    "ws://" + listener.Addr().String() + "/playwright",
			}, nil
		},
		modifyActiveConnectionsFunc: func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
			activeRollback <- redisModifyCall{delta: delta, ctxErr: ctx.Err()}
			return nil
		},
		modifyLifetimeConnectionsFunc: func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
			lifetimeRollback <- redisModifyCall{delta: delta, ctxErr: ctx.Err()}
			return nil
		},
	}

	cfg := newTestConfig()
	cfg.ProxyConnectTimeout = 1
	handler := proxyHandler(fake, cfg)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", supportedWebSocketVersion)
	req.Header.Set("Sec-WebSocket-Key", validWebSocketKey)
	resp := httptest.NewRecorder()

	start := time.Now()
	handler.ServeHTTP(resp, req)
	elapsed := time.Since(start)

	select {
	case stalledConn = <-acceptedConn:
	case err := <-acceptErr:
		t.Fatalf("failed to accept backend dial: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("expected backend dial to reach stalled listener")
	}

	assertJSONErrorResponse(t, resp, http.StatusServiceUnavailable, connectTimedOutAfterSelectingWorkerMessage)
	if elapsed < 900*time.Millisecond {
		t.Fatalf("expected backend handshake timeout near configured connect timeout, got %v", elapsed)
	}

	select {
	case call := <-activeRollback:
		if call.delta != -1 {
			t.Fatalf("expected ModifyActiveConnections delta -1, got %d", call.delta)
		}
		if call.ctxErr != nil {
			t.Fatalf("expected detached bookkeeping context for active rollback, got %v", call.ctxErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected ModifyActiveConnections rollback")
	}

	select {
	case call := <-lifetimeRollback:
		if call.delta != -1 {
			t.Fatalf("expected ModifyLifetimeConnections delta -1, got %d", call.delta)
		}
		if call.ctxErr != nil {
			t.Fatalf("expected detached bookkeeping context for lifetime rollback, got %v", call.ctxErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected ModifyLifetimeConnections rollback")
	}
}

func TestProxyHandler_ClientUpgraderReceivesRemainingConnectTimeout(t *testing.T) {
	backendUpgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := backendUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("backend upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer backend.Close()

	workerEndpoint := "ws" + strings.TrimPrefix(backend.URL, "http")
	upgraderTimeouts := make(chan time.Duration, 1)

	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			return redis.ServerInfo{ID: "worker-1", Endpoint: workerEndpoint}, nil
		},
	}

	handler := proxyHandlerWithConnectionFactories(
		fake,
		newTestConfig(),
		func(timeout time.Duration) websocketBackendDialer {
			dialer := defaultBackendDialerFactory(timeout)
			return &stubBackendDialer{
				dialContextFunc: func(ctx context.Context, urlStr string, requestHeader http.Header) (*websocket.Conn, *http.Response, error) {
					time.Sleep(200 * time.Millisecond)
					return dialer.DialContext(ctx, urlStr, requestHeader)
				},
			}
		},
		func(timeout time.Duration) websocketClientUpgrader {
			upgraderTimeouts <- timeout
			return &stubClientUpgrader{
				upgradeFunc: func(w http.ResponseWriter, r *http.Request, responseHeader http.Header) (*websocket.Conn, error) {
					return nil, errors.New("upgrade failed")
				},
			}
		},
	)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", supportedWebSocketVersion)
	req.Header.Set("Sec-WebSocket-Key", validWebSocketKey)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	select {
	case timeout := <-upgraderTimeouts:
		if timeout <= 0 {
			t.Fatalf("expected positive upgrader timeout, got %v", timeout)
		}
		if timeout >= time.Second {
			t.Fatalf("expected remaining timeout less than the full connect timeout, got %v", timeout)
		}
		if timeout > 900*time.Millisecond {
			t.Fatalf("expected backend dial time to reduce remaining timeout, got %v", timeout)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected client upgrader to be constructed")
	}
}

func TestProxyHandler_ClientUpgradeFailureRollsBackAndClosesBackendConnection(t *testing.T) {
	backendClosed := make(chan struct{}, 1)
	backendUpgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := backendUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("backend upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			backendClosed <- struct{}{}
		}
	}))
	defer backend.Close()

	workerEndpoint := "ws" + strings.TrimPrefix(backend.URL, "http")
	activeRollbacks := make(chan redisModifyCall, 1)
	lifetimeRollbacks := make(chan redisModifyCall, 1)

	fake := &fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			return redis.ServerInfo{ID: "worker-1", Endpoint: workerEndpoint}, nil
		},
		modifyActiveConnectionsFunc: func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
			activeRollbacks <- redisModifyCall{delta: delta, ctxErr: ctx.Err()}
			return nil
		},
		modifyLifetimeConnectionsFunc: func(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error {
			lifetimeRollbacks <- redisModifyCall{delta: delta, ctxErr: ctx.Err()}
			return nil
		},
	}

	handler := proxyHandlerWithConnectionFactories(
		fake,
		newTestConfig(),
		defaultBackendDialerFactory,
		func(timeout time.Duration) websocketClientUpgrader {
			return &stubClientUpgrader{
				upgradeFunc: func(w http.ResponseWriter, r *http.Request, responseHeader http.Header) (*websocket.Conn, error) {
					if timeout <= 0 {
						t.Fatalf("expected positive upgrader timeout, got %v", timeout)
					}
					return nil, context.DeadlineExceeded
				},
			}
		},
	)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", supportedWebSocketVersion)
	req.Header.Set("Sec-WebSocket-Key", validWebSocketKey)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Body.Len() != 0 {
		t.Fatalf("expected no HTTP response body after client upgrade failure, got %q", resp.Body.String())
	}
	if contentType := resp.Header().Get("Content-Type"); contentType != "" {
		t.Fatalf("expected no content type after client upgrade failure, got %q", contentType)
	}

	select {
	case call := <-activeRollbacks:
		if call.delta != -1 {
			t.Fatalf("expected ModifyActiveConnections delta -1, got %d", call.delta)
		}
		if call.ctxErr != nil {
			t.Fatalf("expected detached bookkeeping context for active rollback, got %v", call.ctxErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected ModifyActiveConnections rollback")
	}

	select {
	case call := <-lifetimeRollbacks:
		if call.delta != -1 {
			t.Fatalf("expected ModifyLifetimeConnections delta -1, got %d", call.delta)
		}
		if call.ctxErr != nil {
			t.Fatalf("expected detached bookkeeping context for lifetime rollback, got %v", call.ctxErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected ModifyLifetimeConnections rollback")
	}

	select {
	case <-backendClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("expected backend connection to close after client upgrade failure")
	}
}

func TestIsTimeoutLikeError(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "deadline exceeded", err: context.DeadlineExceeded, want: true},
		{name: "timeout net error", err: timeoutNetError{}, want: true},
		{name: "generic error", err: errors.New("boom"), want: false},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := isTimeoutLikeError(tc.err); got != tc.want {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
		})
	}
}

func TestProxyHandler_KeepsWorkerSelectionTimeoutRequestScopedAcrossKeepAliveRequests(t *testing.T) {
	cfg := newTestConfig()
	handler := http.HandlerFunc(proxyHandler(&fakeRedisClient{
		selectWorkerFunc: func(ctx context.Context, browserType string) (redis.ServerInfo, error) {
			return redis.ServerInfo{}, redis.ErrNoAvailableWorkers
		},
	}, cfg))

	server := httptest.NewUnstartedServer(handler)
	server.Config = newHTTPServer(cfg, handler)
	server.Start()
	defer server.Close()

	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial test server: %v", err)
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	requestTarget := "http://" + server.Listener.Addr().String() + "/"
	host := server.Listener.Addr().String()

	sendRequest := func() (time.Duration, *http.Response) {
		t.Helper()

		req, err := http.NewRequest(http.MethodGet, requestTarget, nil)
		if err != nil {
			t.Fatalf("failed to create request: %v", err)
		}

		raw := "GET / HTTP/1.1\r\n" +
			"Host: " + host + "\r\n" +
			"Connection: Upgrade\r\n" +
			"Upgrade: websocket\r\n" +
			"Sec-WebSocket-Version: " + supportedWebSocketVersion + "\r\n" +
			"Sec-WebSocket-Key: " + validWebSocketKey + "\r\n\r\n"

		start := time.Now()
		if _, err := io.WriteString(conn, raw); err != nil {
			t.Fatalf("failed to write request: %v", err)
		}

		resp, err := http.ReadResponse(reader, req)
		if err != nil {
			t.Fatalf("failed to read response: %v", err)
		}

		return time.Since(start), resp
	}

	elapsedFirst, firstResp := sendRequest()
	assertJSONHTTPErrorResponse(t, firstResp, http.StatusServiceUnavailable, workerSelectionTimedOutMessage)
	if elapsedFirst < 900*time.Millisecond {
		t.Fatalf("expected first request to wait for worker selection timeout, got %v", elapsedFirst)
	}

	elapsedSecond, secondResp := sendRequest()
	assertJSONHTTPErrorResponse(t, secondResp, http.StatusServiceUnavailable, workerSelectionTimedOutMessage)
	if elapsedSecond < 900*time.Millisecond {
		t.Fatalf("expected second keep-alive request to get a fresh selection timeout, got %v", elapsedSecond)
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

func assertJSONErrorResponse(t *testing.T, resp *httptest.ResponseRecorder, status int, message string) {
	t.Helper()

	if resp.Code != status {
		t.Fatalf("expected status %d, got %d", status, resp.Code)
	}

	if contentType := resp.Header().Get("Content-Type"); contentType != "application/json; charset=utf-8" {
		t.Fatalf("expected JSON content type, got %q", contentType)
	}

	var body models.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if body.Error.Code != status {
		t.Fatalf("expected error code %d, got %d", status, body.Error.Code)
	}

	if body.Error.Message != message {
		t.Fatalf("expected error message %q, got %q", message, body.Error.Message)
	}
}

func assertPlainTextStatusResponse(t *testing.T, resp *httptest.ResponseRecorder, status int) {
	t.Helper()

	if resp.Code != status {
		t.Fatalf("expected status %d, got %d", status, resp.Code)
	}

	if contentType := resp.Header().Get("Content-Type"); contentType != "text/plain; charset=utf-8" {
		t.Fatalf("expected text/plain content type, got %q", contentType)
	}

	if body := resp.Body.String(); body != http.StatusText(status)+"\n" {
		t.Fatalf("expected body %q, got %q", http.StatusText(status)+"\n", body)
	}
}

func assertJSONHTTPErrorResponse(t *testing.T, resp *http.Response, status int, message string) {
	t.Helper()
	defer resp.Body.Close()

	if resp.StatusCode != status {
		t.Fatalf("expected status %d, got %d", status, resp.StatusCode)
	}

	if contentType := resp.Header.Get("Content-Type"); contentType != "application/json; charset=utf-8" {
		t.Fatalf("expected JSON content type, got %q", contentType)
	}

	var body models.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if body.Error.Code != status {
		t.Fatalf("expected error code %d, got %d", status, body.Error.Code)
	}

	if body.Error.Message != message {
		t.Fatalf("expected error message %q, got %q", message, body.Error.Message)
	}
}

func newWebSocketUpgradeRequest(method string) *http.Request {
	req := httptest.NewRequest(method, "/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", supportedWebSocketVersion)
	req.Header.Set("Sec-WebSocket-Key", validWebSocketKey)
	return req
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

type redisModifyCall struct {
	delta  int64
	ctxErr error
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

type stubBackendDialer struct {
	dialContextFunc func(ctx context.Context, urlStr string, requestHeader http.Header) (*websocket.Conn, *http.Response, error)
}

func (s *stubBackendDialer) DialContext(ctx context.Context, urlStr string, requestHeader http.Header) (*websocket.Conn, *http.Response, error) {
	if s.dialContextFunc == nil {
		return nil, nil, errors.New("unexpected dial")
	}

	return s.dialContextFunc(ctx, urlStr, requestHeader)
}

type stubClientUpgrader struct {
	upgradeFunc func(w http.ResponseWriter, r *http.Request, responseHeader http.Header) (*websocket.Conn, error)
}

func (s *stubClientUpgrader) Upgrade(w http.ResponseWriter, r *http.Request, responseHeader http.Header) (*websocket.Conn, error) {
	if s.upgradeFunc == nil {
		return nil, errors.New("unexpected upgrade")
	}

	return s.upgradeFunc(w, r, responseHeader)
}

type fakeNetAddr string

func (f fakeNetAddr) Network() string { return "tcp" }

func (f fakeNetAddr) String() string { return string(f) }

func fakeAddr(label string) net.Addr {
	return fakeNetAddr(label)
}

type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "timeout" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return false }

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
