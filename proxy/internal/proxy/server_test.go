package proxy

import (
	"net/http"
	"testing"
	"time"
)

func TestNewHTTPServer_DerivesHTTPTimeoutsFromProxyConfig(t *testing.T) {
	cfg := newTestConfig()
	cfg.ProxyReadHeaderTimeout = 4
	cfg.ProxyWorkerSelectionTimeout = 7
	cfg.ProxyConnectTimeout = 3

	server := newHTTPServer(cfg, http.NewServeMux())

	if server.ReadHeaderTimeout != 4*time.Second {
		t.Fatalf("expected ReadHeaderTimeout 4s, got %v", server.ReadHeaderTimeout)
	}
	if server.WriteTimeout != 8*time.Second {
		t.Fatalf("expected WriteTimeout 8s, got %v", server.WriteTimeout)
	}
	if server.IdleTimeout != httpIdleTimeout {
		t.Fatalf("expected IdleTimeout %v, got %v", httpIdleTimeout, server.IdleTimeout)
	}
}
