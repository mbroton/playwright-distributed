package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	config, err := Load(func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load() returned an error: %v", err)
	}
	if config.WorkerHeartbeatTTL != 30*time.Second ||
		config.SessionHeartbeatTTL != 30*time.Second ||
		config.PendingSessionTTL != 30*time.Second ||
		config.StalledWorkerTTL != 10*time.Minute ||
		config.RescuerInterval != 5*time.Second ||
		config.MaxQueueSize != 100 ||
		config.QueueWaitTimeout != 30*time.Second ||
		config.MaxLifetimeSessions != 50 {
		t.Fatalf("Load() = %+v, want documented defaults", config)
	}
}

func TestLoad_Overrides(t *testing.T) {
	values := map[string]string{
		"WORKER_HEARTBEAT_TTL":  "10s",
		"SESSION_HEARTBEAT_TTL": "11s",
		"PENDING_SESSION_TTL":   "12s",
		"STALLED_WORKER_TTL":    "13m",
		"RESCUER_INTERVAL":      "2s",
		"MAX_QUEUE_SIZE":        "0",
		"QUEUE_WAIT_TIMEOUT":    "3s",
		"MAX_LIFETIME_SESSIONS": "0",
	}
	config, err := Load(func(name string) string { return values[name] })
	if err != nil {
		t.Fatalf("Load() returned an error: %v", err)
	}
	if config.WorkerHeartbeatTTL != 10*time.Second ||
		config.SessionHeartbeatTTL != 11*time.Second ||
		config.PendingSessionTTL != 12*time.Second ||
		config.StalledWorkerTTL != 13*time.Minute ||
		config.RescuerInterval != 2*time.Second ||
		config.MaxQueueSize != 0 ||
		config.QueueWaitTimeout != 3*time.Second ||
		config.MaxLifetimeSessions != 0 {
		t.Fatalf("Load() = %+v, want overrides", config)
	}
}

func TestLoad_RejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr string
	}{
		{name: "invalid duration", key: "WORKER_HEARTBEAT_TTL", value: "soon", wantErr: "must be a duration"},
		{name: "zero duration", key: "SESSION_HEARTBEAT_TTL", value: "0s", wantErr: "must be positive"},
		{name: "sub-microsecond duration", key: "PENDING_SESSION_TTL", value: "1ns", wantErr: "at least one microsecond"},
		{name: "negative queue", key: "MAX_QUEUE_SIZE", value: "-1", wantErr: "must not be negative"},
		{name: "negative lifetime", key: "MAX_LIFETIME_SESSIONS", value: "-1", wantErr: "must not be negative"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(func(name string) string {
				if name == test.key {
					return test.value
				}
				return ""
			})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Load() error = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}
