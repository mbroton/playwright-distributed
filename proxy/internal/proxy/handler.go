package proxy

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"proxy/internal/redis"
	"proxy/pkg/browser"
	"proxy/pkg/config"
	"proxy/pkg/httputils"
	"proxy/pkg/logger"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	retryDelay                = 500 * time.Millisecond
	bookkeepingRedisTimeout   = 5 * time.Second
	supportedWebSocketVersion = "13"
	secWebSocketVersionHeader = "Sec-Websocket-Version"

	notFoundMessage                            = "not found"
	unsupportedBrowserMessage                  = browser.UnsupportedTypeMessage
	unsupportedQueryParametersMessage          = "unsupported query parameters; only browser is allowed"
	websocketUpgradeRequiredMessage            = "websocket upgrade required"
	workerSelectionTimedOutMessage             = "worker selection timed out"
	connectTimedOutAfterSelectingWorkerMessage = "connect timed out after selecting worker"
	selectedWorkerUnavailableMessage           = "selected worker unavailable"
	internalServerErrorMessage                 = "internal server error"
)

type redisClient interface {
	SelectWorker(ctx context.Context, browserType string, excludedWorkerIDs []string) (redis.ServerInfo, error)
	RecordSuccessfulSessionAndTriggerShutdownIfNeeded(ctx context.Context, serverInfo *redis.ServerInfo)
	ModifyActiveConnections(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error
	ModifyAllocatedSessions(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error
}

type wsConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	RemoteAddr() net.Addr
}

type websocketBackendDialer interface {
	DialContext(ctx context.Context, urlStr string, requestHeader http.Header) (*websocket.Conn, *http.Response, error)
}

type websocketBackendDialerFactory func(timeout time.Duration) websocketBackendDialer

type websocketClientUpgrader interface {
	Upgrade(w http.ResponseWriter, r *http.Request, responseHeader http.Header) (*websocket.Conn, error)
}

type websocketClientUpgraderFactory func(timeout time.Duration) websocketClientUpgrader

var (
	errWorkerSelectionDeadlineExceeded = errors.New("worker selection deadline exceeded")
	errWorkerSelectionCanceled         = errors.New("worker selection canceled")
)

func newBookkeepingContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), bookkeepingRedisTimeout)
}

func newTimeoutContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}

func rollbackWorkerCounters(parent context.Context, rd redisClient, server *redis.ServerInfo) {
	ctx, cancel := newBookkeepingContext(parent)
	defer cancel()

	if derr := rd.ModifyActiveConnections(ctx, server, -1); derr != nil {
		logger.Error("Failed to roll back active connections for %s: %v", server.WorkerID(), derr)
	}

	if derr := rd.ModifyAllocatedSessions(ctx, server, -1); derr != nil {
		logger.Error("Failed to roll back allocated sessions for %s: %v", server.WorkerID(), derr)
	}
}

func rollbackWorkerCountersAsync(parent context.Context, rd redisClient, server redis.ServerInfo) {
	// Retry-path rollback must not consume the reselection budget. Temporary
	// counter inflation is preferable to letting Redis bookkeeping delay the
	// next selection attempt beyond PROXY_WORKER_SELECTION_TIMEOUT.
	go rollbackWorkerCounters(parent, rd, &server)
}

func classifyWorkerSelectionError(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return errWorkerSelectionDeadlineExceeded
	case errors.Is(err, context.Canceled):
		return errWorkerSelectionCanceled
	default:
		return err
	}
}

func selectWorkerWithRetry(ctx context.Context, rd redisClient, browserType string) (redis.ServerInfo, error) {
	return selectWorkerWithRetryExcluding(ctx, rd, browserType, nil)
}

func selectWorkerWithRetryExcluding(ctx context.Context, rd redisClient, browserType string, excludedWorkerIDs []string) (redis.ServerInfo, error) {
	ticker := time.NewTicker(retryDelay)
	defer ticker.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return redis.ServerInfo{}, classifyWorkerSelectionError(err)
		}

		server, err := rd.SelectWorker(ctx, browserType, excludedWorkerIDs)
		if err == nil {
			return server, nil
		}

		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return redis.ServerInfo{}, classifyWorkerSelectionError(err)
		}

		if !errors.Is(err, redis.ErrNoAvailableWorkers) {
			return redis.ServerInfo{}, err
		}

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			return redis.ServerInfo{}, classifyWorkerSelectionError(ctx.Err())
		}
	}
}

func appendExcludedWorkerID(excludedWorkerIDs []string, workerID string) []string {
	if workerID == "" {
		return excludedWorkerIDs
	}

	for _, excludedWorkerID := range excludedWorkerIDs {
		if excludedWorkerID == workerID {
			return excludedWorkerIDs
		}
	}

	return append(excludedWorkerIDs, workerID)
}

func defaultBackendDialerFactory(timeout time.Duration) websocketBackendDialer {
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = timeout
	return &dialer
}

func defaultClientUpgraderFactory(timeout time.Duration) websocketClientUpgrader {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
		HandshakeTimeout: timeout,
	}

	return &upgrader
}

func resolveBrowserType(r *http.Request, defaultBrowserType string) (string, int, string, bool) {
	if r.URL.Path != "/" {
		return "", http.StatusNotFound, notFoundMessage, false
	}

	query := r.URL.Query()
	if len(query) == 0 {
		return defaultBrowserType, 0, "", true
	}

	if len(query) != 1 {
		return "", http.StatusBadRequest, unsupportedQueryParametersMessage, false
	}

	values, ok := query["browser"]
	if !ok {
		return "", http.StatusBadRequest, unsupportedQueryParametersMessage, false
	}

	if len(values) != 1 || !browser.IsSupportedType(values[0]) {
		return "", http.StatusBadRequest, unsupportedBrowserMessage, false
	}

	return values[0], 0, "", true
}

func headerTokenContainsValue(header http.Header, name, expectedValue string) bool {
	for _, rawValue := range header.Values(name) {
		for _, token := range strings.Split(rawValue, ",") {
			if strings.EqualFold(strings.TrimSpace(token), expectedValue) {
				return true
			}
		}
	}

	return false
}

func isValidWebSocketChallengeKey(challengeKey string) bool {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(challengeKey))
	if err != nil {
		return false
	}

	return len(decoded) == 16
}

func writeClientHandshakeError(w http.ResponseWriter, status int) {
	w.Header().Set(secWebSocketVersionHeader, supportedWebSocketVersion)
	http.Error(w, http.StatusText(status), status)
}

func validateClientHandshake(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		writeClientHandshakeError(w, http.StatusMethodNotAllowed)
		return false
	}

	if !headerTokenContainsValue(r.Header, secWebSocketVersionHeader, supportedWebSocketVersion) {
		writeClientHandshakeError(w, http.StatusBadRequest)
		return false
	}

	if !isValidWebSocketChallengeKey(r.Header.Get("Sec-WebSocket-Key")) {
		writeClientHandshakeError(w, http.StatusBadRequest)
		return false
	}

	return true
}

func remainingTimeout(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}

	timeout := time.Until(deadline)
	if timeout < 0 {
		return 0
	}

	return timeout
}

func newConnectAttemptContext(
	requestCtx context.Context,
	selectionCtx context.Context,
	connectTimeout time.Duration,
	isRetry bool,
) (context.Context, context.CancelFunc) {
	// The first selected-worker handoff keeps the full connect timeout. Only
	// retries after fast selected-worker failures are bounded by the remaining
	// selection budget, so reselection cannot extend PROXY_WORKER_SELECTION_TIMEOUT.
	if isRetry {
		return newTimeoutContext(selectionCtx, connectTimeout)
	}

	return newTimeoutContext(requestCtx, connectTimeout)
}

func isTimeoutLikeError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return true
	}

	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func refreshPreUpgradeWriteDeadline(w http.ResponseWriter, cfg *config.Config) {
	// net/http sets a request write deadline once headers are read. Refresh it
	// when entering the post-selection handoff so a long queue wait does not
	// consume the whole budget for returning a connect-phase HTTP error.
	err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(proxyHTTPWriteTimeout(cfg)))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		logger.Debug("Failed to refresh write deadline before worker handoff: %v", err)
	}
}

func proxyHandler(rd redisClient, cfg *config.Config) http.HandlerFunc {
	return proxyHandlerWithConnectionFactories(rd, cfg, defaultBackendDialerFactory, defaultClientUpgraderFactory)
}

func proxyHandlerWithBackendDialer(rd redisClient, cfg *config.Config, dialerFactory websocketBackendDialerFactory) http.HandlerFunc {
	return proxyHandlerWithConnectionFactories(rd, cfg, dialerFactory, defaultClientUpgraderFactory)
}

func proxyHandlerWithConnectionFactories(
	rd redisClient,
	cfg *config.Config,
	dialerFactory websocketBackendDialerFactory,
	upgraderFactory websocketClientUpgraderFactory,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		browserType, statusCode, message, ok := resolveBrowserType(r, cfg.DefaultBrowserType)
		if !ok {
			httputils.ErrorResponse(w, statusCode, message)
			return
		}

		if !websocket.IsWebSocketUpgrade(r) {
			httputils.ErrorResponse(w, http.StatusUpgradeRequired, websocketUpgradeRequiredMessage)
			return
		}

		if !validateClientHandshake(w, r) {
			return
		}

		selectionTimeout := proxyWorkerSelectionTimeout(cfg)
		selectionCtx, cancel := newTimeoutContext(r.Context(), selectionTimeout)
		defer cancel()

		connectTimeout := proxyConnectTimeout(cfg)
		excludedWorkerIDs := []string(nil)
		sawSelectedWorkerFailure := false

		var (
			server        redis.ServerInfo
			serverConn    *websocket.Conn
			connectCtx    context.Context
			connectCancel context.CancelFunc
		)

		for {
			server, err := selectWorkerWithRetryExcluding(selectionCtx, rd, browserType, excludedWorkerIDs)
			if err != nil {
				switch {
				case errors.Is(err, errWorkerSelectionDeadlineExceeded):
					if sawSelectedWorkerFailure {
						logger.Error(
							"Connection from %s rejected. Selection budget expired after selected worker failures over %v.",
							r.RemoteAddr,
							selectionTimeout,
						)
						httputils.ErrorResponse(w, http.StatusServiceUnavailable, selectedWorkerUnavailableMessage)
						return
					}

					logger.Error(
						"Connection from %s rejected. Worker selection timed out after %v.",
						r.RemoteAddr,
						selectionTimeout,
					)
					httputils.ErrorResponse(w, http.StatusServiceUnavailable, workerSelectionTimedOutMessage)
				case errors.Is(err, errWorkerSelectionCanceled):
					logger.Debug("Connection from %s canceled during worker selection.", r.RemoteAddr)
				default:
					logger.Error(
						"Connection from %s rejected. An unexpected error occurred while selecting a worker: %v",
						r.RemoteAddr,
						err,
					)
					httputils.ErrorResponse(w, http.StatusInternalServerError, internalServerErrorMessage)
				}
				return
			}

			// Waiting for capacity is the main user-facing timeout. The first
			// selected-worker handoff keeps the full connect timeout. Later retry
			// attempts are capped by the remaining selection budget so reselection
			// cannot extend PROXY_WORKER_SELECTION_TIMEOUT.
			refreshPreUpgradeWriteDeadline(w, cfg)

			connectCtx, connectCancel = newConnectAttemptContext(
				r.Context(),
				selectionCtx,
				connectTimeout,
				sawSelectedWorkerFailure,
			)

			backendURL, parseErr := url.Parse(server.Endpoint)
			if parseErr != nil {
				connectCancel()
				logger.Error("Connection from %s rejected. Worker endpoint for %s is invalid: %v", r.RemoteAddr, server.WorkerID(), parseErr)
				rollbackWorkerCountersAsync(r.Context(), rd, server)
				excludedWorkerIDs = appendExcludedWorkerID(excludedWorkerIDs, server.WorkerID())
				sawSelectedWorkerFailure = true
				continue
			}

			backendTimeout := remainingTimeout(connectCtx)
			if backendTimeout <= 0 {
				connectCancel()
				logger.Error("Connection from %s rejected. Connect timed out after selecting worker %s.", r.RemoteAddr, server.WorkerID())
				rollbackWorkerCounters(r.Context(), rd, &server)
				httputils.ErrorResponse(w, http.StatusServiceUnavailable, connectTimedOutAfterSelectingWorkerMessage)
				return
			}

			serverConn, _, err = dialerFactory(backendTimeout).DialContext(connectCtx, backendURL.String(), nil)
			if err == nil {
				break
			}

			connectCancel()
			if isTimeoutLikeError(err) {
				logger.Error("Connection from %s rejected. Connect timed out while dialing selected worker %s.", r.RemoteAddr, server.WorkerID())
				rollbackWorkerCounters(r.Context(), rd, &server)
				httputils.ErrorResponse(w, http.StatusServiceUnavailable, connectTimedOutAfterSelectingWorkerMessage)
				return
			}
			if errors.Is(err, context.Canceled) {
				logger.Debug("Connection from %s canceled while dialing selected worker %s.", r.RemoteAddr, server.WorkerID())
				rollbackWorkerCounters(r.Context(), rd, &server)
				return
			}

			logger.Error("Connection from %s rejected. Failed to connect to selected worker %s: %v", r.RemoteAddr, server.WorkerID(), err)
			rollbackWorkerCountersAsync(r.Context(), rd, server)
			excludedWorkerIDs = appendExcludedWorkerID(excludedWorkerIDs, server.WorkerID())
			sawSelectedWorkerFailure = true
		}
		defer serverConn.Close()
		defer connectCancel()

		clientHandshakeTimeout := remainingTimeout(connectCtx)
		if clientHandshakeTimeout <= 0 {
			logger.Error("Connection from %s rejected. Connect timed out before client upgrade after selecting worker %s.", r.RemoteAddr, server.WorkerID())
			rollbackWorkerCounters(r.Context(), rd, &server)
			httputils.ErrorResponse(w, http.StatusServiceUnavailable, connectTimedOutAfterSelectingWorkerMessage)
			return
		}

		clientConn, err := upgraderFactory(clientHandshakeTimeout).Upgrade(w, r, nil)
		if err != nil {
			rollbackWorkerCounters(r.Context(), rd, &server)
			// Gorilla may already have hijacked the socket before returning an
			// error, so keep the detailed cause in logs and cleanup only. Do not
			// promise a fresh JSON HTTP error once Upgrade has started.
			if isTimeoutLikeError(err) {
				logger.Error("Connection from %s rejected. Client upgrade timed out after selecting worker %s: %v", r.RemoteAddr, server.WorkerID(), err)
				return
			}
			if errors.Is(err, context.Canceled) {
				logger.Debug("Connection from %s canceled during client upgrade after selecting worker %s.", r.RemoteAddr, server.WorkerID())
				return
			}
			logger.Error("Connection from %s rejected. Failed to upgrade client connection for selected worker %s: %v", r.RemoteAddr, server.WorkerID(), err)
			return
		}
		defer clientConn.Close()

		go func() {
			ctx, cancel := newBookkeepingContext(r.Context())
			defer cancel()
			// Shutdown must be driven by committed successful sessions, not the
			// optimistic selector allocation counter. Async rollback can leave the
			// allocation counter temporarily inflated, but it must not drain a
			// healthy worker one session early.
			rd.RecordSuccessfulSessionAndTriggerShutdownIfNeeded(ctx, &server)
		}()

		atomic.AddInt64(&activeConnections, 1)
		logger.Info("New connection from %s", r.RemoteAddr)
		logger.Debug("Proxy connection established (%s <-> %s)", r.RemoteAddr, server.Endpoint)
		defer func() {
			atomic.AddInt64(&activeConnections, -1)

			// `rd.SelectWorker` reserves one active slot during selection.
			ctx, cancel := newBookkeepingContext(r.Context())
			defer cancel()
			if err := rd.ModifyActiveConnections(ctx, &server, -1); err != nil {
				logger.Error("Failed to decrement active connections for %s: %v", server.WorkerID(), err)
			}
			logger.Debug("Proxy connection closed (%s <-> %s)", r.RemoteAddr, server.Endpoint)
		}()

		done := make(chan struct{})
		var once sync.Once

		go func() {
			relay(clientConn, serverConn, "client->server")
			once.Do(func() {
				close(done)
			})
		}()

		go func() {
			relay(serverConn, clientConn, "server->client")
			once.Do(func() {
				close(done)
			})
		}()

		<-done
	}
}

func relay(src wsConn, dst wsConn, direction string) {
	srcAddr := src.RemoteAddr()
	dstAddr := dst.RemoteAddr()

	for {
		msgType, message, err := src.ReadMessage()
		if err != nil {
			if e, ok := err.(*websocket.CloseError); ok {
				switch e.Code {
				case websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived:
					logger.Debug("Connection closed normally (%s): %v", direction, err)

				case websocket.CloseAbnormalClosure:
					logger.Debug("Connection closed abnormally (%s): %v", direction, err)

				default:
					logger.Error("Unexpected websocket close error (%s): %v", direction, err)
				}
			} else if errors.Is(err, net.ErrClosed) {
				logger.Debug("Connection closed by proxy teardown (%s)", direction)
			} else {
				logger.Error("Unexpected network error in relay (%s): %v", direction, err)
			}
			return
		}

		err = dst.WriteMessage(msgType, message)
		if err != nil {
			logger.Error("Failed to relay message (%s): %v", direction, err)
			return
		}

		logger.Debug("Relayed %s->%s: %d bytes", srcAddr, dstAddr, len(message))
	}
}
