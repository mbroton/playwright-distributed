package config

import (
	"fmt"
	"strconv"
	"time"
)

type Config struct {
	WorkerHeartbeatTTL  time.Duration
	SessionHeartbeatTTL time.Duration
	PendingSessionTTL   time.Duration
	StalledWorkerTTL    time.Duration
	RescuerInterval     time.Duration
	MaxQueueSize        int
	QueueWaitTimeout    time.Duration
	MaxLifetimeSessions int64
}

func Load(getenv func(string) string) (Config, error) {
	workerTTL, err := positiveDuration(getenv, "WORKER_HEARTBEAT_TTL", 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	sessionTTL, err := positiveDuration(getenv, "SESSION_HEARTBEAT_TTL", 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	pendingTTL, err := positiveDuration(getenv, "PENDING_SESSION_TTL", 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	stalledTTL, err := positiveDuration(getenv, "STALLED_WORKER_TTL", 10*time.Minute)
	if err != nil {
		return Config{}, err
	}
	rescuerInterval, err := positiveDuration(getenv, "RESCUER_INTERVAL", 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	queueWaitTimeout, err := positiveDuration(getenv, "QUEUE_WAIT_TIMEOUT", 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	maxQueueSize, err := nonNegativeInt(getenv, "MAX_QUEUE_SIZE", 100)
	if err != nil {
		return Config{}, err
	}
	maxLifetimeSessions, err := nonNegativeInt64(getenv, "MAX_LIFETIME_SESSIONS", 50)
	if err != nil {
		return Config{}, err
	}

	return Config{
		WorkerHeartbeatTTL:  workerTTL,
		SessionHeartbeatTTL: sessionTTL,
		PendingSessionTTL:   pendingTTL,
		StalledWorkerTTL:    stalledTTL,
		RescuerInterval:     rescuerInterval,
		MaxQueueSize:        maxQueueSize,
		QueueWaitTimeout:    queueWaitTimeout,
		MaxLifetimeSessions: maxLifetimeSessions,
	}, nil
}

func positiveDuration(getenv func(string) string, name string, defaultValue time.Duration) (time.Duration, error) {
	raw := getenv(name)
	if raw == "" {
		return defaultValue, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", name, err)
	}
	if value <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	if value.Microseconds() == 0 {
		return 0, fmt.Errorf("%s must be at least one microsecond", name)
	}
	return value, nil
}

func nonNegativeInt(getenv func(string) string, name string, defaultValue int) (int, error) {
	raw := getenv(name)
	if raw == "" {
		return defaultValue, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	if value < 0 {
		return 0, fmt.Errorf("%s must not be negative", name)
	}
	return value, nil
}

func nonNegativeInt64(getenv func(string) string, name string, defaultValue int64) (int64, error) {
	raw := getenv(name)
	if raw == "" {
		return defaultValue, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	if value < 0 {
		return 0, fmt.Errorf("%s must not be negative", name)
	}
	return value, nil
}
