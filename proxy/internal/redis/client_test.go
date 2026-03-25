package redis

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"proxy/pkg/config"
	"proxy/pkg/logger"
)

func init() {
	logger.Log = logrus.New()
}

type selectorTestWorker struct {
	id            string
	browserType   string
	endpoint      string
	status        string
	startedAt     int64
	lastHeartbeat int64
	active        int64
	allocated     int64
}

type redisTestServer struct {
	addr string
	rd   *goredis.Client
	cmd  *exec.Cmd
}

func startRedisTestServer(t *testing.T) *redisTestServer {
	t.Helper()

	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr != "" {
		rd := goredis.NewClient(&goredis.Options{Addr: addr})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := rd.Ping(ctx).Err(); err != nil {
			t.Fatalf("failed to ping test redis at %s: %v", addr, err)
		}
		if err := rd.FlushDB(ctx).Err(); err != nil {
			t.Fatalf("failed to flush test redis at %s: %v", addr, err)
		}

		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cleanupCancel()
			_ = rd.FlushDB(cleanupCtx).Err()
			_ = rd.Close()
		})

		return &redisTestServer{addr: addr, rd: rd}
	}

	redisServerPath, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server not found and TEST_REDIS_ADDR is not set")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve redis port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("failed to close reserved redis port: %v", err)
	}

	cmd := exec.Command(
		redisServerPath,
		"--save", "",
		"--appendonly", "no",
		"--bind", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--dir", t.TempDir(),
	)

	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start redis-server: %v", err)
	}

	addr = fmt.Sprintf("127.0.0.1:%d", port)
	rd := goredis.NewClient(&goredis.Options{Addr: addr})

	startCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for {
		if err := rd.Ping(startCtx).Err(); err == nil {
			break
		}

		if startCtx.Err() != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("redis-server did not become ready: %v\n%s", startCtx.Err(), stderr.String())
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_ = rd.FlushDB(cleanupCtx).Err()
		_ = rd.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	return &redisTestServer{addr: addr, rd: rd, cmd: cmd}
}

func newSelectorTestClient(t *testing.T, cfg *config.Config, rd *goredis.Client) *Client {
	t.Helper()

	return &Client{
		rd:             rd,
		cfg:            cfg,
		selectorScript: goredis.NewScript(selectorScriptSource),
		reaperScript:   goredis.NewScript(reaperScriptSource),
	}
}

func newSelectorTestConfig() *config.Config {
	return &config.Config{
		MaxConcurrentSessions:    5,
		MaxLifetimeSessions:      10,
		SelectorFreshnessTimeout: 60,
	}
}

func seedWorker(t *testing.T, rd *goredis.Client, worker selectorTestWorker) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	workerID := worker.browserType + ":" + worker.id
	workerKey := "worker:" + workerID

	if err := rd.HSet(ctx, activeConnectionsKey, workerID, worker.active).Err(); err != nil {
		t.Fatalf("failed to seed active connections for %s: %v", workerID, err)
	}
	if err := rd.HSet(ctx, allocatedSessionsKey, workerID, worker.allocated).Err(); err != nil {
		t.Fatalf("failed to seed allocated sessions for %s: %v", workerID, err)
	}
	if err := rd.HSet(ctx, workerKey, map[string]any{
		"id":            worker.id,
		"browserType":   worker.browserType,
		"endpoint":      worker.endpoint,
		"status":        worker.status,
		"startedAt":     worker.startedAt,
		"lastHeartbeat": worker.lastHeartbeat,
	}).Err(); err != nil {
		t.Fatalf("failed to seed worker %s: %v", workerID, err)
	}

	return workerID
}

func seedOrphanCounter(t *testing.T, rd *goredis.Client, workerID string, active, allocated int64) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rd.HSet(ctx, activeConnectionsKey, workerID, active).Err(); err != nil {
		t.Fatalf("failed to seed orphan active counter for %s: %v", workerID, err)
	}
	if err := rd.HSet(ctx, allocatedSessionsKey, workerID, allocated).Err(); err != nil {
		t.Fatalf("failed to seed orphan allocated counter for %s: %v", workerID, err)
	}
}

func TestClient_SelectWorkerFiltersEligibilityAndParsesMetadata(t *testing.T) {
	server := startRedisTestServer(t)
	cfg := newSelectorTestConfig()
	client := newSelectorTestClient(t, cfg, server.rd)
	now := time.Now().UnixMilli()

	seedWorker(t, server.rd, selectorTestWorker{
		id:            "selected",
		browserType:   "chromium",
		endpoint:      "ws://selected/playwright",
		status:        "available",
		startedAt:     now - 10_000,
		lastHeartbeat: now,
		active:        1,
		allocated:     2,
	})
	seedWorker(t, server.rd, selectorTestWorker{
		id:            "firefox-only",
		browserType:   "firefox",
		endpoint:      "ws://firefox/playwright",
		status:        "available",
		startedAt:     now - 20_000,
		lastHeartbeat: now,
		active:        0,
		allocated:     0,
	})
	seedWorker(t, server.rd, selectorTestWorker{
		id:            "draining",
		browserType:   "chromium",
		endpoint:      "ws://draining/playwright",
		status:        "draining",
		startedAt:     now - 20_000,
		lastHeartbeat: now,
		active:        0,
		allocated:     0,
	})
	seedWorker(t, server.rd, selectorTestWorker{
		id:            "shutting-down",
		browserType:   "chromium",
		endpoint:      "ws://shutting-down/playwright",
		status:        "shutting-down",
		startedAt:     now - 20_000,
		lastHeartbeat: now,
		active:        0,
		allocated:     0,
	})
	seedWorker(t, server.rd, selectorTestWorker{
		id:            "stale",
		browserType:   "chromium",
		endpoint:      "ws://stale/playwright",
		status:        "available",
		startedAt:     now - 20_000,
		lastHeartbeat: now - 61_000,
		active:        0,
		allocated:     0,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	selected, err := client.SelectWorker(ctx, "chromium", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if selected.ID != "selected" {
		t.Fatalf("expected worker ID 'selected', got %q", selected.ID)
	}
	if selected.BrowserType != "chromium" {
		t.Fatalf("expected browser type chromium, got %q", selected.BrowserType)
	}
	if selected.Endpoint != "ws://selected/playwright" {
		t.Fatalf("expected endpoint ws://selected/playwright, got %q", selected.Endpoint)
	}
	if selected.Status != "available" {
		t.Fatalf("expected status available, got %q", selected.Status)
	}
	if selected.StartedAt != strconv.FormatInt(now-10_000, 10) {
		t.Fatalf("expected startedAt %d, got %q", now-10_000, selected.StartedAt)
	}
	if selected.LastHeartbeat != strconv.FormatInt(now, 10) {
		t.Fatalf("expected lastHeartbeat %d, got %q", now, selected.LastHeartbeat)
	}
}

func TestClient_SelectWorkerIgnoresOrphanCountersAndUsesEligibleWorkerMargin(t *testing.T) {
	server := startRedisTestServer(t)
	cfg := newSelectorTestConfig()
	client := newSelectorTestClient(t, cfg, server.rd)
	now := time.Now().UnixMilli()

	seedWorker(t, server.rd, selectorTestWorker{
		id:            "tier-two",
		browserType:   "chromium",
		endpoint:      "ws://tier-two/playwright",
		status:        "available",
		startedAt:     now - 20_000,
		lastHeartbeat: now,
		active:        0,
		allocated:     6,
	})
	seedWorker(t, server.rd, selectorTestWorker{
		id:            "tier-one",
		browserType:   "chromium",
		endpoint:      "ws://tier-one/playwright",
		status:        "available",
		startedAt:     now - 10_000,
		lastHeartbeat: now,
		active:        1,
		allocated:     2,
	})
	for i := 0; i < 8; i++ {
		seedOrphanCounter(t, server.rd, fmt.Sprintf("chromium:orphan-%d", i), 0, 0)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	selected, err := client.SelectWorker(ctx, "chromium", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if selected.ID != "tier-one" {
		t.Fatalf("expected eligible-worker margin to select tier-one, got %q", selected.ID)
	}
}

func TestClient_SelectWorkerPrefersTierOneOverTierTwo(t *testing.T) {
	server := startRedisTestServer(t)
	cfg := newSelectorTestConfig()
	client := newSelectorTestClient(t, cfg, server.rd)
	now := time.Now().UnixMilli()

	seedWorker(t, server.rd, selectorTestWorker{
		id:            "tier-two",
		browserType:   "chromium",
		endpoint:      "ws://tier-two/playwright",
		status:        "available",
		startedAt:     now - 20_000,
		lastHeartbeat: now,
		active:        0,
		allocated:     6,
	})
	seedWorker(t, server.rd, selectorTestWorker{
		id:            "tier-one",
		browserType:   "chromium",
		endpoint:      "ws://tier-one/playwright",
		status:        "available",
		startedAt:     now - 10_000,
		lastHeartbeat: now,
		active:        1,
		allocated:     1,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	selected, err := client.SelectWorker(ctx, "chromium", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if selected.ID != "tier-one" {
		t.Fatalf("expected tier-one worker to win, got %q", selected.ID)
	}
}

func TestClient_SelectWorkerPrefersLowerActiveBeforeHigherLifetimeWithinTier(t *testing.T) {
	server := startRedisTestServer(t)
	cfg := newSelectorTestConfig()
	client := newSelectorTestClient(t, cfg, server.rd)
	now := time.Now().UnixMilli()

	seedWorker(t, server.rd, selectorTestWorker{
		id:            "higher-lifetime",
		browserType:   "chromium",
		endpoint:      "ws://higher-lifetime/playwright",
		status:        "available",
		startedAt:     now - 20_000,
		lastHeartbeat: now,
		active:        1,
		allocated:     4,
	})
	seedWorker(t, server.rd, selectorTestWorker{
		id:            "lower-active",
		browserType:   "chromium",
		endpoint:      "ws://lower-active/playwright",
		status:        "available",
		startedAt:     now - 10_000,
		lastHeartbeat: now,
		active:        0,
		allocated:     3,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	selected, err := client.SelectWorker(ctx, "chromium", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if selected.ID != "lower-active" {
		t.Fatalf("expected least-loaded worker to win, got %q", selected.ID)
	}
}

func TestClient_SelectWorkerUsesStartedAtThenIDTieBreaks(t *testing.T) {
	t.Run("oldest startedAt wins", func(t *testing.T) {
		server := startRedisTestServer(t)
		cfg := newSelectorTestConfig()
		client := newSelectorTestClient(t, cfg, server.rd)
		now := time.Now().UnixMilli()

		seedWorker(t, server.rd, selectorTestWorker{
			id:            "older",
			browserType:   "chromium",
			endpoint:      "ws://older/playwright",
			status:        "available",
			startedAt:     now - 20_000,
			lastHeartbeat: now,
			active:        0,
			allocated:     3,
		})
		seedWorker(t, server.rd, selectorTestWorker{
			id:            "newer",
			browserType:   "chromium",
			endpoint:      "ws://newer/playwright",
			status:        "available",
			startedAt:     now - 10_000,
			lastHeartbeat: now,
			active:        0,
			allocated:     3,
		})

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		selected, err := client.SelectWorker(ctx, "chromium", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if selected.ID != "older" {
			t.Fatalf("expected oldest worker to win, got %q", selected.ID)
		}
	})

	t.Run("smallest ID breaks exact ties", func(t *testing.T) {
		server := startRedisTestServer(t)
		cfg := newSelectorTestConfig()
		client := newSelectorTestClient(t, cfg, server.rd)
		now := time.Now().UnixMilli()

		seedWorker(t, server.rd, selectorTestWorker{
			id:            "b-worker",
			browserType:   "chromium",
			endpoint:      "ws://b-worker/playwright",
			status:        "available",
			startedAt:     now - 20_000,
			lastHeartbeat: now,
			active:        0,
			allocated:     3,
		})
		seedWorker(t, server.rd, selectorTestWorker{
			id:            "a-worker",
			browserType:   "chromium",
			endpoint:      "ws://a-worker/playwright",
			status:        "available",
			startedAt:     now - 20_000,
			lastHeartbeat: now,
			active:        0,
			allocated:     3,
		})

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		selected, err := client.SelectWorker(ctx, "chromium", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if selected.ID != "a-worker" {
			t.Fatalf("expected lexicographically smallest worker ID, got %q", selected.ID)
		}
	})
}

func TestClient_SelectWorkerNoEligibleWorkers(t *testing.T) {
	server := startRedisTestServer(t)
	cfg := newSelectorTestConfig()
	client := newSelectorTestClient(t, cfg, server.rd)
	now := time.Now().UnixMilli()

	seedWorker(t, server.rd, selectorTestWorker{
		id:            "busy",
		browserType:   "chromium",
		endpoint:      "ws://busy/playwright",
		status:        "available",
		startedAt:     now - 10_000,
		lastHeartbeat: now,
		active:        int64(cfg.MaxConcurrentSessions),
		allocated:     0,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := client.SelectWorker(ctx, "chromium", nil)
	if !errors.Is(err, ErrNoAvailableWorkers) {
		t.Fatalf("expected ErrNoAvailableWorkers, got %v", err)
	}
}

func TestClient_SelectWorker(t *testing.T) {
	server := startRedisTestServer(t)
	cfg := newSelectorTestConfig()
	client := newSelectorTestClient(t, cfg, server.rd)
	now := time.Now().UnixMilli()

	excludedWorkerID := seedWorker(t, server.rd, selectorTestWorker{
		id:            "excluded",
		browserType:   "chromium",
		endpoint:      "ws://excluded/playwright",
		status:        "available",
		startedAt:     now - 20_000,
		lastHeartbeat: now,
		active:        0,
		allocated:     4,
	})
	seedWorker(t, server.rd, selectorTestWorker{
		id:            "fallback",
		browserType:   "chromium",
		endpoint:      "ws://fallback/playwright",
		status:        "available",
		startedAt:     now - 10_000,
		lastHeartbeat: now,
		active:        0,
		allocated:     3,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	selected, err := client.SelectWorker(ctx, "chromium", []string{excludedWorkerID})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if selected.ID != "fallback" {
		t.Fatalf("expected excluded worker to be skipped, got %q", selected.ID)
	}
}

func TestClient_RecordSuccessfulSessionAndTriggerShutdownIfNeededUsesCommittedCount(t *testing.T) {
	server := startRedisTestServer(t)
	cfg := newSelectorTestConfig()
	cfg.MaxLifetimeSessions = 3
	client := newSelectorTestClient(t, cfg, server.rd)
	now := time.Now().UnixMilli()

	workerID := seedWorker(t, server.rd, selectorTestWorker{
		id:            "worker-1",
		browserType:   "chromium",
		endpoint:      "ws://worker-1/playwright",
		status:        "available",
		startedAt:     now - 20_000,
		lastHeartbeat: now,
		active:        0,
		allocated:     3,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.rd.HSet(ctx, successfulSessionsKey, workerID, 1).Err(); err != nil {
		t.Fatalf("failed to seed successful sessions for %s: %v", workerID, err)
	}

	serverInfo := &ServerInfo{
		ID:          "worker-1",
		BrowserType: "chromium",
		Endpoint:    "ws://worker-1/playwright",
	}

	client.RecordSuccessfulSessionAndTriggerShutdownIfNeeded(ctx, serverInfo)

	cmdKey := fmt.Sprintf(workerShutdownCommandFmt, workerID)
	if exists, err := server.rd.Exists(ctx, cmdKey).Result(); err != nil {
		t.Fatalf("failed to check shutdown command after first success: %v", err)
	} else if exists != 0 {
		t.Fatalf("shutdown command should not be set before the committed limit is reached")
	}

	if committed, err := server.rd.HGet(ctx, successfulSessionsKey, workerID).Int64(); err != nil {
		t.Fatalf("failed to read committed successful sessions: %v", err)
	} else if committed != 2 {
		t.Fatalf("expected committed successful sessions to be 2, got %d", committed)
	}

	client.RecordSuccessfulSessionAndTriggerShutdownIfNeeded(ctx, serverInfo)

	if exists, err := server.rd.Exists(ctx, cmdKey).Result(); err != nil {
		t.Fatalf("failed to check shutdown command after second success: %v", err)
	} else if exists != 1 {
		t.Fatalf("expected shutdown command once committed limit was reached")
	}
}

func TestClient_ReapStaleWorkersRemovesAllocatedAndSuccessfulSessionState(t *testing.T) {
	server := startRedisTestServer(t)
	cfg := newSelectorTestConfig()
	client := newSelectorTestClient(t, cfg, server.rd)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	staleWorkerID := "chromium:stale"
	liveWorkerID := "chromium:live"

	if err := server.rd.HSet(ctx, activeConnectionsKey, liveWorkerID, 1).Err(); err != nil {
		t.Fatalf("failed to seed active state for live worker: %v", err)
	}
	if err := server.rd.HSet(ctx, allocatedSessionsKey, staleWorkerID, 2).Err(); err != nil {
		t.Fatalf("failed to seed allocated state for stale worker: %v", err)
	}
	if err := server.rd.HSet(ctx, successfulSessionsKey, staleWorkerID, 5).Err(); err != nil {
		t.Fatalf("failed to seed successful state for stale worker: %v", err)
	}
	if err := server.rd.HSet(ctx, successfulSessionsKey, liveWorkerID, 3).Err(); err != nil {
		t.Fatalf("failed to seed successful state for live worker: %v", err)
	}
	if err := server.rd.HSet(ctx, "worker:"+liveWorkerID, map[string]any{
		"id":            "live",
		"browserType":   "chromium",
		"endpoint":      "ws://live/playwright",
		"status":        "available",
		"startedAt":     time.Now().UnixMilli(),
		"lastHeartbeat": time.Now().UnixMilli(),
	}).Err(); err != nil {
		t.Fatalf("failed to seed live worker metadata: %v", err)
	}

	reaped, err := client.ReapStaleWorkers(ctx)
	if err != nil {
		t.Fatalf("unexpected reaper error: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("expected 1 stale worker to be reaped, got %d", reaped)
	}

	if exists, err := server.rd.HExists(ctx, allocatedSessionsKey, staleWorkerID).Result(); err != nil {
		t.Fatalf("failed to check allocated state for stale worker: %v", err)
	} else if exists {
		t.Fatal("expected stale worker allocated state to be removed")
	}
	if exists, err := server.rd.HExists(ctx, successfulSessionsKey, staleWorkerID).Result(); err != nil {
		t.Fatalf("failed to check successful state for stale worker: %v", err)
	} else if exists {
		t.Fatal("expected stale worker successful state to be removed")
	}
	if exists, err := server.rd.HExists(ctx, successfulSessionsKey, liveWorkerID).Result(); err != nil {
		t.Fatalf("failed to check successful state for live worker: %v", err)
	} else if !exists {
		t.Fatal("expected live worker successful state to remain")
	}
}
