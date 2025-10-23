package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"proxy/internal/models"
	"proxy/internal/redis"
	"proxy/pkg/config"
	"proxy/pkg/httputils"
	"proxy/pkg/logger"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	retryDelay = 500 * time.Millisecond
)

type redisClient interface {
	SelectWorker(ctx context.Context, browserType string) (redis.ServerInfo, error)
	TriggerWorkerShutdownIfNeeded(ctx context.Context, serverInfo *redis.ServerInfo)
	ModifyActiveConnections(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error
	ModifyLifetimeConnections(ctx context.Context, serverInfo *redis.ServerInfo, delta int64) error
}

type wsConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	RemoteAddr() net.Addr
}

func rollbackWorkerCounters(ctx context.Context, rd redisClient, server *redis.ServerInfo) {
	if derr := rd.ModifyActiveConnections(ctx, server, -1); derr != nil {
		logger.Error("Failed to roll back active connections for %s: %v", server.WorkerID(), derr)
	}

	if derr := rd.ModifyLifetimeConnections(ctx, server, -1); derr != nil {
		logger.Error("Failed to roll back lifetime connections for %s: %v", server.WorkerID(), derr)
	}
}

func selectWorkerWithRetry(ctx context.Context, rd redisClient, timeout time.Duration, browserType string) (redis.ServerInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(retryDelay)
	defer ticker.Stop()

	for {
		server, err := rd.SelectWorker(ctx, browserType)
		if err == nil {
			return server, nil
		}

		if !errors.Is(err, redis.ErrNoAvailableWorkers) {
			return redis.ServerInfo{}, err
		}

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			return redis.ServerInfo{}, redis.ErrNoAvailableWorkers
		}
	}
}

func selectAndConnectWorker(ctx context.Context, rd redisClient, timeout time.Duration, browserType string, maxAttempts int) (redis.ServerInfo, *websocket.Conn, error) {
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Select a worker from the pool
		server, err := selectWorkerWithRetry(ctx, rd, timeout, browserType)
		if err != nil {
			// No workers available at all - no point retrying
			return redis.ServerInfo{}, nil, err
		}

		// Attempt to connect to the selected worker
		backendURL, _ := url.Parse(server.Endpoint)
		serverConn, _, err := websocket.DefaultDialer.Dial(backendURL.String(), nil)
		if err == nil {
			// Success!
			if attempt > 1 {
				logger.Info("Successfully connected to worker %s on attempt %d/%d", server.WorkerID(), attempt, maxAttempts)
			}
			return server, serverConn, nil
		}

		// Connection failed - log and rollback counters
		lastErr = err
		logger.Error(
			"Failed to connect to worker %s (attempt %d/%d): %v",
			server.WorkerID(),
			attempt,
			maxAttempts,
			err,
		)
		rollbackWorkerCounters(ctx, rd, &server)

		// If we have more attempts, log that we're retrying
		if attempt < maxAttempts {
			logger.Info("Retrying with a different worker...")
		}
	}

	// All attempts exhausted
	logger.Error(
		"Exhausted all %d connection attempts. Last error: %v",
		maxAttempts,
		lastErr,
	)
	return redis.ServerInfo{}, nil, fmt.Errorf("failed to connect to any worker after %d attempts: %w", maxAttempts, lastErr)
}

func proxyHandler(rd redisClient, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		browserType := cfg.DefaultBrowserType
		if b := r.URL.Query().Get("browser"); b != "" {
			switch b {
			case "firefox":
				browserType = "firefox"
			case "chromium":
				browserType = "chromium"
			case "webkit":
				browserType = "webkit"
			default:
				httputils.ErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("Unknown browser type: %s - allowed: chromium, firefox, webkit", b))
				return
			}
		}

		if !websocket.IsWebSocketUpgrade(r) {
			httputils.JSONResponse(w, http.StatusUpgradeRequired, models.MessageResponse{
				Message: "This endpoint is for WebSocket connections only.",
			})
			return
		}

		timeout := time.Duration(cfg.WorkerSelectTimeout) * time.Second
		server, serverConn, err := selectAndConnectWorker(r.Context(), rd, timeout, browserType, cfg.MaxConnectionAttempts)
		if err != nil {
			if errors.Is(err, redis.ErrNoAvailableWorkers) {
				logger.Error(
					"Connection from %s rejected. No workers available after %v timeout: %v",
					r.RemoteAddr,
					timeout,
					err,
				)
				httputils.ErrorResponse(w, http.StatusServiceUnavailable, "No available servers")
			} else {
				logger.Error(
					"Connection from %s rejected. Failed to connect to any worker: %v",
					r.RemoteAddr,
					err,
				)
				httputils.ErrorResponse(w, http.StatusInternalServerError, "Browser server error")
			}
			return
		}
		defer serverConn.Close()

		clientConn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			logger.Error("Failed to upgrade client connection: %v", err)
			rollbackWorkerCounters(r.Context(), rd, &server)
			return
		}
		defer clientConn.Close()

		go rd.TriggerWorkerShutdownIfNeeded(r.Context(), &server)

		atomic.AddInt64(&activeConnections, 1)
		logger.Info("New connection from %s", r.RemoteAddr)
		logger.Debug("Proxy connection established (%s <-> %s)", r.RemoteAddr, server.Endpoint)
		defer func() {
			atomic.AddInt64(&activeConnections, -1)
			// `rd.SelectWorker` is increasing this counter during selection process
			rd.ModifyActiveConnections(r.Context(), &server, -1)
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
