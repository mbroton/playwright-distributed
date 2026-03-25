package redis

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"proxy/pkg/config"
	"proxy/pkg/logger"
	"slices"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	activeConnectionsKey     = "cluster:active_connections"
	allocatedSessionsKey     = "cluster:allocated_sessions"
	successfulSessionsKey    = "cluster:successful_sessions"
	workerShutdownCommandFmt = "worker:cmd:%s"
)

//go:embed selector.lua
var selectorScriptSource string

//go:embed reaper.lua
var reaperScriptSource string

var ErrNoAvailableWorkers = errors.New("no available workers")

const selectorResultFieldCount = 6

type Client struct {
	rd             *redis.Client
	cfg            *config.Config
	selectorScript *redis.Script
	reaperScript   *redis.Script
}

func NewClient(cfg *config.Config) (*Client, error) {
	rd := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%d", cfg.RedisHost, cfg.RedisPort),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rd.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	logger.Info("Connected to Redis")

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

func (c *Client) GetAllocatedSessions(ctx context.Context, workerId string) (int64, error) {
	val, err := c.rd.HGet(ctx, allocatedSessionsKey, workerId).Int64()
	if err == redis.Nil {
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("failed to get allocated sessions for worker %s: %w", workerId, err)
	}

	return val, nil
}

func (c *Client) RecordSuccessfulSessionAndTriggerShutdownIfNeeded(ctx context.Context, serverInfo *ServerInfo) {
	// Shutdown must follow committed successful handoffs, not optimistic
	// selector allocations. Async rollback can leave allocated_sessions
	// temporarily inflated after a failed pre-upgrade handoff.
	currentSuccessful, err := c.rd.HIncrBy(ctx, successfulSessionsKey, serverInfo.WorkerID(), 1).Result()
	if err != nil {
		logger.WithField("workerId", serverInfo.WorkerID()).Errorf("Failed to record successful session: %v", err)
		return
	}

	if currentSuccessful < int64(c.cfg.MaxLifetimeSessions) {
		return
	}

	cmdKey := fmt.Sprintf(workerShutdownCommandFmt, serverInfo.WorkerID())
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
			"Worker has reached successful session limit (%d/%d). Shutdown command sent.",
			currentSuccessful,
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

func (c *Client) ModifyAllocatedSessions(ctx context.Context, serverInfo *ServerInfo, delta int64) error {
	err := c.rd.HIncrBy(ctx, allocatedSessionsKey, serverInfo.WorkerID(), int64(delta)).Err()
	if err != nil {
		return fmt.Errorf("failed to modify allocated sessions for worker %s: %w", serverInfo.WorkerID(), err)
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

func (c *Client) SelectWorker(ctx context.Context, browserType string, excludedWorkerIDs []string) (ServerInfo, error) {
	args := []any{
		c.cfg.MaxConcurrentSessions,
		c.cfg.MaxLifetimeSessions,
		browserType,
		c.cfg.SelectorFreshnessTimeout,
	}
	for _, excludedWorkerID := range excludedWorkerIDs {
		if excludedWorkerID == "" {
			continue
		}
		args = append(args, excludedWorkerID)
	}

	result, err := c.selectorScript.Run(
		ctx,
		c.rd,
		[]string{},
		args...,
	).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return ServerInfo{}, ErrNoAvailableWorkers
		}
		return ServerInfo{}, fmt.Errorf("failed to run selector script: %w", err)
	}

	resultFields, ok := result.([]any)
	if !ok {
		return ServerInfo{}, fmt.Errorf("selector script returned unexpected type: %T", result)
	}

	return parseSelectorResult(resultFields)
}

func parseSelectorResult(resultFields []any) (ServerInfo, error) {
	if len(resultFields) != selectorResultFieldCount {
		return ServerInfo{}, fmt.Errorf("selector script returned %d fields, want %d", len(resultFields), selectorResultFieldCount)
	}

	stringFields := make([]string, len(resultFields))
	for i, rawField := range resultFields {
		field, ok := rawField.(string)
		if !ok {
			return ServerInfo{}, fmt.Errorf("selector script returned non-string field at index %d: %T", i, rawField)
		}
		stringFields[i] = field
	}

	serverInfo := ServerInfo{
		ID:            stringFields[0],
		BrowserType:   stringFields[1],
		Endpoint:      stringFields[2],
		Status:        stringFields[3],
		StartedAt:     stringFields[4],
		LastHeartbeat: stringFields[5],
	}

	if slices.Contains([]string{
		serverInfo.ID,
		serverInfo.BrowserType,
		serverInfo.Endpoint,
		serverInfo.Status,
		serverInfo.StartedAt,
		serverInfo.LastHeartbeat,
	}, "") {
		return ServerInfo{}, fmt.Errorf("selector script returned incomplete worker metadata")
	}

	return serverInfo, nil
}

func (c *Client) ReapStaleWorkers(ctx context.Context) (int, error) {
	workerIDs, err := c.getReapCandidateWorkerIDs(ctx)
	if err != nil {
		return 0, err
	}

	if len(workerIDs) == 0 {
		return 0, nil
	}

	args := make([]any, len(workerIDs))
	for i, v := range workerIDs {
		args[i] = v
	}

	result, err := c.reaperScript.Run(
		ctx,
		c.rd,
		[]string{activeConnectionsKey, allocatedSessionsKey, successfulSessionsKey},
		args...,
	).Result()
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

func (c *Client) getReapCandidateWorkerIDs(ctx context.Context) ([]string, error) {
	hashes := []struct {
		key         string
		description string
	}{
		{key: activeConnectionsKey, description: "active connections"},
		{key: allocatedSessionsKey, description: "allocated sessions"},
		{key: successfulSessionsKey, description: "successful sessions"},
	}

	workerIDSet := make(map[string]struct{})
	for _, hash := range hashes {
		workerIDs, err := c.rd.HKeys(ctx, hash.key).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				continue
			}
			return nil, fmt.Errorf("failed to get worker IDs from %s: %w", hash.description, err)
		}

		for _, workerID := range workerIDs {
			workerIDSet[workerID] = struct{}{}
		}
	}

	workerIDs := make([]string, 0, len(workerIDSet))
	for workerID := range workerIDSet {
		workerIDs = append(workerIDs, workerID)
	}
	slices.Sort(workerIDs)

	return workerIDs, nil
}
