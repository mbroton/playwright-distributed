package config

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func resetViper(t *testing.T) {
	t.Helper()

	viper.Reset()
	t.Cleanup(viper.Reset)
}

func TestLoadConfig_UsesProxyTimeouts(t *testing.T) {
	resetViper(t)

	t.Setenv("REDIS_HOST", "redis")
	t.Setenv("REDIS_PORT", "6379")
	t.Setenv("PROXY_READ_HEADER_TIMEOUT", "3")
	t.Setenv("PROXY_WORKER_SELECTION_TIMEOUT", "7")
	t.Setenv("PROXY_CONNECT_TIMEOUT", "7")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.ProxyReadHeaderTimeout != 3 {
		t.Fatalf("expected PROXY_READ_HEADER_TIMEOUT 3, got %d", cfg.ProxyReadHeaderTimeout)
	}
	if cfg.ProxyWorkerSelectionTimeout != 7 {
		t.Fatalf("expected PROXY_WORKER_SELECTION_TIMEOUT 7, got %d", cfg.ProxyWorkerSelectionTimeout)
	}
	if cfg.ProxyConnectTimeout != 7 {
		t.Fatalf("expected PROXY_CONNECT_TIMEOUT 7, got %d", cfg.ProxyConnectTimeout)
	}
}

func TestLoadConfig_UsesTimeoutDefaults(t *testing.T) {
	resetViper(t)

	t.Setenv("REDIS_HOST", "redis")
	t.Setenv("REDIS_PORT", "6379")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.ProxyReadHeaderTimeout != 5 {
		t.Fatalf("expected default PROXY_READ_HEADER_TIMEOUT 5, got %d", cfg.ProxyReadHeaderTimeout)
	}
	if cfg.ProxyWorkerSelectionTimeout != 5 {
		t.Fatalf("expected default PROXY_WORKER_SELECTION_TIMEOUT 5, got %d", cfg.ProxyWorkerSelectionTimeout)
	}
	if cfg.ProxyConnectTimeout != 5 {
		t.Fatalf("expected default PROXY_CONNECT_TIMEOUT 5, got %d", cfg.ProxyConnectTimeout)
	}
}

func TestLoadConfig_RejectsNonPositiveTimeouts(t *testing.T) {
	tests := []struct {
		name    string
		envName string
		want    string
	}{
		{
			name:    "read header timeout",
			envName: "PROXY_READ_HEADER_TIMEOUT",
			want:    "PROXY_READ_HEADER_TIMEOUT must be greater than 0",
		},
		{
			name:    "worker selection timeout",
			envName: "PROXY_WORKER_SELECTION_TIMEOUT",
			want:    "PROXY_WORKER_SELECTION_TIMEOUT must be greater than 0",
		},
		{
			name:    "connect timeout",
			envName: "PROXY_CONNECT_TIMEOUT",
			want:    "PROXY_CONNECT_TIMEOUT must be greater than 0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resetViper(t)

			t.Setenv("REDIS_HOST", "redis")
			t.Setenv("REDIS_PORT", "6379")
			t.Setenv(tc.envName, "0")

			_, err := LoadConfig()
			if err == nil {
				t.Fatal("expected error")
			}

			if err.Error() != tc.want {
				t.Fatalf("expected error %q, got %q", tc.want, err.Error())
			}
		})
	}
}

func TestLoadConfig_RejectsUnsupportedDefaultBrowserType(t *testing.T) {
	resetViper(t)

	t.Setenv("REDIS_HOST", "redis")
	t.Setenv("REDIS_PORT", "6379")
	t.Setenv("DEFAULT_BROWSER_TYPE", "opera")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error")
	}

	if !strings.Contains(err.Error(), "DEFAULT_BROWSER_TYPE must be one of: chromium, firefox, webkit") {
		t.Fatalf("unexpected error: %v", err)
	}
}
