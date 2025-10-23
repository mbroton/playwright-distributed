package redis

import (
	"context"
	"crypto/tls"
	_ "embed"
	"errors"
	"fmt"
	"proxy/pkg/config"
	"proxy/pkg/logger"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	activeConnectionsKey   = "cluster:active_connections"
	lifetimeConnectionsKey = "cluster:lifetime_connections"
)

//go:embed selector.lua
var selectorScriptSource string

//go:embed reaper.lua
var reaperScriptSource string

var ErrNoAvailableWorkers = errors.New("no available workers")

type Client struct {
	rd             *redis.Client
	cfg            *config.Config
	selectorScript *redis.Script
	reaperScript   *redis.Script
}

func NewClient(cfg *config.Config) (*Client, error) {
	opts := &redis.Options{
		Addr:     fmt.Sprintf("%s:%d", cfg.RedisHost, cfg.RedisPort),
		Password: cfg.RedisPassword, // empty string = no auth
	}

	// Enable TLS if configured (uses system CA pool for public certificates)
	if cfg.RedisTLS {
		opts.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	rd := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rd.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	if cfg.RedisTLS {
		logger.Info("Connected to Redis with TLS enabled")
	} else {
		logger.Info("Connected to Redis")
	}

	selector := redis.NewScript(selectorScriptSource)
	reaper := redis.NewScript(reaperScriptSource)

	return &Client{
		rd:             rd,
		cfg:            cfg,
		selectorScript: selector,
		reaperScript:   reaper,
	}, nil
}

func (c *Client) Close() error {
	return c.rd.Close()
}

type ServerInfo struct {
	ID            string `redis:"id"`
	BrowserType   string `redis:"browserType"`
	Endpoint      string `redis:"endpoint"`
	Status        string `redis:"status"`
	StartedAt     string `redis:"startedAt"`
	LastHeartbeat string `redis:"lastHeartbeat"`
}

func (s *ServerInfo) WorkerID() string {
	return fmt.Sprintf("%s:%s", s.BrowserType, s.ID)
}

func (c *Client) GetAllActiveConnections(ctx context.Context) (map[string]int64, error) {
	result := make(map[string]int64)

	vals, err := c.rd.HGetAll(ctx, activeConnectionsKey).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get all active connections: %w", err)
	}

	for workerId, val := range vals {
		count, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			logger.WithField("workerId", workerId).Errorf("Failed to parse active connections count: %v", err)
			continue
		}
		result[workerId] = count
	}

	return result, nil
}

func (c *Client) GetWorkerActiveConnections(ctx context.Context, workerId string) (int64, error) {
	val, err := c.rd.HGet(ctx, activeConnectionsKey, workerId).Int64()
	if err == redis.Nil {
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("failed to get active connections for worker %s: %w", workerId, err)
	}

	return val, nil
}

func (c *Client) GetLifetimeConnections(ctx context.Context, workerId string) (int64, error) {
	val, err := c.rd.HGet(ctx, lifetimeConnectionsKey, workerId).Int64()
	if err == redis.Nil {
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("failed to get lifetime connections for worker %s: %w", workerId, err)
	}

	return val, nil
}

func (c *Client) TriggerWorkerShutdownIfNeeded(ctx context.Context, serverInfo *ServerInfo) {
	currentLifetime, err := c.GetLifetimeConnections(ctx, serverInfo.WorkerID())
	if err != nil {
		logger.WithField("workerId", serverInfo.WorkerID()).Errorf("Could not get lifetime connections: %v", err)
		return
	}

	if currentLifetime < int64(c.cfg.MaxLifetimeSessions) {
		return
	}

	cmdKey := fmt.Sprintf("worker:cmd:%s", serverInfo.WorkerID())
	wasSet, err := c.rd.SetNX(ctx, cmdKey, "shutdown", time.Duration(c.cfg.ShutdownCommandTTL)*time.Second).Result()
	if err != nil {
		logger.WithField("workerId", serverInfo.WorkerID()).Errorf("Failed to set shutdown command: %v", err)
		return
	}

	if wasSet {
		if pubErr := c.rd.Publish(ctx, cmdKey, "shutdown").Err(); pubErr != nil {
			logger.WithField("workerId", serverInfo.WorkerID()).Warnf("Failed to publish shutdown command: %v", pubErr)
		}

		logger.WithField("workerId", serverInfo.WorkerID()).Infof(
			"Worker has reached session limit (%d/%d). Shutdown command sent.",
			currentLifetime,
			c.cfg.MaxLifetimeSessions,
		)
	}
}

func (c *Client) ModifyActiveConnections(ctx context.Context, serverInfo *ServerInfo, delta int64) error {
	err := c.rd.HIncrBy(ctx, activeConnectionsKey, serverInfo.WorkerID(), int64(delta)).Err()
	if err != nil {
		return fmt.Errorf("failed to modify active connections for worker %s: %w", serverInfo.WorkerID(), err)
	}

	return nil
}

func (c *Client) ModifyLifetimeConnections(ctx context.Context, serverInfo *ServerInfo, delta int64) error {
	err := c.rd.HIncrBy(ctx, lifetimeConnectionsKey, serverInfo.WorkerID(), int64(delta)).Err()
	if err != nil {
		return fmt.Errorf("failed to modify lifetime connections for worker %s: %w", serverInfo.WorkerID(), err)
	}

	return nil
}

func (c *Client) GetWorker(ctx context.Context, workerId string) (ServerInfo, error) {
	workerKey := fmt.Sprintf("worker:%s", workerId)
	var serverInfo ServerInfo

	if err := c.rd.HGetAll(ctx, workerKey).Scan(&serverInfo); err != nil {
		return ServerInfo{}, fmt.Errorf("failed to get worker %s: %w", workerId, err)
	}

	if serverInfo.ID == "" {
		return ServerInfo{}, fmt.Errorf("worker %s not found", workerId)
	}

	return serverInfo, nil
}

func (c *Client) SelectWorker(ctx context.Context, browserType string) (ServerInfo, error) {
	result, err := c.selectorScript.Run(
		ctx,
		c.rd,
		[]string{},
		c.cfg.MaxConcurrentSessions,
		c.cfg.MaxLifetimeSessions,
		browserType,
	).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return ServerInfo{}, ErrNoAvailableWorkers
		}
		return ServerInfo{}, fmt.Errorf("failed to run selector script: %w", err)
	}

	workerId, ok := result.(string)
	if !ok {
		return ServerInfo{}, fmt.Errorf("selector script returned unexpected type: %T", result)
	}

	return c.GetWorker(ctx, workerId)
}

func (c *Client) ReapStaleWorkers(ctx context.Context) (int, error) {
	workerIDs, err := c.rd.HKeys(ctx, lifetimeConnectionsKey).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to get worker IDs from lifetime connections: %w", err)
	}

	if len(workerIDs) == 0 {
		return 0, nil
	}

	args := make([]any, len(workerIDs))
	for i, v := range workerIDs {
		args[i] = v
	}

	result, err := c.reaperScript.Run(ctx, c.rd, []string{activeConnectionsKey, lifetimeConnectionsKey}, args...).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to run reaper script: %w", err)
	}

	reapedIDs, ok := result.([]any)
	if !ok {
		return 0, fmt.Errorf("reaper script returned unexpected type: %T", result)
	}

	if len(reapedIDs) > 0 {
		logger.Info("Reaped %d stale worker(s): %v", len(reapedIDs), reapedIDs)
	}

	return len(reapedIDs), nil
}
