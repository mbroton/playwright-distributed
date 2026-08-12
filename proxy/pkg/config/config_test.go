package config

import (
	"strings"
	"testing"
)

func validConfig() *Config {
	return &Config{
		RedisHost:             "redis",
		RedisPort:             6379,
		MaxConcurrentSessions: 5,
		MaxLifetimeSessions:   50,
		ReaperRunInterval:     300,
		ShutdownCommandTTL:    60,
		WorkerSelectTimeout:   5,
		DefaultBrowserType:    "chromium",
	}
}

func TestValidateAcceptsValidConfig(t *testing.T) {
	if err := validate(validConfig()); err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
}

func TestValidateRejectsInvalidTimingAndSessionValues(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Config)
		expected  string
	}{
		{
			name: "max concurrent sessions",
			configure: func(cfg *Config) {
				cfg.MaxConcurrentSessions = 0
			},
			expected: "MAX_CONCURRENT_SESSIONS must be greater than zero",
		},
		{
			name: "max lifetime sessions",
			configure: func(cfg *Config) {
				cfg.MaxLifetimeSessions = 0
			},
			expected: "MAX_LIFETIME_SESSIONS must be greater than zero",
		},
		{
			name: "reaper interval",
			configure: func(cfg *Config) {
				cfg.ReaperRunInterval = 0
			},
			expected: "REAPER_RUN_INTERVAL must be greater than zero",
		},
		{
			name: "shutdown command TTL",
			configure: func(cfg *Config) {
				cfg.ShutdownCommandTTL = 0
			},
			expected: "SHUTDOWN_COMMAND_TTL must be greater than zero",
		},
		{
			name: "worker select timeout",
			configure: func(cfg *Config) {
				cfg.WorkerSelectTimeout = 0
			},
			expected: "WORKER_SELECT_TIMEOUT must be greater than zero",
		},
		{
			name: "worker select timeout reaches HTTP write timeout",
			configure: func(cfg *Config) {
				cfg.WorkerSelectTimeout = 15
			},
			expected: "WORKER_SELECT_TIMEOUT must be less than the HTTP write timeout",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig()
			test.configure(cfg)

			err := validate(cfg)
			if err == nil {
				t.Fatalf("expected error containing %q", test.expected)
			}
			if !strings.Contains(err.Error(), test.expected) {
				t.Fatalf("expected error containing %q, got %q", test.expected, err)
			}
		})
	}
}
