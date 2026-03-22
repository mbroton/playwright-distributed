package proxy

import (
	"context"
	"net/http"
	"proxy/internal/models"
	"proxy/internal/redis"
	"proxy/pkg/config"
	"proxy/pkg/httputils"
	"proxy/pkg/logger"
	"sync/atomic"
	"time"
)

var (
	activeConnections int64
)

const (
	httpWriteTimeoutSlack = 1 * time.Second
	httpIdleTimeout       = 60 * time.Second
)

type reaperClient interface {
	ReapStaleWorkers(ctx context.Context) (int, error)
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	count := atomic.LoadInt64(&activeConnections)
	httputils.JSONResponse(w, 200, models.MetricsResponse{
		ActiveConnections: count,
	})
}

func faviconHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func newProxyMux(cfg *config.Config, rd redisClient) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", metricsHandler)
	mux.HandleFunc("/favicon.ico", faviconHandler)
	mux.HandleFunc("/", proxyHandler(rd, cfg))
	return mux
}

func runReaperLoop(ctx context.Context, cfg *config.Config, rd reaperClient, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			logger.Debug("Running reaper to clean up stale workers...")
			reapedCount, err := rd.ReapStaleWorkers(ctx)
			if err != nil {
				logger.Error("Reaper error: %v", err)
				continue
			}
			if reapedCount > 0 {
				logger.Info("Reaper cleaned up %d stale worker(s)", reapedCount)
			} else {
				logger.Debug("Reaper found no stale workers to clean up.")
			}
		}
	}
}

func proxyReadHeaderTimeout(cfg *config.Config) time.Duration {
	return time.Duration(cfg.ProxyReadHeaderTimeout) * time.Second
}

func proxyWorkerSelectionTimeout(cfg *config.Config) time.Duration {
	return time.Duration(cfg.ProxyWorkerSelectionTimeout) * time.Second
}

func proxyConnectTimeout(cfg *config.Config) time.Duration {
	return time.Duration(cfg.ProxyConnectTimeout) * time.Second
}

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}

	return right
}

func proxyHTTPWriteTimeout(cfg *config.Config) time.Duration {
	// Keep these deadlines request-scoped. Connection-scoped timeout accounting
	// breaks on keep-alive reuse because later requests inherit stale timing.
	return maxDuration(proxyWorkerSelectionTimeout(cfg), proxyConnectTimeout(cfg)) + httpWriteTimeoutSlack
}

func newHTTPServer(cfg *config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              ":8080",
		ReadHeaderTimeout: proxyReadHeaderTimeout(cfg),
		WriteTimeout:      proxyHTTPWriteTimeout(cfg),
		IdleTimeout:       httpIdleTimeout,
		Handler:           handler,
	}
}

func StartProxyServer(cfg *config.Config, rd *redis.Client) {
	mux := newProxyMux(cfg, rd)

	server := newHTTPServer(cfg, mux)

	go func() {
		ticker := time.NewTicker(time.Duration(cfg.ReaperRunInterval) * time.Second)
		defer ticker.Stop()

		runReaperLoop(context.Background(), cfg, rd, ticker.C)
	}()

	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			logger.Debug("Active connections: %d", atomic.LoadInt64(&activeConnections))
		}
	}()

	logger.Info("Starting proxy server at 0.0.0.0:8080")
	server.ListenAndServe()
}
