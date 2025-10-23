package proxy

import (
	"context"
	"fmt"
	"net/http"
	"proxy/internal/models"
	"proxy/internal/redis"
	"proxy/pkg/config"
	"proxy/pkg/httputils"
	"proxy/pkg/logger"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	activeConnections int64
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

func StartProxyServer(cfg *config.Config, rd *redis.Client) {
	mux := newProxyMux(cfg, rd)

	server := &http.Server{
		Addr:         ":8080",
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
		Handler:      mux,
	}

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
