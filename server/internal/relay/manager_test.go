package relay

import (
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func TestManager_RegisterDistinguishesDrainAndDuplicate(t *testing.T) {
	manager := NewManager(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{})
	sessionID := uuid.New()
	events := make(chan relayEvent, 1)
	stop := func(event relayEvent) { events <- event }
	if got := manager.register(sessionID, stop); got != registerAccepted {
		t.Fatalf("first register result = %d, want accepted", got)
	}
	t.Cleanup(func() { manager.unregister(sessionID) })
	if got := manager.register(sessionID, func(relayEvent) {}); got != registerDuplicate {
		t.Fatalf("duplicate register result = %d, want duplicate", got)
	}

	manager.live[sessionID](relayEvent{reason: "owner"})
	if event := <-events; event.reason != "owner" {
		t.Fatalf("registered owner event reason = %q, want owner", event.reason)
	}
	manager.draining = true
	if got := manager.register(uuid.New(), func(relayEvent) {}); got != registerDraining {
		t.Fatalf("draining register result = %d, want draining", got)
	}
}

func TestErrorEvent_SanitizesNonTransmittableCloseCodes(t *testing.T) {
	tests := []struct {
		name       string
		code       int
		wantCode   int
		wantReason string
		wantNormal bool
	}{
		{
			name:       "normal close",
			code:       websocket.CloseNormalClosure,
			wantCode:   websocket.CloseNormalClosure,
			wantReason: "peer reason",
			wantNormal: true,
		},
		{
			name:       "no status uses empty close payload",
			code:       websocket.CloseNoStatusReceived,
			wantCode:   websocket.CloseNoStatusReceived,
			wantReason: "peer reason",
			wantNormal: true,
		},
		{
			name:       "application close",
			code:       4008,
			wantCode:   4008,
			wantReason: "peer reason",
		},
		{
			name:       "abnormal close",
			code:       websocket.CloseAbnormalClosure,
			wantCode:   websocket.CloseInternalServerErr,
			wantReason: "peer connection lost",
		},
		{
			name:       "TLS close",
			code:       websocket.CloseTLSHandshake,
			wantCode:   websocket.CloseInternalServerErr,
			wantReason: "peer connection lost",
		},
		{
			name:       "unregistered close",
			code:       2000,
			wantCode:   websocket.CloseInternalServerErr,
			wantReason: "peer connection lost",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := errorEvent(&websocket.CloseError{Code: test.code, Text: "peer reason"})
			if event.code != test.wantCode || event.reason != test.wantReason || event.normal != test.wantNormal {
				t.Fatalf(
					"errorEvent() = code %d, reason %q, normal %t; want %d, %q, %t",
					event.code,
					event.reason,
					event.normal,
					test.wantCode,
					test.wantReason,
					test.wantNormal,
				)
			}
		})
	}
}
